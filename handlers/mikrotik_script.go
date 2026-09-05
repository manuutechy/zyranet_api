package handlers

import (
	"fmt"
	"strings"
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
	gatewayIP := "10.5.50.1"
	if zone.HotspotAddress != "" {
		gatewayIP = strings.TrimSpace(strings.Split(zone.HotspotAddress, "/")[0])
	}
	redirectURL := fmt.Sprintf(
		"%s/?zone=%d&mac=$(mac)&ip=$(ip)&link-login=http://%s/login&link-orig=$(link-orig-esc)#/dashboard",
		portalHost, zone.ID, gatewayIP,
	)

	html := fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Zyra Net</title><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="preconnect" href="https://captive.zyranet.co.ke" crossorigin><link rel="dns-prefetch" href="https://captive.zyranet.co.ke"><link rel="preconnect" href="https://api.zyranet.co.ke" crossorigin><link rel="dns-prefetch" href="https://api.zyranet.co.ke"><meta http-equiv="refresh" content="0; url=%s"><script>window.location.replace(%q);</script><style>html,body{background:#0b0f19;color:#fff;margin:0;height:100%%;display:flex;align-items:center;justify-content:center;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;overflow:hidden;}.sh{text-align:center;padding:24px;}.spin{width:44px;height:44px;border:3px solid rgba(255,255,255,0.12);border-top-color:#0ea5e9;border-radius:50%%;animation:s .65s linear infinite;margin:0 auto 16px;box-shadow:0 0 16px rgba(14,165,233,0.35);}@keyframes s{to{transform:rotate(360deg)}}h1{font-size:16px;font-weight:600;letter-spacing:-0.02em;margin-bottom:6px;}p{font-size:13px;color:#94a3b8;}</style></head><body><div class="sh"><div class="spin"></div><h1>Connecting to Zyra Net</h1><p>Launching high-speed sign in...</p></div><script>window.location.replace(%q);</script></body></html>`, redirectURL, redirectURL, redirectURL)

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
	updates := map[string]interface{}{
		"last_seen_at": &now,
		"last_status":  "online",
	}

	// Update router IP if it was unconfigured or a private placeholder (learn real public IP)
	clientIP := c.IP()
	if clientIP != "" && clientIP != "127.0.0.1" && (zone.RouterIP == "" || zone.RouterIP == "10.100.0.1" || strings.HasPrefix(zone.RouterIP, "10.") || strings.HasPrefix(zone.RouterIP, "192.168.")) {
		updates["router_ip"] = clientIP
	}

	board := strings.TrimSpace(c.Query("board"))
	if board != "" && (zone.RouterName == "" || zone.RouterName == "Default Router") {
		updates["router_name"] = board
	}

	config.DB.Model(&zone).Updates(updates)

	// Ingest live telemetry metrics if sent in query string
	cpuLoad := c.QueryInt("cpu", -1)
	totalMemBytes := c.QueryInt("totalmem", 0)
	freeMemBytes := c.QueryInt("freemem", 0)
	clients := c.QueryInt("clients", 0)

	if cpuLoad >= 0 || totalMemBytes > 0 || clients > 0 {
		memTotalMB := totalMemBytes / (1024 * 1024)
		memUsedMB := (totalMemBytes - freeMemBytes) / (1024 * 1024)
		if cpuLoad < 0 {
			cpuLoad = 0
		}
		stat := models.ZoneStat{
			ZoneID:           zone.ID,
			CPULoad:          cpuLoad,
			MemoryUsedMB:     memUsedMB,
			MemoryTotalMB:    memTotalMB,
			ConnectedClients: clients,
			RecordedAt:       now,
		}
		config.DB.Create(&stat)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"status":    "ok",
		"zone_id":   zone.ID,
		"timestamp": now.Unix(),
	}, "Heartbeat acknowledged.")
}

// PublicZoneSync generates dynamic RouterOS commands to sync active packages, users, and drop expired sessions.
func PublicZoneSync(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return c.SendString("# Zone not found\n")
	}

	content, err := scriptSvc.GenerateSyncScript(zone.ID)
	if err != nil {
		return c.SendString("# " + err.Error() + "\n")
	}

	c.Set("Content-Type", "text/plain; charset=utf-8")
	return c.SendString(content)
}

