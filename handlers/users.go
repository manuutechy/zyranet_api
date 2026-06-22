package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
)

// UserIndex lists all users (super_admin only).
func UserIndex(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	page, perPage := utils.ParsePage(c)
	var users []models.User
	var total int64

	query := config.DB.Model(&models.User{}).Preload("Zone")
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

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
	}

	if body.Status == "" {
		body.Status = "active"
	}

	user := models.User{
		Name:     body.Name,
		Email:    body.Email,
		Password: string(hash),
		Phone:    body.Phone,
		Role:     body.Role,
		ZoneID:   body.ZoneID,
		Status:   body.Status,
	}

	if err := config.DB.Create(&user).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create user.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, user, "User created successfully.", fiber.StatusCreated)
}

// UserShow returns a single user.
func UserShow(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}
	var user models.User
	if err := config.DB.Preload("Zone").First(&user, c.Params("id")).Error; err != nil {
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
	if err := config.DB.First(&user, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "", fiber.StatusNotFound)
	}

	var body map[string]interface{}
	c.BodyParser(&body)

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
	if err := config.DB.First(&user, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "", fiber.StatusNotFound)
	}
	if user.ID == claims.UserID {
		return utils.ErrorResponse(c, "You cannot delete your own account.", "", fiber.StatusBadRequest)
	}

	config.DB.Delete(&user)
	return utils.SuccessResponse(c, nil, "User deleted successfully.")
}
