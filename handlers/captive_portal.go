package handlers

import (
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// availableCaptiveThemes are the page layouts the `customer` app knows how
// to render — keep in sync with that app's theme registry.
var availableCaptiveThemes = []string{"classic", "split"}

func isValidCaptiveTheme(theme string) bool {
	for _, t := range availableCaptiveThemes {
		if t == theme {
			return true
		}
	}
	return false
}

// CaptivePortalPublicSettings returns the branding/theme the captive portal
// (the `customer` app) should render for a connecting hotspot user,
// resolved from ?zone_id= (forwarded from the router's login redirect —
// see resolveHotspotZone). Falls back to the first zone in the DB if
// zone_id is missing/invalid, matching the rest of the hotspot flow's
// backward-compat behavior for routers not yet passing a zone.
func CaptivePortalPublicSettings(c *fiber.Ctx) error {
	zone, err := resolveHotspotZone(c, "")
	if err != nil {
		return utils.ErrorResponse(c, "No zone configured on the server.", "", fiber.StatusInternalServerError)
	}

	var org models.Organization
	if err := config.DB.First(&org, zone.OrganizationID).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusInternalServerError)
	}

	companyName := org.CaptivePortalCompanyName
	if companyName == "" {
		companyName = org.Name
	}
	primaryColor := org.CaptivePortalPrimaryColor
	if primaryColor == "" {
		primaryColor = "#FF6B00"
	}
	theme := org.CaptivePortalTheme
	if !isValidCaptiveTheme(theme) {
		theme = "classic"
	}

	logoURL := org.CaptivePortalLogo
	if logoURL != "" && logoURL[0] != 'h' { // not already an absolute http(s) URL
		logoURL = c.BaseURL() + "/" + logoURL
	}

	return utils.SuccessResponse(c, fiber.Map{
		"zone_id":       zone.ID,
		"theme":         theme,
		"company_name":  companyName,
		"logo_url":      logoURL,
		"primary_color": primaryColor,
		"tagline":       org.CaptivePortalTagline,
		"support_phone": org.CaptivePortalSupportPhone,
	}, "")
}

// CaptivePortalSettingsShow returns the calling ISP's own captive portal
// branding/theme configuration for editing.
func CaptivePortalSettingsShow(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var org models.Organization
	if err := config.DB.First(&org, claims.OrganizationID).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusNotFound)
	}

	var zones []models.Zone
	config.DB.Where("organization_id = ?", claims.OrganizationID).Order("name ASC").Find(&zones)

	return utils.SuccessResponse(c, fiber.Map{
		"theme":         org.CaptivePortalTheme,
		"company_name":  org.CaptivePortalCompanyName,
		"logo":          org.CaptivePortalLogo,
		"primary_color": org.CaptivePortalPrimaryColor,
		"tagline":       org.CaptivePortalTagline,
		"support_phone": org.CaptivePortalSupportPhone,
		"zones":         zones,
		"themes":        availableCaptiveThemes,
	}, "")
}

// CaptivePortalSettingsUpdate updates the calling ISP's captive portal
// branding/theme.
func CaptivePortalSettingsUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to update captive portal settings.", "", fiber.StatusForbidden)
	}

	var body struct {
		Theme        string `json:"theme"`
		CompanyName  string `json:"company_name"`
		Logo         string `json:"logo"`
		PrimaryColor string `json:"primary_color"`
		Tagline      string `json:"tagline"`
		SupportPhone string `json:"support_phone"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if !isValidCaptiveTheme(body.Theme) {
		return utils.ErrorResponse(c, fmt.Sprintf("theme must be one of: %v", availableCaptiveThemes), "", fiber.StatusUnprocessableEntity)
	}

	if err := config.DB.Model(&models.Organization{}).Where("id = ?", claims.OrganizationID).Updates(map[string]interface{}{
		"captive_portal_theme":         body.Theme,
		"captive_portal_company_name":  body.CompanyName,
		"captive_portal_logo":          body.Logo,
		"captive_portal_primary_color": body.PrimaryColor,
		"captive_portal_tagline":       body.Tagline,
		"captive_portal_support_phone": body.SupportPhone,
	}).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, nil, "Captive portal settings updated successfully.")
}

// ZoneCaptiveLoginHTML generates the login.html a MikroTik router serves to
// a newly-connected hotspot client. It doesn't render any UI itself — it
// immediately redirects to captive.zyranet.co.ke with this zone's ID plus
// RouterOS's own $(...) template variables (substituted by the router when
// it serves the file, not by this handler), so the customer app can
// identify which ISP/zone the client belongs to and fetch that org's
// branding via CaptivePortalPublicSettings. Uploaded manually via WinBox
// into the hotspot's Files directory (same manual-upload workflow already
// used for the main .rsc setup script) — RouterOS hotspot login pages
// aren't provisionable via .rsc script commands.
func ZoneCaptiveLoginHTML(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}

	portalHost := "https://captive.zyranet.co.ke"
	redirectURL := fmt.Sprintf(
		"%s/login?zone=%d&mac=$(mac)&ip=$(ip)&link-login=$(link-login-only)&link-orig=$(link-orig-esc)",
		portalHost, zone.ID,
	)

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Redirecting…</title>
  <meta http-equiv="refresh" content="0; url=%s">
</head>
<body>
  <script>window.location.replace(%q);</script>
  <p>Redirecting to the login page…</p>
</body>
</html>`, redirectURL, redirectURL)

	c.Set("Content-Type", "text/html")
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="login.html"`))
	return c.SendString(html)
}
