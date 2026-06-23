package handlers

import (
	crand "crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

var settingDefaults = map[string]string{
	"company_name":            "Zyra Net ISP",
	"logo":                    "",
	"primary_color":           "#f97316",
	"support_phone":           "0113297270",
	"support_whatsapp":        "",
	"mpesa_environment":       "sandbox",
	"mpesa_consumer_key":      "",
	"mpesa_consumer_secret":   "",
	"mpesa_shortcode":         "174379",
	"mpesa_passkey":           "",
	"mpesa_callback_url":      "",
	"mpesa_billing_type":      "paybill",
	"mpesa_till_number":       "",
	"mpesa_paybill_number":    "",
	"mpesa_paybill_account":   "",
	"mpesa_bank_account":      "",
	"mpesa_bank_name":         "",
	"africastalking_api_key":   "",
	"africastalking_username":  "sandbox",
	"africastalking_sender":    "ZyraNet",
	"sms_provider":             "africastalking",
	"hostpinnacle_base_url":    "https://smsportal.hostpinnacle.co.ke/SMSApi/send",
	"hostpinnacle_api_key":     "",
	"hostpinnacle_username":    "",
	"hostpinnacle_sender_id":   "",
	"banner_image_url":         "",
	"banner_enabled":           "yes",
}

// SettingsIndex returns all settings.
func SettingsIndex(c *fiber.Ctx) error {
	settings := loadAllSettings()
	return utils.SuccessResponse(c, settings, "")
}

// SettingsPublic returns public-facing branding settings (no auth).
func SettingsPublic(c *fiber.Ctx) error {
	settings := loadAllSettings()

	logoURL := settings["logo"]
	if logoURL != "" && !strings.HasPrefix(logoURL, "http") {
		logoURL = c.BaseURL() + "/" + strings.TrimLeft(logoURL, "/")
	}
	bannerURL := settings["banner_image_url"]
	if bannerURL != "" && !strings.HasPrefix(bannerURL, "http") {
		bannerURL = c.BaseURL() + "/" + strings.TrimLeft(bannerURL, "/")
	}

	return utils.SuccessResponse(c, fiber.Map{
		"companyName":       settings["company_name"],
		"logoUrl":           logoURL,
		"primaryColor":      settings["primary_color"],
		"supportPhone":      settings["support_phone"],
		"supportWhatsapp":   settings["support_whatsapp"],
		"bannerImageUrl":    bannerURL,
		"bannerEnabled":     settings["banner_enabled"],
		"mpesaBillingType":  settings["mpesa_billing_type"],
		"mpesaBankName":     settings["mpesa_bank_name"],
		"mpesaShortcode":    settings["mpesa_shortcode"],
		"mpesaTillNumber":   settings["mpesa_till_number"],
		"mpesaPaybillNumber": settings["mpesa_paybill_number"],
		"mpesaPaybillAccount": settings["mpesa_paybill_account"],
		"mpesaBankAccount":  settings["mpesa_bank_account"],
	}, "")
}

// SettingsUpdate updates or creates settings (super_admin only).
func SettingsUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to update settings.", "", fiber.StatusForbidden)
	}

	var rawBody map[string]interface{}
	if err := c.BodyParser(&rawBody); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	settingsToUpdate := make(map[string]string)

	// Check if there is a nested "settings" key
	if nested, ok := rawBody["settings"].(map[string]interface{}); ok {
		for k, v := range nested {
			if strVal, ok := v.(string); ok {
				settingsToUpdate[k] = strVal
			} else if v != nil {
				settingsToUpdate[k] = fmt.Sprintf("%v", v)
			}
		}
	} else {
		// Treat the raw body as a flat settings map
		for k, v := range rawBody {
			if strVal, ok := v.(string); ok {
				settingsToUpdate[k] = strVal
			} else if v != nil {
				settingsToUpdate[k] = fmt.Sprintf("%v", v)
			}
		}
	}

	credentialKeys := []string{
		"mpesa_consumer_key", "mpesa_consumer_secret", "mpesa_passkey", "mpesa_shortcode",
		"mpesa_till_number", "mpesa_paybill_number", "mpesa_paybill_account", "mpesa_bank_account",
		"africastalking_api_key", "africastalking_username", "africastalking_sender",
		"hostpinnacle_api_key", "hostpinnacle_username", "hostpinnacle_sender_id",
	}
	isCredKey := func(k string) bool {
		for _, ck := range credentialKeys {
			if ck == k {
				return true
			}
		}
		return false
	}

	for key, val := range settingsToUpdate {
		trimmed := strings.TrimSpace(val)
		if isCredKey(key) {
			trimmed = strings.ReplaceAll(trimmed, " ", "")
		}
		if trimmed == "" {
			config.DB.Where("`key` = ?", key).Delete(&models.Setting{})
		} else {
			config.DB.Where(models.Setting{Key: key}).Assign(models.Setting{Value: &trimmed}).FirstOrCreate(&models.Setting{})
		}
	}

	return utils.SuccessResponse(c, loadAllSettings(), "Settings updated successfully.")
}

// SettingsUploadImage handles logo/banner image uploads.
func SettingsUploadImage(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	file, err := c.FormFile("image")
	if err != nil {
		return utils.ErrorResponse(c, "No image file found in request.", "", fiber.StatusBadRequest)
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".svg": true}
	if !allowed[ext] {
		return utils.ErrorResponse(c, "Invalid file type.", "", fiber.StatusBadRequest)
	}

	uploadDir := "public/uploads"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return utils.ErrorResponse(c, "Failed to create upload directory.", "", fiber.StatusInternalServerError)
	}

	filename := fmt.Sprintf("%d_%s%s", time.Now().UnixNano(), randomHex(3), ext)
	dst := filepath.Join(uploadDir, filename)

	src, err := file.Open()
	if err != nil {
		return utils.ErrorResponse(c, "Failed to read uploaded file.", "", fiber.StatusInternalServerError)
	}
	defer src.Close()

	out, err := os.Create(dst)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to save file.", "", fiber.StatusInternalServerError)
	}
	defer out.Close()
	io.Copy(out, src)

	path := "uploads/" + filename
	return utils.SuccessResponse(c, fiber.Map{
		"path": path,
		"url":  c.BaseURL() + "/" + path,
	}, "Image uploaded successfully.")
}

// loadAllSettings merges DB settings with defaults.
func loadAllSettings() map[string]string {
	result := make(map[string]string)
	for k, v := range settingDefaults {
		result[k] = v
	}
	var settings []models.Setting
	config.DB.Find(&settings)
	for _, s := range settings {
		if s.Value != nil {
			result[s.Key] = *s.Value
		}
	}
	return result
}

func randomHex(n int) string {
	b := make([]byte, n)
	crand.Read(b)
	return fmt.Sprintf("%x", b)
}
