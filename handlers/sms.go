package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// TestSms sends a real SMS via the configured provider.
// Only super_admin may call this to avoid abuse.
func TestSms(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var body struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	body.Phone = strings.TrimSpace(body.Phone)
	body.Message = strings.TrimSpace(body.Message)

	if body.Phone == "" {
		return utils.ErrorResponse(c, "Phone number is required.", "", fiber.StatusBadRequest)
	}
	if body.Message == "" {
		body.Message = "Hello! This is a test SMS from Zyra Net Billing System."
	}

	svc := services.NewSmsService()
	log, err := svc.Send(body.Phone, body.Message)
	if err != nil {
		return utils.ErrorResponse(c, "SMS sending failed: "+err.Error(), "", fiber.StatusBadGateway)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"status":           log.Status,
		"providerResponse": log.ProviderResponse,
	}, "Test SMS dispatched successfully.")
}
