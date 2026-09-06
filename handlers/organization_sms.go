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

	mobilesasaBaseURL := cfg.MobilesasaBaseURL
	if mobilesasaBaseURL == "" {
		mobilesasaBaseURL = "https://api.mobilesasa.com/v1/send/message"
	}

	return utils.SuccessResponse(c, fiber.Map{
		"mode":                     cfg.Mode,
		"provider":                 cfg.Provider,
		"hostpinnacle_base_url":    cfg.HostpinnacleBaseURL,
		"has_hostpinnacle_api_key": cfg.HostpinnacleAPIKey != "",
		"hostpinnacle_username":    cfg.HostpinnacleUsername,
		"hostpinnacle_sender_id":   cfg.HostpinnacleSenderID,
		"mobilesasa_base_url":      mobilesasaBaseURL,
		"has_mobilesasa_api_token": cfg.MobilesasaAPIToken != "",
		"mobilesasa_sender_id":     cfg.MobilesasaSenderID,
	}, "")
}

// OrganizationSmsUpdate lets an ISP super_admin switch between the
// platform's shared gateway and their own, and configure HostPinnacle or MobileSasa.
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
		MobilesasaBaseURL    string `json:"mobilesasa_base_url"`
		MobilesasaAPIToken   string `json:"mobilesasa_api_token"`
		MobilesasaSenderID   string `json:"mobilesasa_sender_id"`
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
	if body.Provider != "hostpinnacle" && body.Provider != "mobilesasa" {
		return utils.ErrorResponse(c, "provider must be 'hostpinnacle' or 'mobilesasa'.", "", fiber.StatusUnprocessableEntity)
	}

	var cfg models.OrganizationSmsConfig
	config.DB.Where(models.OrganizationSmsConfig{OrganizationID: claims.OrganizationID}).
		FirstOrCreate(&cfg, models.OrganizationSmsConfig{OrganizationID: claims.OrganizationID})

	cfg.Mode = body.Mode
	cfg.Provider = body.Provider
	if body.Mode == "own" {
		if body.Provider == "hostpinnacle" {
			cfg.HostpinnacleBaseURL = body.HostpinnacleBaseURL
			if body.HostpinnacleAPIKey != "" {
				cfg.HostpinnacleAPIKey = body.HostpinnacleAPIKey
			}
			cfg.HostpinnacleUsername = body.HostpinnacleUsername
			cfg.HostpinnacleSenderID = body.HostpinnacleSenderID
		} else if body.Provider == "mobilesasa" {
			if body.MobilesasaBaseURL != "" {
				cfg.MobilesasaBaseURL = body.MobilesasaBaseURL
			} else {
				cfg.MobilesasaBaseURL = "https://api.mobilesasa.com/v1/send/message"
			}
			if body.MobilesasaAPIToken != "" {
				cfg.MobilesasaAPIToken = body.MobilesasaAPIToken
			}
			if body.MobilesasaSenderID != "" {
				cfg.MobilesasaSenderID = body.MobilesasaSenderID
			}
		}
	}

	if err := config.DB.Save(&cfg).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to update SMS settings.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, fiber.Map{"mode": cfg.Mode, "provider": cfg.Provider}, "SMS settings updated successfully.")
}
