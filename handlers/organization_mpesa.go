package handlers

import (
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
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
		"direct_settlement":         org.DirectSettlement,
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
		"billing_type":        normalizedBillingType(cfg.BillingType),
		"till_number":         cfg.TillNumber,
		"paybill_number":      cfg.PaybillNumber,
		"paybill_account":     cfg.PaybillAccount,
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
		// On the shared app the destination IS where customer money goes:
		// saving it switches on direct settlement (each STK push pays it).
		config.DB.Model(&models.Organization{}).Where("id = ?", claims.OrganizationID).Updates(map[string]interface{}{
			"settlement_type":           body.SettlementType,
			"settlement_till_number":    strings.TrimSpace(body.SettlementTillNumber),
			"settlement_paybill_number": strings.TrimSpace(body.SettlementPaybillNumber),
			"settlement_account_number": strings.TrimSpace(body.SettlementAccountNumber),
			"direct_settlement":         true,
		})
	}

	var cfg models.OrganizationMpesaConfig
	config.DB.Where(models.OrganizationMpesaConfig{OrganizationID: claims.OrganizationID}).
		FirstOrCreate(&cfg, models.OrganizationMpesaConfig{OrganizationID: claims.OrganizationID})

	cfg.Mode = body.Mode
	if body.Mode == "own" {
		// A payment can only settle into the shortcode/till the Daraja app
		// belongs to, so collection is paybill or till. "bank" (the old third
		// option) is a settlement destination, not something a customer can be
		// charged into — treat any stored/legacy "bank" as paybill.
		billingType := strings.ToLower(strings.TrimSpace(body.BillingType))
		if billingType == "" || billingType == "bank" {
			billingType = "paybill"
		}
		if billingType != "paybill" && billingType != "till" {
			return utils.ErrorResponse(c, "billing_type must be 'paybill' or 'till'.", "", fiber.StatusUnprocessableEntity)
		}
		env := strings.ToLower(strings.TrimSpace(body.Env))
		if env == "" {
			env = "sandbox"
		}
		if env != "sandbox" && env != "production" {
			return utils.ErrorResponse(c, "env must be 'sandbox' or 'production'.", "", fiber.StatusUnprocessableEntity)
		}

		// Blank secret/passkey keep the stored value, so only complain when
		// there's neither a new nor a stored one.
		consumerSecret := cfg.ConsumerSecret
		if strings.TrimSpace(body.ConsumerSecret) != "" {
			consumerSecret = strings.TrimSpace(body.ConsumerSecret)
		}
		passkey := cfg.Passkey
		if strings.TrimSpace(body.Passkey) != "" {
			passkey = strings.TrimSpace(body.Passkey)
		}
		var missing []string
		if strings.TrimSpace(body.ConsumerKey) == "" {
			missing = append(missing, "consumer key")
		}
		if consumerSecret == "" {
			missing = append(missing, "consumer secret")
		}
		if strings.TrimSpace(body.Shortcode) == "" {
			missing = append(missing, "shortcode")
		}
		if passkey == "" {
			missing = append(missing, "passkey")
		}
		if billingType == "till" && strings.TrimSpace(body.TillNumber) == "" {
			missing = append(missing, "till number")
		}
		if len(missing) > 0 {
			return utils.ErrorResponse(c,
				"Your own Daraja app needs: "+strings.Join(missing, ", ")+". Fill these in, or switch back to Zyra Net's shared app.",
				"", fiber.StatusUnprocessableEntity)
		}

		cfg.ConsumerKey = strings.TrimSpace(body.ConsumerKey)
		cfg.ConsumerSecret = consumerSecret
		cfg.Shortcode = strings.TrimSpace(body.Shortcode)
		cfg.Passkey = passkey
		cfg.CallbackURL = strings.TrimSpace(body.CallbackURL)
		cfg.Env = env
		cfg.BillingType = billingType
		cfg.TillNumber = strings.TrimSpace(body.TillNumber)
		cfg.PaybillNumber = strings.TrimSpace(body.PaybillNumber)
		cfg.PaybillAccount = strings.TrimSpace(body.PaybillAccount)
		cfg.BankName = ""
		cfg.BankAccount = ""
	}

	if err := config.DB.Save(&cfg).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to update M-Pesa settings.", fiber.StatusInternalServerError)
	}
	services.InvalidateMpesaCaches()

	return utils.SuccessResponse(c, fiber.Map{"mode": cfg.Mode}, "M-Pesa settings updated successfully.")
}

// OrganizationMpesaRegisterC2B registers C2B Validation and Confirmation URLs with Safaricom Daraja.
func OrganizationMpesaRegisterC2B(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var body struct {
		ResponseType    string `json:"response_type"`
		ConfirmationURL string `json:"confirmation_url"`
		ValidationURL   string `json:"validation_url"`
	}
	_ = c.BodyParser(&body)

	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone).Error; err != nil {
		return utils.ErrorResponse(c, "No zone configured for your organization yet.", "", fiber.StatusUnprocessableEntity)
	}

	res, err := mpesaSvcGlobal.RegisterC2BURLs(zone.ID, body.ConfirmationURL, body.ValidationURL, body.ResponseType)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "C2B URL registration failed.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, res, "C2B URLs registered successfully with Safaricom Daraja.")
}

// OrganizationMpesaTest tests OAuth authentication against Safaricom Daraja.
func OrganizationMpesaTest(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var zone models.Zone
	var zoneID uint = 0
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone).Error; err == nil {
		zoneID = zone.ID
	}

	creds := mpesaSvcGlobal.ResolveMpesaCreds(zoneID)
	token, err := mpesaSvcGlobal.GetAccessToken(creds)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Daraja Auth Test Failed", fiber.StatusBadRequest)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"status":      "connected",
		"environment": creds.Env,
		"token_valid": token != "",
	}, fmt.Sprintf("Daraja OAuth authentication successful on %s environment.", creds.Env))
}

// normalizedBillingType reports a stored billing type as one of the two
// collection types the UI offers (legacy "bank" reads back as "paybill").
func normalizedBillingType(t string) string {
	if strings.EqualFold(strings.TrimSpace(t), "till") {
		return "till"
	}
	return "paybill"
}
