package handlers

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

var platformSettingDefaults = map[string]string{
	"default_commission_percent": "0",
}

// GetPlatformSetting reads a platform-wide setting, falling back to its default.
func GetPlatformSetting(key string) string {
	var setting models.PlatformSetting
	if err := config.DB.Where("`key` = ?", key).First(&setting).Error; err == nil && setting.Value != nil {
		v := strings.TrimSpace(*setting.Value)
		if v != "" {
			return v
		}
	}
	return strings.TrimSpace(platformSettingDefaults[key])
}

// PlatformSettingsIndex returns all platform-wide SA settings.
func PlatformSettingsIndex(c *fiber.Ctx) error {
	result := make(map[string]string)
	for k, v := range platformSettingDefaults {
		result[k] = v
	}
	var settings []models.PlatformSetting
	config.DB.Find(&settings)
	for _, s := range settings {
		if s.Value != nil {
			result[s.Key] = *s.Value
		}
	}
	return utils.SuccessResponse(c, result, "")
}

// PlatformSettingsUpdate upserts platform-wide SA settings (e.g. the
// default commission percentage used for the Overview earnings estimate).
func PlatformSettingsUpdate(c *fiber.Ctx) error {
	var body map[string]interface{}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	for key, val := range body {
		strVal := strings.TrimSpace(toStringVal(val))
		if strVal == "" {
			config.DB.Where("`key` = ?", key).Delete(&models.PlatformSetting{})
			continue
		}
		config.DB.Where(models.PlatformSetting{Key: key}).Assign(models.PlatformSetting{Value: &strVal}).FirstOrCreate(&models.PlatformSetting{})
	}

	return utils.SuccessResponse(c, nil, "Platform settings updated successfully.")
}

func toStringVal(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}
