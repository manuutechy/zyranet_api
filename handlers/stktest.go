package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

type stkTestBody struct {
	Phone  string  `json:"phone"`
	Amount float64 `json:"amount"`
}

func (b stkTestBody) validate() error {
	if b.Phone == "" {
		return fiber.NewError(fiber.StatusUnprocessableEntity, "phone is required.")
	}
	if b.Amount <= 0 {
		return fiber.NewError(fiber.StatusUnprocessableEntity, "amount must be greater than zero.")
	}
	return nil
}

// runStkTest fires one real test prompt through the provider the zone's ISP
// uses (Cyzora or Daraja — see services.PaymentRouting) without creating a
// Payment row, so test pushes never pollute revenue/billing figures.
func runStkTest(zoneID uint, body stkTestBody) (fiber.Map, error) {
	start, err := mpesaSvcGlobal.TestPrompt(zoneID, body.Phone, body.Amount)
	if err != nil {
		return nil, err
	}
	return fiber.Map{
		"provider":            start.Provider,
		"checkout_request_id": start.Reference,
		"message":             start.Message,
		"is_mock":             start.IsMock,
	}, nil
}

// OrganizationStkTest lets an ISP super_admin fire a test STK push against
// their own configured Daraja (shared or their own), to verify it actually
// works without going through a real customer purchase flow.
func OrganizationStkTest(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var body stkTestBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if err := body.validate(); err != nil {
		fe := err.(*fiber.Error)
		return utils.ErrorResponse(c, fe.Message, "Validation failed.", fe.Code)
	}

	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone).Error; err != nil {
		return utils.ErrorResponse(c, "No zone configured for your organization yet.", "", fiber.StatusUnprocessableEntity)
	}

	result, err := runStkTest(zone.ID, body)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "STK push failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, result, "Test STK push sent — check the phone.")
}

// PlatformOrganizationStkTest lets SA fire a test STK push on behalf of any
// organization, using whichever Daraja config that org actually resolves
// to — useful for verifying a new ISP's own Daraja setup during onboarding
// support, without needing their admin credentials.
func PlatformOrganizationStkTest(c *fiber.Ctx) error {
	var org models.Organization
	if err := config.DB.First(&org, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusNotFound)
	}

	var body stkTestBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if err := body.validate(); err != nil {
		fe := err.(*fiber.Error)
		return utils.ErrorResponse(c, fe.Message, "Validation failed.", fe.Code)
	}

	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", org.ID).First(&zone).Error; err != nil {
		return utils.ErrorResponse(c, "This organization has no zones configured yet.", "", fiber.StatusUnprocessableEntity)
	}

	result, err := runStkTest(zone.ID, body)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "STK push failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, result, "Test STK push sent — check the phone.")
}
