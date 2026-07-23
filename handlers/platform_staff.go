package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
)

// PlatformStaffIndex lists every Super Admin (SA) platform account.
func PlatformStaffIndex(c *fiber.Ctx) error {
	var staff []models.PlatformUser
	config.DB.Order("created_at ASC").Find(&staff)
	return utils.SuccessResponse(c, staff, "")
}

// PlatformStaffStore creates a new SA platform account.
func PlatformStaffStore(c *fiber.Ctx) error {
	var body struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&body); err != nil || body.Email == "" || body.Password == "" {
		return utils.ErrorResponse(c, "name, email, and password are required.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
	}

	user := models.PlatformUser{
		Name:     body.Name,
		Email:    body.Email,
		Password: string(hash),
		Status:   "active",
	}
	if err := config.DB.Create(&user).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create platform user (email may already be taken).", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, user, "Platform user created successfully.", fiber.StatusCreated)
}

// PlatformStaffUpdate edits an SA account's name/status, or resets its
// password if one is supplied.
func PlatformStaffUpdate(c *fiber.Ctx) error {
	var user models.PlatformUser
	if err := config.DB.First(&user, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Platform user not found.", "", fiber.StatusNotFound)
	}

	var body struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	updates := map[string]interface{}{}
	if body.Name != "" {
		updates["name"] = body.Name
	}
	if body.Status != "" {
		updates["status"] = body.Status
	}
	if body.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
		if err != nil {
			return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
		}
		updates["password"] = string(hash)
	}

	if err := config.DB.Model(&user).Updates(updates).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	config.DB.First(&user, user.ID)
	return utils.SuccessResponse(c, user, "Platform user updated successfully.")
}

// PlatformStaffDestroy removes an SA account's access. An SA cannot delete
// their own account (avoids a support session locking everyone out).
func PlatformStaffDestroy(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var user models.PlatformUser
	if err := config.DB.First(&user, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Platform user not found.", "", fiber.StatusNotFound)
	}
	if user.ID == claims.PlatformUserID {
		return utils.ErrorResponse(c, "You cannot delete your own account.", "", fiber.StatusBadRequest)
	}
	config.DB.Delete(&user)
	return utils.SuccessResponse(c, nil, "Platform user removed successfully.")
}
