package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
)

// Login authenticates an admin user and returns a JWT.
func Login(c *fiber.Ctx) error {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "Please provide email and password.", fiber.StatusBadRequest)
	}
	if body.Email == "" || body.Password == "" {
		return utils.ErrorResponse(c, "Email and password are required.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}

	var user models.User
	if err := config.DB.Where("email = ?", body.Email).First(&user).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid login credentials.", "Authentication failed.", fiber.StatusUnauthorized)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(body.Password)); err != nil {
		return utils.ErrorResponse(c, "Invalid login credentials.", "Authentication failed.", fiber.StatusUnauthorized)
	}

	if user.Status != "active" {
		return utils.ErrorResponse(c, "Your account is inactive.", "Account disabled.", fiber.StatusForbidden)
	}

	token, err := middleware.GenerateAdminToken(user.ID, user.Role, user.ZoneID)
	if err != nil {
		return utils.ErrorResponse(c, "Token generation failed.", "Server error.", fiber.StatusInternalServerError)
	}

	middleware.SetAuthCookie(c, middleware.AdminCookieName, token)

	return utils.SuccessResponse(c, fiber.Map{
		"token": token,
		"user": fiber.Map{
			"id":      user.ID,
			"name":    user.Name,
			"email":   user.Email,
			"role":    user.Role,
			"zone_id": user.ZoneID,
		},
	}, "Login successful.")
}

// Logout clears the admin session cookie (stateless JWT — nothing server-side to invalidate).
func Logout(c *fiber.Ctx) error {
	middleware.ClearAuthCookie(c, middleware.AdminCookieName)
	return utils.SuccessResponse(c, nil, "Logged out successfully.")
}

// Me returns the authenticated admin user's profile.
func Me(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "Token invalid.", fiber.StatusUnauthorized)
	}

	var user models.User
	if err := config.DB.First(&user, claims.UserID).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "Not found.", fiber.StatusNotFound)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"id":      user.ID,
		"name":    user.Name,
		"email":   user.Email,
		"role":    user.Role,
		"zone_id": user.ZoneID,
		"phone":   user.Phone,
		"status":  user.Status,
	}, "")
}
