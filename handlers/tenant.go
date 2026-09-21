package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// validateSubdomain normalises a subdomain and rejects malformed, reserved and
// platform-owned labels (e.g. the live admin site's own hostname).
func validateSubdomain(raw string) (string, error) {
	sub, err := utils.NormalizeSubdomain(raw)
	if err != nil {
		return "", err
	}
	if sub != "" && middleware.IsPlatformLabel(sub) {
		return "", errors.New("that subdomain is reserved")
	}
	return sub, nil
}

// TenantPublic returns the public branding of the ISP that owns a subdomain,
// so the staff-admin login page served at <subdomain>.<BASE_DOMAIN> can show
// which ISP it belongs to — and tell a typo'd/unknown subdomain apart.
// The subdomain comes from ?subdomain=, else from the request's Origin.
// Only public branding is returned.
func TenantPublic(c *fiber.Ctx) error {
	sub := ""
	if q := strings.TrimSpace(c.Query("subdomain")); q != "" {
		// The platform's own sites (admin., bit1., platform., …) are not ISP
		// portals, but they are not errors either: say so, so the login page
		// there shows its normal, unbranded form instead of "unknown portal".
		if utils.IsReservedSubdomain(q) || middleware.IsPlatformLabel(q) {
			return utils.SuccessResponse(c, fiber.Map{"platform": true}, "")
		}
		normalized, err := validateSubdomain(q)
		if err != nil {
			return utils.ErrorResponse(c, "This ISP portal does not exist.", "", fiber.StatusNotFound)
		}
		sub = normalized
	} else {
		sub, _, _, _ = middleware.TenantFromRequest(c)
	}
	if sub == "" {
		return utils.ErrorResponse(c, "This ISP portal does not exist.", "", fiber.StatusNotFound)
	}

	var org models.Organization
	if err := config.DB.Where("subdomain = ?", sub).First(&org).Error; err != nil {
		return utils.ErrorResponse(c, "This ISP portal does not exist.", "", fiber.StatusNotFound)
	}

	name := org.CaptivePortalCompanyName
	if name == "" {
		name = org.Name
	}
	logoURL := org.CaptivePortalLogo
	if logoURL != "" && logoURL[0] != 'h' { // not already an absolute http(s) URL
		logoURL = c.BaseURL() + "/" + logoURL
	}
	return utils.SuccessResponse(c, fiber.Map{
		"subdomain":     sub,
		"name":          name,
		"logo_url":      logoURL,
		"primary_color": org.CaptivePortalPrimaryColor,
		"suspended":     org.Status == "suspended",
	}, "")
}
