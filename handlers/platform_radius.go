package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

var radiusSvcGlobal *services.RadiusService

// InitRadiusService injects the RADIUS service.
func InitRadiusService(r *services.RadiusService) { radiusSvcGlobal = r }

// PlatformRadiusStatus shows whether the RADIUS server's tables are installed
// and, per zone, which auth mode it uses and how many accounts are live.
func PlatformRadiusStatus(c *fiber.Ctx) error {
	var zones []models.Zone
	config.DB.Preload("Organization").Order("organization_id ASC, id ASC").Find(&zones)
	type row struct {
		ZoneID         uint   `json:"zone_id"`
		ZoneName       string `json:"zone_name"`
		Organization   string `json:"organization"`
		RouterIP       string `json:"router_ip"`
		AuthMode       string `json:"auth_mode"`
		ActiveAccounts int64  `json:"active_accounts"`
	}
	rows := make([]row, 0, len(zones))
	for _, z := range zones {
		var n int64
		config.DB.Model(&models.RadiusAccount{}).Where("zone_id = ? AND active = ?", z.ID, true).Count(&n)
		org := ""
		if z.Organization != nil {
			org = z.Organization.Name
		}
		mode := z.AuthMode
		if mode == "" {
			mode = "api"
		}
		rows = append(rows, row{z.ID, z.Name, org, z.RouterIP, mode, n})
	}
	return utils.SuccessResponse(c, fiber.Map{
		"installed":   services.RadiusTablesExist(),
		"server_addr": config.Config.RadiusServerAddr,
		"zones":       rows,
	}, "")
}

// PlatformZoneRadiusSet switches a zone between the API-driven model and RADIUS.
// It only prepares the server side (secret, client entry, account rows); the
// router itself is changed only when someone applies the downloaded script.
func PlatformZoneRadiusSet(c *fiber.Ctx) error {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}

	if !body.Enabled {
		radiusSvcGlobal.ClearZone(zone.ID)
		radiusSvcGlobal.RemoveNAS(zone.ID)
		config.DB.Model(&zone).Update("auth_mode", "api")
		radiusSvcGlobal.ReloadServer()
		return utils.SuccessResponse(c, fiber.Map{"zone_id": zone.ID, "auth_mode": "api"},
			"RADIUS switched off for this zone. Run the rollback script on the router to match.")
	}

	if !services.RadiusTablesExist() {
		return utils.ErrorResponse(c, services.ErrRadiusNotInstalled.Error(), "", fiber.StatusUnprocessableEntity)
	}
	if zone.RouterIP == "" {
		return utils.ErrorResponse(c, "This zone has no router address yet — the RADIUS server needs it to recognise the router.", "", fiber.StatusUnprocessableEntity)
	}
	if zone.RadiusSecret == "" {
		secret, err := services.GenerateRadiusSecret()
		if err != nil {
			return utils.ErrorResponse(c, "Could not generate a secret.", "", fiber.StatusInternalServerError)
		}
		zone.RadiusSecret = secret
	}
	if err := radiusSvcGlobal.EnsureNAS(&zone); err != nil {
		return utils.ErrorResponse(c, err.Error(), "", fiber.StatusInternalServerError)
	}
	config.DB.Model(&zone).Updates(map[string]interface{}{"auth_mode": "radius", "radius_secret": zone.RadiusSecret})
	if err := radiusSvcGlobal.SyncZone(zone.ID); err != nil {
		return utils.ErrorResponse(c, err.Error(), "", fiber.StatusInternalServerError)
	}
	reloaded := radiusSvcGlobal.ReloadServer()
	msg := "RADIUS prepared and the server restarted. Now apply the router script."
	if !reloaded {
		msg = "RADIUS prepared for this zone. Restart the RADIUS server once, then apply the router script."
	}
	return utils.SuccessResponse(c, fiber.Map{
		"zone_id": zone.ID, "auth_mode": "radius",
		// FreeRADIUS reads its client list when it (re)loads; true = a person still has to do it.
		"server_reload_needed": !reloaded,
	}, msg)
}

// PlatformZoneRadiusScript downloads the RouterOS script (?rollback=1 for the
// undo script). It contains the zone's shared secret, so it is platform-only.
func PlatformZoneRadiusScript(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	var script, name string
	if c.QueryBool("rollback", false) {
		script, name = services.GenerateRadiusRollbackScript(&zone), "radius-rollback"
	} else {
		var err error
		script, err = services.GenerateRadiusScript(&zone, config.Config.RadiusServerAddr)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "", fiber.StatusUnprocessableEntity)
		}
		name = "radius-enable"
	}
	c.Set("Content-Type", "text/plain; charset=utf-8")
	c.Set("Content-Disposition", `attachment; filename="`+name+`-zone`+c.Params("id")+`.rsc"`)
	return c.SendString(script)
}

// ZoneQueueTuningScript lets an ISP download the fair-queue tuning script for
// one of its zones, from its real uplink speed. GET /zones/:id/queue-tuning-script?down=50&up=20
func ZoneQueueTuningScript(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	script, err := services.GenerateQueueTuningScript(&zone, c.QueryInt("down", 0), c.QueryInt("up", 0))
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "", fiber.StatusUnprocessableEntity)
	}
	// Remember what they told us, so the form is pre-filled next time.
	config.DB.Model(&zone).Updates(map[string]interface{}{"uplink_down_mbps": c.QueryInt("down", 0), "uplink_up_mbps": c.QueryInt("up", 0)})
	c.Set("Content-Type", "text/plain; charset=utf-8")
	c.Set("Content-Disposition", `attachment; filename="queue-tuning-zone`+c.Params("id")+`.rsc"`)
	return c.SendString(script)
}
