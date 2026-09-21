package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
)

// validUserRoles are the staff roles the RBAC middleware knows about.
var validUserRoles = map[string]bool{"super_admin": true, "zone_manager": true, "finance": true, "field_agent": true}

// zoneInOrg reports whether zoneID is a zone of the organization. A staff
// member's zone_id scopes what they can see, so accepting one from another
// ISP would hand them that ISP's zone.
func zoneInOrg(zoneID, orgID uint) bool {
	var n int64
	config.DB.Model(&models.Zone{}).Where("id = ? AND organization_id = ?", zoneID, orgID).Count(&n)
	return n > 0
}

// orgLoginURL is where staff of an organization sign in, "" if it has no subdomain.
func orgLoginURL(orgID uint) string {
	var org models.Organization
	if err := config.DB.Select("id", "subdomain").First(&org, orgID).Error; err != nil {
		return ""
	}
	return middleware.AdminURL(org.Subdomain)
}

// UserIndex lists all users (super_admin only).
func UserIndex(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	page, perPage := utils.ParsePage(c)
	var users []models.User
	var total int64

	query := config.DB.Model(&models.User{}).Preload("Zone").Where("organization_id = ?", claims.OrganizationID)
	if r := c.Query("role"); r != "" {
		query = query.Where("role = ?", r)
	}
	if s := c.Query("search"); s != "" {
		query = query.Where("name LIKE ? OR email LIKE ? OR phone LIKE ?",
			"%"+s+"%", "%"+s+"%", "%"+s+"%")
	}
	if c.Query("all") != "" {
		query.Find(&users)
		return utils.SuccessResponse(c, users, "")
	}

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&users)
	return utils.PaginatedResponse(c, users, total, page, perPage)
}

// UserStore creates a new admin user (super_admin only).
func UserStore(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var body struct {
		Name     string  `json:"name"`
		Email    string  `json:"email"`
		Password string  `json:"password"`
		Phone    *string `json:"phone"`
		Role     string  `json:"role"`
		ZoneID   *uint   `json:"zone_id"`
		Status   string  `json:"status"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	if body.Name == "" || body.Email == "" || body.Password == "" {
		return utils.ErrorResponse(c, "Name, email, and password are required.", "Validation failed.", fiber.StatusBadRequest)
	}

	// Check for existing account with the same email (including soft-deleted)
	var existingUser models.User
	if err := config.DB.Unscoped().Where("email = ?", body.Email).First(&existingUser).Error; err == nil {
		if existingUser.DeletedAt.Valid {
			// Permanently purge previous soft-deleted record so the email can be re-registered
			config.DB.Unscoped().Delete(&existingUser)
		} else {
			return utils.ErrorResponse(c, "An account with this email address already exists.", "Email already in use.", fiber.StatusConflict)
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
	}

	if body.Status == "" {
		body.Status = "active"
	}
	if body.Role == "" {
		body.Role = "field_agent"
	}
	if !validUserRoles[body.Role] {
		return utils.ErrorResponse(c, "Unknown role.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}

	orgID := claims.OrganizationID
	if orgID == 0 {
		var adminUser models.User
		if err := config.DB.First(&adminUser, claims.UserID).Error; err == nil && adminUser.OrganizationID != 0 {
			orgID = adminUser.OrganizationID
		} else {
			var defaultOrg models.Organization
			if err := config.DB.Where("slug = ?", "default").First(&defaultOrg).Error; err == nil {
				orgID = defaultOrg.ID
			} else {
				orgID = 1
			}
		}
	}

	if body.ZoneID != nil && !zoneInOrg(*body.ZoneID, orgID) {
		return utils.ErrorResponse(c, "That zone does not belong to your organization.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}

	user := models.User{
		Name:           body.Name,
		Email:          body.Email,
		Password:       string(hash),
		Phone:          body.Phone,
		Role:           body.Role,
		ZoneID:         body.ZoneID,
		OrganizationID: orgID,
		Status:         body.Status,
	}

	if err := config.DB.Create(&user).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create user.", fiber.StatusInternalServerError)
	}
	user.LoginURL = orgLoginURL(orgID)
	return utils.SuccessResponse(c, user, "User created successfully.", fiber.StatusCreated)
}

// UserShow returns a single user.
func UserShow(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}
	var user models.User
	if err := config.DB.Preload("Zone").Where("organization_id = ?", claims.OrganizationID).First(&user, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, user, "")
}

// UserUpdate updates an admin user (super_admin only).
func UserUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var user models.User
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&user, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "", fiber.StatusNotFound)
	}

	var body map[string]interface{}
	c.BodyParser(&body)
	delete(body, "organization_id") // never allow reassigning a user's tenant via this endpoint
	delete(body, "login_url")

	if role, ok := body["role"].(string); ok && !validUserRoles[role] {
		return utils.ErrorResponse(c, "Unknown role.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if rawZone, present := body["zone_id"]; present && rawZone != nil {
		zoneFloat, isNum := rawZone.(float64)
		if !isNum || !zoneInOrg(uint(zoneFloat), claims.OrganizationID) {
			return utils.ErrorResponse(c, "That zone does not belong to your organization.", "Validation failed.", fiber.StatusUnprocessableEntity)
		}
	}

	if newEmail, ok := body["email"].(string); ok && newEmail != "" && newEmail != user.Email {
		var existing models.User
		if err := config.DB.Unscoped().Where("email = ? AND id != ?", newEmail, user.ID).First(&existing).Error; err == nil {
			if existing.DeletedAt.Valid {
				config.DB.Unscoped().Delete(&existing)
			} else {
				return utils.ErrorResponse(c, "An account with this email address already exists.", "Email already in use.", fiber.StatusConflict)
			}
		}
	}

	if pw, ok := body["password"].(string); ok && pw != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
		}
		body["password"] = string(hash)
	} else {
		delete(body, "password")
	}

	if err := config.DB.Model(&user).Updates(body).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zone").First(&user, user.ID)
	return utils.SuccessResponse(c, user, "User updated successfully.")
}

// UserDestroy deletes a user (super_admin only, cannot delete self).
func UserDestroy(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var user models.User
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&user, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "", fiber.StatusNotFound)
	}
	if user.ID == claims.UserID {
		return utils.ErrorResponse(c, "You cannot delete your own account.", "", fiber.StatusBadRequest)
	}

	config.DB.Delete(&user)
	return utils.SuccessResponse(c, nil, "User deleted successfully.")
}
