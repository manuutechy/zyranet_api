package handlers

import (
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/utils"
)

// These handlers let Zyra Net platform (SA) staff manage the shared
// infrastructure credentials that back the "platform" Daraja mode (see
// OrganizationMpesaConfig) and the Hostpinnacle SMS integration — the same
// global Setting keys services/mpesa.go and services/sms.go already read
// via getSetting/GetSetting. They're reachable only via PlatformAuth, never
// through the ISP-facing /settings endpoints, so an ISP admin can no
// longer be the one place these shared secrets are readable from — that
// surface now belongs entirely to the platform side.

var daraKeys = []string{
	"mpesa_environment", "mpesa_consumer_key", "mpesa_consumer_secret",
	"mpesa_shortcode", "mpesa_passkey", "mpesa_callback_url",
	"mpesa_billing_type", "mpesa_transaction_type", "mpesa_till_number",
	"mpesa_paybill_number", "mpesa_paybill_account", "mpesa_bank_name",
	"mpesa_bank_account",
}

var smsKeys = []string{
	"sms_provider", "hostpinnacle_base_url", "hostpinnacle_api_key",
	"hostpinnacle_username", "hostpinnacle_password", "hostpinnacle_sender_id",
}

func filterSettings(all map[string]string, keys []string) fiber.Map {
	result := make(fiber.Map, len(keys))
	for _, k := range keys {
		result[k] = all[k]
	}
	return result
}

func parseSettingsUpdateBody(c *fiber.Ctx) (map[string]string, error) {
	var rawBody map[string]interface{}
	if err := c.BodyParser(&rawBody); err != nil {
		return nil, err
	}
	settingsToUpdate := make(map[string]string)
	for k, v := range rawBody {
		if strVal, ok := v.(string); ok {
			settingsToUpdate[k] = strVal
		} else if v != nil {
			settingsToUpdate[k] = fmt.Sprintf("%v", v)
		}
	}
	return settingsToUpdate, nil
}

// PlatformDarajaShow returns Zyra Net's own shared Daraja app credentials.
func PlatformDarajaShow(c *fiber.Ctx) error {
	return utils.SuccessResponse(c, filterSettings(loadAllSettings(), daraKeys), "")
}

// PlatformDarajaUpdate updates Zyra Net's own shared Daraja app credentials.
func PlatformDarajaUpdate(c *fiber.Ctx) error {
	settingsToUpdate, err := parseSettingsUpdateBody(c)
	if err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	UpsertSettings(settingsToUpdate)
	return utils.SuccessResponse(c, filterSettings(loadAllSettings(), daraKeys), "Shared Daraja settings updated successfully.")
}

// PlatformSmsShow returns Zyra Net's Hostpinnacle SMS gateway credentials.
func PlatformSmsShow(c *fiber.Ctx) error {
	return utils.SuccessResponse(c, filterSettings(loadAllSettings(), smsKeys), "")
}

// PlatformSmsUpdate updates Zyra Net's Hostpinnacle SMS gateway credentials.
func PlatformSmsUpdate(c *fiber.Ctx) error {
	settingsToUpdate, err := parseSettingsUpdateBody(c)
	if err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	UpsertSettings(settingsToUpdate)
	return utils.SuccessResponse(c, filterSettings(loadAllSettings(), smsKeys), "SMS gateway settings updated successfully.")
}

// PlatformSmsTest sends a test SMS via the shared Hostpinnacle gateway.
func PlatformSmsTest(c *fiber.Ctx) error {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := c.BodyParser(&body); err != nil || body.Phone == "" {
		return utils.ErrorResponse(c, "A phone number is required.", "", fiber.StatusUnprocessableEntity)
	}
	if _, err := smsSvcGlobal.Send(body.Phone, "This is a test message from Zyra Net Platform."); err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to send test SMS.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Test SMS sent successfully.")
}
