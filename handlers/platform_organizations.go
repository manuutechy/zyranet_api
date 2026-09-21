package handlers

import (
	"log"
	"net/mail"
	"regexp"
	"sort"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// OrganizationSummary is an Organization enriched with the cross-tenant
// health/usage figures the Super Admin dashboard needs — zone counts,
// router online/offline split, active customer count, and lifetime revenue.
type OrganizationSummary struct {
	models.Organization
	ZoneCount       int64   `json:"zone_count"`
	ZonesOnline     int64   `json:"zones_online"`
	ZonesOffline    int64   `json:"zones_offline"`
	ActiveCustomers int64   `json:"active_customers"`
	TotalRevenue    float64 `json:"total_revenue"`
}

// subdomainTaken reports whether another organization (including a soft-deleted
// one — the unique index still covers it) already owns subdomain.
func subdomainTaken(subdomain string, excludeOrgID uint) bool {
	var n int64
	config.DB.Unscoped().Model(&models.Organization{}).
		Where("subdomain = ? AND id != ?", subdomain, excludeOrgID).Count(&n)
	return n > 0
}

func organizationSummary(org models.Organization) OrganizationSummary {
	org.AdminURL = middleware.AdminURL(org.Subdomain)
	s := OrganizationSummary{Organization: org}

	var zoneIDs []uint
	config.DB.Model(&models.Zone{}).Where("organization_id = ?", org.ID).Pluck("id", &zoneIDs)

	s.ZoneCount = int64(len(zoneIDs))
	if s.ZoneCount == 0 {
		return s
	}

	config.DB.Model(&models.Zone{}).Where("organization_id = ? AND last_status = ?", org.ID, "online").Count(&s.ZonesOnline)
	config.DB.Model(&models.Zone{}).Where("organization_id = ? AND last_status = ?", org.ID, "offline").Count(&s.ZonesOffline)
	config.DB.Model(&models.Customer{}).Where("zone_id IN (?) AND status = ?", zoneIDs, "active").Count(&s.ActiveCustomers)
	config.DB.Model(&models.Payment{}).Where("zone_id IN (?) AND status = ?", zoneIDs, "completed").
		Select("COALESCE(SUM(amount), 0)").Scan(&s.TotalRevenue)

	return s
}

// OrganizationIndex lists every ISP tenant with a health/usage summary.
// Supports ?search= (name/slug), ?status=, and ?sort=name|revenue|customers|created_at
// with ?order=asc|desc (default desc).
func OrganizationIndex(c *fiber.Ctx) error {
	query := config.DB.Model(&models.Organization{})
	if search := c.Query("search"); search != "" {
		query = query.Where("name LIKE ? OR slug LIKE ?", "%"+search+"%", "%"+search+"%")
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	var orgs []models.Organization
	if err := query.Order("created_at DESC").Find(&orgs).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to load organizations.", fiber.StatusInternalServerError)
	}

	summaries := make([]OrganizationSummary, 0, len(orgs))
	for _, org := range orgs {
		summaries = append(summaries, organizationSummary(org))
	}

	sortBy := c.Query("sort", "created_at")
	ascending := c.Query("order", "desc") == "asc"
	less := func(i, j int) bool {
		switch sortBy {
		case "name":
			return summaries[i].Name < summaries[j].Name
		case "revenue":
			return summaries[i].TotalRevenue < summaries[j].TotalRevenue
		case "customers":
			return summaries[i].ActiveCustomers < summaries[j].ActiveCustomers
		default:
			return summaries[i].CreatedAt.Before(summaries[j].CreatedAt)
		}
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if ascending {
			return less(i, j)
		}
		return less(j, i)
	})

	return utils.SuccessResponse(c, summaries, "")
}

// OrganizationShow returns a single tenant's summary plus its zones.
func OrganizationShow(c *fiber.Ctx) error {
	var org models.Organization
	if err := config.DB.First(&org, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusNotFound)
	}

	var zones []models.Zone
	config.DB.Where("organization_id = ?", org.ID).Order("name ASC").Find(&zones)

	var alerts []models.ZoneAlert
	zoneIDs := make([]uint, 0, len(zones))
	for _, z := range zones {
		zoneIDs = append(zoneIDs, z.ID)
	}
	if len(zoneIDs) > 0 {
		config.DB.Preload("Zone").Where("zone_id IN (?) AND resolved_at IS NULL", zoneIDs).
			Order("created_at DESC").Find(&alerts)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"organization": organizationSummary(org),
		"zones":        zones,
		"alerts":       alerts,
	}, "")
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// slugify turns an ISP name into a URL-safe slug ("Coastline Networks" ->
// "coastline-networks").
func slugify(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 100 {
		out = strings.Trim(out[:100], "-")
	}
	return out
}

// onboardingError is a validation failure that names the offending form field
// so the platform UI can point at it. error and message both carry the text so
// any client that shows either one shows something useful.
func onboardingError(c *fiber.Ctx, field, msg string, status int) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "error": msg, "message": msg, "field": field})
}

// OrganizationStore onboards a new ISP tenant: creates the Organization and
// its first super_admin User in one atomic step, so a failure (e.g. the admin
// email is already used) can't leave behind an ISP with nobody able to log in.
//
// slug defaults to a slugified name, subdomain is optional, and a blank
// admin_password generates a strong temporary one, returned once as
// temp_password for the operator to hand to the ISP.
func OrganizationStore(c *fiber.Ctx) error {
	var body struct {
		Name          string `json:"name"`
		Slug          string `json:"slug"`
		Subdomain     string `json:"subdomain"`
		ContactEmail  string `json:"contact_email"`
		ContactPhone  string `json:"contact_phone"`
		AdminName     string `json:"admin_name"`
		AdminEmail    string `json:"admin_email"`
		AdminPassword string `json:"admin_password"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		return onboardingError(c, "name", "ISP name is required.", fiber.StatusUnprocessableEntity)
	}
	adminEmail := strings.ToLower(strings.TrimSpace(body.AdminEmail))
	if adminEmail == "" {
		return onboardingError(c, "admin_email", "Admin email is required.", fiber.StatusUnprocessableEntity)
	}
	if _, err := mail.ParseAddress(adminEmail); err != nil {
		return onboardingError(c, "admin_email", "Enter a valid admin email address.", fiber.StatusUnprocessableEntity)
	}

	slug := strings.ToLower(strings.TrimSpace(body.Slug))
	if slug == "" {
		slug = slugify(name)
	}
	if !slugRe.MatchString(slug) {
		return onboardingError(c, "slug", "Slug may only contain lowercase letters, numbers and single hyphens.", fiber.StatusUnprocessableEntity)
	}
	var slugCount int64
	config.DB.Unscoped().Model(&models.Organization{}).Where("slug = ?", slug).Count(&slugCount)
	if slugCount > 0 {
		return onboardingError(c, "slug", "That slug is already used by another ISP.", fiber.StatusConflict)
	}

	subdomain, err := validateSubdomain(body.Subdomain)
	if err != nil {
		return onboardingError(c, "subdomain", err.Error(), fiber.StatusUnprocessableEntity)
	}
	if subdomain != "" && subdomainTaken(subdomain, 0) {
		return onboardingError(c, "subdomain", "That subdomain is already taken.", fiber.StatusConflict)
	}

	password := body.AdminPassword
	generated := false
	if password == "" {
		password = randomHex(6)
		generated = true
	} else if len(password) < 8 {
		return onboardingError(c, "admin_password", "Password must be at least 8 characters, or leave it blank to generate one.", fiber.StatusUnprocessableEntity)
	}

	// An email that only belongs to a soft-deleted user is free to reuse (same
	// rule as UserStore); a live one is not.
	var existingUser models.User
	emailOwned := config.DB.Unscoped().Where("LOWER(email) = ?", adminEmail).First(&existingUser).Error == nil
	if emailOwned && !existingUser.DeletedAt.Valid {
		return onboardingError(c, "admin_email", "An account with this email already exists.", fiber.StatusConflict)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
	}

	org := models.Organization{Name: name, Slug: slug, ContactEmail: strings.TrimSpace(body.ContactEmail), Status: "active"}
	if subdomain != "" {
		org.Subdomain = &subdomain
	}
	if phone := strings.TrimSpace(body.ContactPhone); phone != "" {
		org.ContactPhone = &phone
	}
	adminName := strings.TrimSpace(body.AdminName)
	if adminName == "" {
		adminName = name + " Admin"
	}
	admin := models.User{Name: adminName, Email: adminEmail, Password: string(hash), Role: "super_admin", Status: "active"}

	txErr := config.DB.Transaction(func(tx *gorm.DB) error {
		if emailOwned {
			if err := tx.Unscoped().Delete(&existingUser).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&org).Error; err != nil {
			return err
		}
		admin.OrganizationID = org.ID
		return tx.Create(&admin).Error
	})
	if txErr != nil {
		log.Printf("[onboarding] failed to create organization %q: %v", slug, txErr)
		// Most likely a unique-index race with a concurrent onboarding.
		return onboardingError(c, "", "Could not create the ISP — the slug, subdomain or admin email may have just been taken. Please try again.", fiber.StatusConflict)
	}

	middleware.InvalidateTenantCache()
	org.AdminURL = middleware.AdminURL(org.Subdomain)
	loginURL := org.AdminURL
	if loginURL == "" {
		loginURL = "https://admin." + config.Config.BaseDomain
	}

	result := fiber.Map{
		"organization": org,
		"admin_user": fiber.Map{
			"id":        admin.ID,
			"name":      admin.Name,
			"email":     admin.Email,
			"login_url": loginURL,
		},
		"login_url": loginURL,
	}
	if generated {
		result["temp_password"] = password
	}
	return utils.SuccessResponse(c, result, "Organization onboarded successfully.", fiber.StatusCreated)
}

// OrganizationUpdate edits a tenant's contact info or suspends/activates it.
func OrganizationUpdate(c *fiber.Ctx) error {
	var org models.Organization
	if err := config.DB.First(&org, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusNotFound)
	}

	var body map[string]interface{}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	delete(body, "id")
	delete(body, "slug") // slug is used for routing (e.g. future per-org M-Pesa callbacks) — immutable after creation
	delete(body, "admin_url")

	if raw, present := body["commission_percent"]; present && raw != nil {
		pct, isNum := raw.(float64)
		if !isNum || pct < 0 || pct > 100 {
			return utils.ErrorResponse(c, "commission_percent must be a number from 0 to 100, or blank to use the platform default.", "Validation failed.", fiber.StatusUnprocessableEntity)
		}
	}

	// Subdomain is validated and written on its own: it must be a valid,
	// unreserved, unique DNS label, and "" clears it back to NULL (a bare ""
	// would collide under the unique index with every other cleared ISP).
	if raw, present := body["subdomain"]; present {
		delete(body, "subdomain")
		text, isString := raw.(string)
		if raw != nil && !isString {
			return utils.ErrorResponse(c, "subdomain must be text.", "Validation failed.", fiber.StatusUnprocessableEntity)
		}
		subdomain, err := validateSubdomain(text)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Validation failed.", fiber.StatusUnprocessableEntity)
		}
		if subdomain != "" && subdomainTaken(subdomain, org.ID) {
			return utils.ErrorResponse(c, "That subdomain is already taken.", "Validation failed.", fiber.StatusConflict)
		}
		var value interface{}
		if subdomain != "" {
			value = subdomain
		}
		if err := config.DB.Model(&org).Update("subdomain", value).Error; err != nil {
			return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
		}
	}

	if len(body) > 0 {
		if err := config.DB.Model(&org).Updates(body).Error; err != nil {
			return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
		}
	}
	middleware.InvalidateTenantCache()

	config.DB.First(&org, org.ID)
	org.AdminURL = middleware.AdminURL(org.Subdomain)
	return utils.SuccessResponse(c, org, "Organization updated successfully.")
}

// OrganizationUsers lists the admin/staff users belonging to a tenant, so
// the SA can pick which one to reset a password for during support.
func OrganizationUsers(c *fiber.Ctx) error {
	var org models.Organization
	if err := config.DB.First(&org, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusNotFound)
	}
	var users []models.User
	config.DB.Where("organization_id = ?", org.ID).Order("created_at ASC").Find(&users)
	return utils.SuccessResponse(c, users, "")
}

// OrganizationResetUserPassword lets an SA reset a tenant's admin/staff
// password for support purposes (e.g. the ISP is locked out). The new
// temporary password is returned once in the response — the SA is
// responsible for relaying it to the ISP out-of-band.
func OrganizationResetUserPassword(c *fiber.Ctx) error {
	var org models.Organization
	if err := config.DB.First(&org, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusNotFound)
	}

	var user models.User
	if err := config.DB.Where("organization_id = ?", org.ID).First(&user, c.Params("userId")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found for this organization.", "", fiber.StatusNotFound)
	}

	tempPassword := randomHex(6)
	hash, err := bcrypt.GenerateFromPassword([]byte(tempPassword), bcrypt.DefaultCost)
	if err != nil {
		return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
	}
	if err := config.DB.Model(&user).Update("password", string(hash)).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to reset password.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"user_id":       user.ID,
		"email":         user.Email,
		"temp_password": tempPassword,
	}, "Password reset. Share this temporary password with the ISP directly — it will not be shown again.")
}

// OrganizationSubdomainCheck tells the platform UI whether a subdomain can be
// given to an ISP, so it can be validated as the operator types.
// GET /platform/subdomains/check?subdomain=acme[&exclude_org_id=3]
func OrganizationSubdomainCheck(c *fiber.Ctx) error {
	subdomain, err := validateSubdomain(c.Query("subdomain"))
	if err != nil {
		return utils.SuccessResponse(c, fiber.Map{"available": false, "reason": err.Error()}, "")
	}
	if subdomain == "" {
		return utils.SuccessResponse(c, fiber.Map{"available": false, "reason": "enter a subdomain"}, "")
	}
	exclude := uint(c.QueryInt("exclude_org_id", 0))
	if subdomainTaken(subdomain, exclude) {
		return utils.SuccessResponse(c, fiber.Map{"available": false, "subdomain": subdomain, "reason": "already taken"}, "")
	}
	return utils.SuccessResponse(c, fiber.Map{
		"available": true,
		"subdomain": subdomain,
		"url":       middleware.AdminURL(&subdomain),
	}, "")
}
