package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
)

// PlatformLogin authenticates a Zyra Net Super Admin (SA) platform user.
// This is intentionally a separate table/JWT flow from admin/customer auth
// (see middleware.PlatformAuth) so a platform credential is never reachable
// through the per-ISP admin login.
func PlatformLogin(c *fiber.Ctx) error {
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

	var user models.PlatformUser
	if err := config.DB.Where("email = ?", body.Email).First(&user).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid login credentials.", "Authentication failed.", fiber.StatusUnauthorized)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(body.Password)); err != nil {
		return utils.ErrorResponse(c, "Invalid login credentials.", "Authentication failed.", fiber.StatusUnauthorized)
	}

	if user.Status != "active" {
		return utils.ErrorResponse(c, "Your account is inactive.", "Account disabled.", fiber.StatusForbidden)
	}

	token, err := middleware.GeneratePlatformToken(user.ID)
	if err != nil {
		return utils.ErrorResponse(c, "Token generation failed.", "Server error.", fiber.StatusInternalServerError)
	}

	middleware.SetAuthCookie(c, middleware.PlatformCookieName, token)

	return utils.SuccessResponse(c, fiber.Map{
		"token": token,
		"user": fiber.Map{
			"id":    user.ID,
			"name":  user.Name,
			"email": user.Email,
		},
	}, "Login successful.")
}

// PlatformLogout clears the platform session cookie.
func PlatformLogout(c *fiber.Ctx) error {
	middleware.ClearAuthCookie(c, middleware.PlatformCookieName)
	return utils.SuccessResponse(c, nil, "Logged out successfully.")
}

// PlatformMe returns the authenticated platform user's profile.
func PlatformMe(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "Token invalid.", fiber.StatusUnauthorized)
	}

	var user models.PlatformUser
	if err := config.DB.First(&user, claims.PlatformUserID).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "Not found.", fiber.StatusNotFound)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"id":     user.ID,
		"name":   user.Name,
		"email":  user.Email,
		"status": user.Status,
	}, "")
}
