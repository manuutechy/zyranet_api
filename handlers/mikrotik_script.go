package handlers

import (
	"crypto/subtle"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// zoneTokenAuthorized guards the router-facing /public/zones/* endpoints that
// hand back or accept sensitive per-zone data (setup/sync scripts contain
// live customer & voucher credentials; heartbeat writes router identity).
// Zone IDs are small sequential integers, so without this check anyone could
// iterate them and harvest every zone's credentials. The token is embedded
// by GenerateScript/GenerateSyncScript into the URLs the router itself
// fetches, so legitimate routers never need to know it separately — it's
// baked into their provisioning script once at setup time.
func zoneTokenAuthorized(c *fiber.Ctx, zone *models.Zone) bool {
	supplied := c.Query("token")
	if zone.ProvisionToken != "" && supplied != "" &&
		subtle.ConstantTimeCompare([]byte(supplied), []byte(zone.ProvisionToken)) == 1 {
		return true
	}

	// No valid token: a router still running a pre-token script, or someone
	// guessing zone ids. Record it against the zone so the platform can see
	// which routers need re-provisioning (re-run the setup one-liner), then
	// refuse. There is no way back in without the token: the legacy
	// ALLOW_LEGACY_ROUTER_REQUESTS escape hatch is gone, because with it on
	// anyone could pull any zone's sync script.
	noteLegacyRouterRequest(zone.ID)
	return false
}

// noteLegacyRouterRequest stamps the zone with the time of a token-less or
// bad-token router request. Throttled to one write per zone per 10 minutes,
// since a legacy router polls every minute.
func noteLegacyRouterRequest(zoneID uint) {
	now := time.Now()
	config.DB.Model(&models.Zone{}).
		Where("id = ? AND (legacy_request_at IS NULL OR legacy_request_at < ?)", zoneID, now.Add(-10*time.Minute)).
		UpdateColumn("legacy_request_at", now)
}

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

// ZoneProvisionToken returns the zone's router-provisioning token (authenticated,
// org-scoped) so the admin UI can embed it in the 1-liner it shows for copy/paste.
// The token itself is never included in the regular zone JSON (see Zone.ProvisionToken
// `json:"-"`) to keep it out of list/detail responses that get logged or cached more broadly.
func ZoneProvisionToken(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	if zone.ProvisionToken == "" {
		token, err := models.GenerateProvisionToken()
		if err != nil {
			return utils.ErrorResponse(c, "Failed to generate provisioning token.", "", fiber.StatusInternalServerError)
		}
		zone.ProvisionToken = token
		if err := config.DB.Model(&zone).Update("provision_token", token).Error; err != nil {
			return utils.ErrorResponse(c, "Failed to save provisioning token.", "", fiber.StatusInternalServerError)
		}
	}
	return utils.SuccessResponse(c, fiber.Map{"provision_token": zone.ProvisionToken}, "Provisioning token.")
}

// PublicZoneSetupScript serves the RouterOS .rsc script directly to MikroTik /tool fetch
func PublicZoneSetupScript(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	if !zoneTokenAuthorized(c, &zone) {
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
		"%s/?zone=%d&mac=$(mac)&ip=$(ip)&link-login=$(link-login-only)&link-orig=$(link-orig-esc)#/login",
		portalHost, zone.ID,
	)

	html := fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Zyra Net</title><meta name="viewport" content="width=device-width,initial-scale=1"><link rel="preconnect" href="https://captive.zyranet.co.ke" crossorigin><link rel="dns-prefetch" href="https://captive.zyranet.co.ke"><link rel="preconnect" href="https://api.zyranet.co.ke" crossorigin><link rel="dns-prefetch" href="https://api.zyranet.co.ke"><meta http-equiv="refresh" content="0; url=%s"><script>window.location.replace(%q);</script><style>html,body{background:#0b0f19;color:#fff;margin:0;height:100%%;display:flex;align-items:center;justify-content:center;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;overflow:hidden;}.sh{text-align:center;padding:24px;}.spin{width:44px;height:44px;border:3px solid rgba(255,255,255,0.12);border-top-color:#0ea5e9;border-radius:50%%;animation:s .65s linear infinite;margin:0 auto 16px;box-shadow:0 0 16px rgba(14,165,233,0.35);}@keyframes s{to{transform:rotate(360deg)}}h1{font-size:16px;font-weight:600;letter-spacing:-0.02em;margin-bottom:6px;}p{font-size:13px;color:#94a3b8;}</style></head><body><div class="sh"><div class="spin"></div><h1>Connecting to Zyra Net</h1><p>Launching high-speed sign in...</p></div><script>window.location.replace(%q);</script></body></html>`, redirectURL, redirectURL, redirectURL)

	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.SendString(html)
}

// PublicZoneRedirectPage serves the router's redirect.html directly to MikroTik /tool fetch
// This page is served by RouterOS right after a client successfully authenticates.
// It redirects the client to $(link-orig) or the connectivity probe so the CNA closes and grants internet.
func PublicZoneRedirectPage(c *fiber.Ctx) error {
	html := `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Connected - Zyra Net</title><meta http-equiv="refresh" content="0; url=$(link-orig)"><script>var t="$(link-orig)";if(!t||t.indexOf("10.5.50.1")!==-1||t.indexOf("captive.zyranet.co.ke")!==-1){t="http://connectivitycheck.gstatic.com/generate_204";}window.location.replace(t);</script><style>html,body{background:#0b0f19;color:#10b981;margin:0;height:100%;display:flex;align-items:center;justify-content:center;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;text-align:center;}h1{font-size:18px;margin-bottom:8px;}p{font-size:14px;color:#94a3b8;}</style></head><body><div><h1>Connected!</h1><p>Enjoy high-speed internet.</p></div></body></html>`
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.SendString(html)
}

// PublicZoneHeartbeat receives 1-minute heartbeats from active MikroTik routers.
func PublicZoneHeartbeat(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	if !zoneTokenAuthorized(c, &zone) {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}

	now := time.Now()
	// Alive: last_seen, router/public IP from this authenticated request's
	// real source (c.IP() honours CF-Connecting-IP only from trusted
	// proxies, so it can't be spoofed with a header), and outage
	// compensation if it had been away for over 10 minutes.
	services.RecordRouterCheckIn(&zone, services.RouterCheckIn{
		SourceIP:  c.IP(),
		Board:     c.Query("board"),
		Heartbeat: true,
	})

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
	if !zoneTokenAuthorized(c, &zone) {
		return c.SendString("# Zone not found\n")
	}

	content, err := scriptSvc.GenerateSyncScript(zone.ID)
	if err != nil {
		return c.SendString("# " + err.Error() + "\n")
	}
	// One small write per sync (not per customer): when the router last got
	// its logins, so the next sync knows what changed since, and the portal
	// knows when a new login has reached the router.
	now := time.Now()
	config.DB.Model(&models.Zone{}).Where("id = ?", zone.ID).UpdateColumn("last_sync_at", now)
	if zone.LastSeenAt == nil || time.Since(*zone.LastSeenAt) > services.RouterOfflineAfter {
		services.RecordRouterCheckIn(&zone, services.RouterCheckIn{SourceIP: c.IP()})
	}

	c.Set("Content-Type", "text/plain; charset=utf-8")
	return c.SendString(content)
}
