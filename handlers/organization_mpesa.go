package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// OrganizationMpesaShow returns the calling ISP's Daraja configuration.
// Secret fields (consumer_secret, passkey) are never included in the
// response — only whether they're set (has_consumer_secret/has_passkey) —
// mirroring how User.Password is write-only. When mode is "platform" (the
// default, and what's returned if the org has no config row at all), no
// credential fields exist to leak in the first place: the platform's own
// shared Daraja credentials are read from Setting/env only at the point of
// use in services/mpesa.go, never through any admin-facing API.
func OrganizationMpesaShow(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)

	var org models.Organization
	config.DB.First(&org, claims.OrganizationID)
	settlement := fiber.Map{
		"settlement_type":           org.SettlementType,
		"settlement_till_number":    org.SettlementTillNumber,
		"settlement_paybill_number": org.SettlementPaybillNumber,
		"settlement_account_number": org.SettlementAccountNumber,
	}

	var cfg models.OrganizationMpesaConfig
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&cfg).Error; err != nil {
		result := fiber.Map{"mode": "platform"}
		for k, v := range settlement {
			result[k] = v
		}
		return utils.SuccessResponse(c, result, "")
	}

	result := fiber.Map{
		"mode":                cfg.Mode,
		"consumer_key":        cfg.ConsumerKey,
		"has_consumer_secret": cfg.ConsumerSecret != "",
		"shortcode":           cfg.Shortcode,
		"has_passkey":         cfg.Passkey != "",
		"callback_url":        cfg.CallbackURL,
		"env":                 cfg.Env,
		"billing_type":        cfg.BillingType,
		"till_number":         cfg.TillNumber,
		"paybill_number":      cfg.PaybillNumber,
		"paybill_account":     cfg.PaybillAccount,
		"bank_name":           cfg.BankName,
		"bank_account":        cfg.BankAccount,
	}
	for k, v := range settlement {
		result[k] = v
	}
	return utils.SuccessResponse(c, result, "")
}

// OrganizationMpesaUpdate lets an ISP super_admin switch between the
// platform's shared Daraja app and their own, and configure their own
// credentials. Blank consumer_secret/passkey fields keep the existing
// stored value (same "leave blank to keep current" pattern used for
// Zone.RouterPassword) rather than overwriting it with an empty string.
func OrganizationMpesaUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to update M-Pesa settings.", "", fiber.StatusForbidden)
	}

	var body struct {
		Mode           string `json:"mode"`
		ConsumerKey    string `json:"consumer_key"`
		ConsumerSecret string `json:"consumer_secret"`
		Shortcode      string `json:"shortcode"`
		Passkey        string `json:"passkey"`
		CallbackURL    string `json:"callback_url"`
		Env            string `json:"env"`
		BillingType    string `json:"billing_type"`
		TillNumber     string `json:"till_number"`
		PaybillNumber  string `json:"paybill_number"`
		PaybillAccount string `json:"paybill_account"`
		BankName       string `json:"bank_name"`
		BankAccount    string `json:"bank_account"`

		// Where Zyra Net should route this ISP's share when they're on
		// "platform" Daraja mode. Ignored (left untouched) when mode is "own".
		SettlementType          string `json:"settlement_type"`
		SettlementTillNumber    string `json:"settlement_till_number"`
		SettlementPaybillNumber string `json:"settlement_paybill_number"`
		SettlementAccountNumber string `json:"settlement_account_number"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.Mode != "platform" && body.Mode != "own" {
		return utils.ErrorResponse(c, "mode must be 'platform' or 'own'.", "", fiber.StatusUnprocessableEntity)
	}
	if body.Mode == "platform" {
		if body.SettlementType != "till" && body.SettlementType != "paybill" {
			return utils.ErrorResponse(c, "settlement_type must be 'till' or 'paybill'.", "", fiber.StatusUnprocessableEntity)
		}
		if body.SettlementType == "till" && body.SettlementTillNumber == "" {
			return utils.ErrorResponse(c, "settlement_till_number is required for till settlement.", "", fiber.StatusUnprocessableEntity)
		}
		if body.SettlementType == "paybill" && (body.SettlementPaybillNumber == "" || body.SettlementAccountNumber == "") {
			return utils.ErrorResponse(c, "settlement_paybill_number and settlement_account_number are required for paybill settlement.", "", fiber.StatusUnprocessableEntity)
		}
		config.DB.Model(&models.Organization{}).Where("id = ?", claims.OrganizationID).Updates(map[string]interface{}{
			"settlement_type":           body.SettlementType,
			"settlement_till_number":    body.SettlementTillNumber,
			"settlement_paybill_number": body.SettlementPaybillNumber,
			"settlement_account_number": body.SettlementAccountNumber,
		})
	}

	var cfg models.OrganizationMpesaConfig
	config.DB.Where(models.OrganizationMpesaConfig{OrganizationID: claims.OrganizationID}).
		FirstOrCreate(&cfg, models.OrganizationMpesaConfig{OrganizationID: claims.OrganizationID})

	cfg.Mode = body.Mode
	if body.Mode == "own" {
		cfg.ConsumerKey = body.ConsumerKey
		if body.ConsumerSecret != "" {
			cfg.ConsumerSecret = body.ConsumerSecret
		}
		cfg.Shortcode = body.Shortcode
		if body.Passkey != "" {
			cfg.Passkey = body.Passkey
		}
		cfg.CallbackURL = body.CallbackURL
		cfg.Env = body.Env
		cfg.BillingType = body.BillingType
		cfg.TillNumber = body.TillNumber
		cfg.PaybillNumber = body.PaybillNumber
		cfg.PaybillAccount = body.PaybillAccount
		cfg.BankName = body.BankName
		cfg.BankAccount = body.BankAccount
	}

	if err := config.DB.Save(&cfg).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to update M-Pesa settings.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, fiber.Map{"mode": cfg.Mode}, "M-Pesa settings updated successfully.")
}
