package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// OrganizationSmsShow returns the calling ISP's SMS gateway configuration.
// The API key is never included in the response — only whether it's set
// (has_hostpinnacle_api_key) — mirroring OrganizationMpesaShow's handling
// of consumer_secret/passkey. When mode is "platform" (the default, and
// what's returned if the org has no config row at all), no credential
// fields exist to leak in the first place: the platform's own shared
// Hostpinnacle credentials are read from Setting/env only at the point of
// use in services/sms.go, never through any admin-facing API.
func OrganizationSmsShow(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)

	var cfg models.OrganizationSmsConfig
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&cfg).Error; err != nil {
		return utils.SuccessResponse(c, fiber.Map{"mode": "platform", "provider": "hostpinnacle"}, "")
	}

	return utils.SuccessResponse(c, fiber.Map{
		"mode":                     cfg.Mode,
		"provider":                 cfg.Provider,
		"hostpinnacle_base_url":    cfg.HostpinnacleBaseURL,
		"has_hostpinnacle_api_key": cfg.HostpinnacleAPIKey != "",
		"hostpinnacle_username":    cfg.HostpinnacleUsername,
		"hostpinnacle_sender_id":   cfg.HostpinnacleSenderID,
	}, "")
}

// OrganizationSmsUpdate lets an ISP super_admin switch between the
// platform's shared Hostpinnacle gateway and their own, and configure
// their own credentials. A blank hostpinnacle_api_key keeps the existing
// stored value (same "leave blank to keep current" pattern used for
// OrganizationMpesaConfig.ConsumerSecret/Passkey) rather than overwriting
// it with an empty string.
//
// Provider is currently restricted to "hostpinnacle": that's the only SMS
// gateway services/sms.go actually knows how to send through. See the
// Provider field doc comment on models.OrganizationSmsConfig for why the
// field exists at all despite that restriction.
func OrganizationSmsUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to update SMS settings.", "", fiber.StatusForbidden)
	}

	var body struct {
		Mode                 string `json:"mode"`
		Provider             string `json:"provider"`
		HostpinnacleBaseURL  string `json:"hostpinnacle_base_url"`
		HostpinnacleAPIKey   string `json:"hostpinnacle_api_key"`
		HostpinnacleUsername string `json:"hostpinnacle_username"`
		HostpinnacleSenderID string `json:"hostpinnacle_sender_id"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.Mode != "platform" && body.Mode != "own" {
		return utils.ErrorResponse(c, "mode must be 'platform' or 'own'.", "", fiber.StatusUnprocessableEntity)
	}
	if body.Provider == "" {
		body.Provider = "hostpinnacle"
	}
	if body.Provider != "hostpinnacle" {
		return utils.ErrorResponse(c, "provider must be 'hostpinnacle' — no other SMS gateway is currently supported.", "", fiber.StatusUnprocessableEntity)
	}

	var cfg models.OrganizationSmsConfig
	config.DB.Where(models.OrganizationSmsConfig{OrganizationID: claims.OrganizationID}).
		FirstOrCreate(&cfg, models.OrganizationSmsConfig{OrganizationID: claims.OrganizationID})

	cfg.Mode = body.Mode
	cfg.Provider = body.Provider
	if body.Mode == "own" {
		cfg.HostpinnacleBaseURL = body.HostpinnacleBaseURL
		if body.HostpinnacleAPIKey != "" {
			cfg.HostpinnacleAPIKey = body.HostpinnacleAPIKey
		}
		cfg.HostpinnacleUsername = body.HostpinnacleUsername
		cfg.HostpinnacleSenderID = body.HostpinnacleSenderID
	}

	if err := config.DB.Save(&cfg).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to update SMS settings.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, fiber.Map{"mode": cfg.Mode, "provider": cfg.Provider}, "SMS settings updated successfully.")
}
