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

	var cfg models.OrganizationMpesaConfig
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&cfg).Error; err != nil {
		return utils.SuccessResponse(c, fiber.Map{"mode": "platform"}, "")
	}

	return utils.SuccessResponse(c, fiber.Map{
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
	}, "")
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
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.Mode != "platform" && body.Mode != "own" {
		return utils.ErrorResponse(c, "mode must be 'platform' or 'own'.", "", fiber.StatusUnprocessableEntity)
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
