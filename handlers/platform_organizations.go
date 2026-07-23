package handlers

import (
	"sort"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
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

func organizationSummary(org models.Organization) OrganizationSummary {
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

// OrganizationStore onboards a new ISP tenant: creates the Organization and
// its first super_admin User in one step.
func OrganizationStore(c *fiber.Ctx) error {
	var body struct {
		Name          string `json:"name"`
		Slug          string `json:"slug"`
		ContactEmail  string `json:"contact_email"`
		ContactPhone  string `json:"contact_phone"`
		AdminName     string `json:"admin_name"`
		AdminEmail    string `json:"admin_email"`
		AdminPassword string `json:"admin_password"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.Name == "" || body.Slug == "" || body.AdminEmail == "" || body.AdminPassword == "" {
		return utils.ErrorResponse(c, "name, slug, admin_email, and admin_password are required.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}

	org := models.Organization{
		Name:         body.Name,
		Slug:         body.Slug,
		ContactEmail: body.ContactEmail,
		Status:       "active",
	}
	if body.ContactPhone != "" {
		org.ContactPhone = &body.ContactPhone
	}

	if err := config.DB.Create(&org).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create organization (slug may already be taken).", fiber.StatusInternalServerError)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
	}

	adminName := body.AdminName
	if adminName == "" {
		adminName = body.Name + " Admin"
	}
	admin := models.User{
		Name:           adminName,
		Email:          body.AdminEmail,
		Password:       string(hash),
		Role:           "super_admin",
		Status:         "active",
		OrganizationID: org.ID,
	}
	if err := config.DB.Create(&admin).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Organization created, but failed to create its admin user.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"organization": org,
		"admin_user": fiber.Map{
			"id":    admin.ID,
			"name":  admin.Name,
			"email": admin.Email,
		},
	}, "Organization onboarded successfully.", fiber.StatusCreated)
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

	if err := config.DB.Model(&org).Updates(body).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
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
