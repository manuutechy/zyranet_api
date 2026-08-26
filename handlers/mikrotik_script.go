package handlers

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// MikroTikScriptGenerate generates and downloads a .rsc RouterOS config file (authenticated).
func MikroTikScriptGenerate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}

	content, filename, err := scriptSvc.GenerateScript(zone.ID)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Script generation failed.", fiber.StatusBadRequest)
	}

	c.Set("Content-Type", "text/plain")
	c.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return c.SendString(content)
}

// PublicZoneSetupScript serves the RouterOS .rsc script directly to MikroTik /tool fetch
func PublicZoneSetupScript(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}

	content, filename, err := scriptSvc.GenerateScript(zone.ID)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Script generation failed.", fiber.StatusBadRequest)
	}

	c.Set("Content-Type", "text/plain; charset=utf-8")
	c.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return c.SendString(content)
}

// PublicZoneLoginPage serves the cloud redirect login.html directly to MikroTik /tool fetch
func PublicZoneLoginPage(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
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

	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.SendString(html)
}

// PublicZoneHeartbeat receives 1-minute heartbeats from active MikroTik routers.
func PublicZoneHeartbeat(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}

	now := time.Now()
	config.DB.Model(&zone).Updates(map[string]interface{}{
		"last_seen_at": &now,
		"last_status":  "online",
	})

	return utils.SuccessResponse(c, fiber.Map{
		"status":    "ok",
		"zone_id":   zone.ID,
		"timestamp": now.Unix(),
	}, "Heartbeat acknowledged.")
}
