package handlers

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// opsZoneScope resolves which zone (if any) the caller is limited to: a
// zone_manager always to their own; anyone else to ?zone_id= if it belongs
// to their ISP. ok=false means the requested zone isn't theirs.
func opsZoneScope(c *fiber.Ctx, claims *middleware.Claims) (*uint, bool) {
	if claims.Role == "zone_manager" {
		if claims.ZoneID == nil {
			return nil, false
		}
		z := *claims.ZoneID
		return &z, true
	}
	q := strings.TrimSpace(c.Query("zone_id"))
	if q == "" || q == "all" {
		return nil, true
	}
	id, err := strconv.ParseUint(q, 10, 64)
	if err != nil {
		return nil, false
	}
	var n int64
	config.DB.Model(&models.Zone{}).Where("id = ? AND organization_id = ?", id, claims.OrganizationID).Count(&n)
	if n == 0 {
		return nil, false
	}
	z := uint(id)
	return &z, true
}

// OpsLive is the dashboard's live "money today" section: takings vs
// yesterday and last week, buyers, failure reasons, who's online, router
// status, hourly sales, last payments and paid-but-not-connected. Money, so
// super_admin, finance and zone_manager (own zone) only.
func OpsLive(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}
	zoneID, ok := opsZoneScope(c, claims)
	if !ok {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	live, err := services.BuildLiveOps(config.DB, services.LiveScope{OrganizationID: claims.OrganizationID, ZoneID: zoneID}, time.Now())
	if err != nil {
		return utils.ErrorResponse(c, "Could not load live figures.", "", fiber.StatusInternalServerError)
	}
	c.Set("Cache-Control", "no-store")
	return utils.SuccessResponse(c, live, "")
}

// OpsAlertsIndex feeds the in-admin router alert banner: routers offline
// now plus un-dismissed owner alerts from the last 24 h. Router status
// isn't money, so every staff role sees it (zone managers: own zone).
func OpsAlertsIndex(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}
	var zoneID *uint
	if claims.Role == "zone_manager" {
		if claims.ZoneID == nil {
			return utils.SuccessResponse(c, fiber.Map{"offline": []interface{}{}, "alerts": []interface{}{}}, "")
		}
		zoneID = claims.ZoneID
	}
	now := time.Now()
	alerts, err := services.OpsAlertsForBanner(claims.OrganizationID, zoneID, now)
	if err != nil {
		return utils.ErrorResponse(c, "Could not load alerts.", "", fiber.StatusInternalServerError)
	}

	var zones []models.Zone
	q := config.DB.Select("id", "name", "last_seen_at").
		Where("organization_id = ? AND status = ? AND last_seen_at IS NOT NULL AND last_seen_at < ?",
			claims.OrganizationID, "active", now.Add(-services.RouterOfflineAfter))
	if zoneID != nil {
		q = q.Where("id = ?", *zoneID)
	}
	q.Order("last_seen_at ASC").Find(&zones)
	offline := make([]fiber.Map, 0, len(zones))
	for _, z := range zones {
		offline = append(offline, fiber.Map{
			"zone_id":        z.ID,
			"name":           z.Name,
			"last_seen_at":   z.LastSeenAt,
			"last_seen_eat":  z.LastSeenAt.In(services.EAT).Format("2 Jan 15:04"),
			"outage_seconds": int64(now.Sub(*z.LastSeenAt).Seconds()),
		})
	}
	return utils.SuccessResponse(c, fiber.Map{"offline": offline, "alerts": alerts}, "")
}

// OpsAlertAck dismisses one alert from the banner (for the whole ISP).
func OpsAlertAck(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}
	q := config.DB.Model(&models.OpsAlert{}).
		Where("id = ? AND organization_id = ? AND acknowledged_at IS NULL", c.Params("id"), claims.OrganizationID)
	if claims.Role == "zone_manager" {
		if claims.ZoneID == nil {
			return utils.ErrorResponse(c, "Alert not found.", "", fiber.StatusNotFound)
		}
		q = q.Where("zone_id = ?", *claims.ZoneID)
	}
	now := time.Now()
	res := q.Updates(map[string]interface{}{"acknowledged_at": now, "acknowledged_by": claims.UserID})
	if res.Error != nil {
		return utils.ErrorResponse(c, "Could not dismiss alert.", "", fiber.StatusInternalServerError)
	}
	if res.RowsAffected == 0 {
		return utils.ErrorResponse(c, "Alert not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, fiber.Map{"id": c.Params("id")}, "Alert dismissed.")
}

func opsSettingsView(s models.OpsAlertSetting) fiber.Map {
	return fiber.Map{
		"sms_enabled": s.SmsEnabled,
		"phones":      services.SplitAlertPhones(s.Phones),
		"quiet_hours": "23:00-06:00 EAT",
		"rules": []string{
			"One SMS when a router goes offline (no check-in for 5 minutes) and one when it is back.",
			"A router that keeps dropping gets at most one 'down' SMS every 2 hours.",
			"Nothing is sent 23:00-06:00 EAT; one summary SMS goes at 06:00.",
			"Deleted, disabled and never-connected routers are never alerted.",
		},
		"updated_at": s.UpdatedAt,
	}
}

// OpsAlertSettingsShow returns the ISP's router-alert SMS settings.
func OpsAlertSettingsShow(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var s models.OpsAlertSetting
	config.DB.Where("organization_id = ?", claims.OrganizationID).First(&s)
	return utils.SuccessResponse(c, opsSettingsView(s), "")
}

// OpsAlertSettingsUpdate saves who gets router alerts by SMS (up to 3
// Kenyan mobile numbers) and whether SMS is on. Default: off, no phones.
func OpsAlertSettingsUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var body struct {
		SmsEnabled bool     `json:"sms_enabled"`
		Phones     []string `json:"phones"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	phones, err := services.NormalizeAlertPhones(body.Phones)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "", fiber.StatusUnprocessableEntity)
	}
	if body.SmsEnabled && len(phones) == 0 {
		return utils.ErrorResponse(c, "Add at least one phone number to switch SMS alerts on.", "", fiber.StatusUnprocessableEntity)
	}
	var s models.OpsAlertSetting
	config.DB.Where("organization_id = ?", claims.OrganizationID).First(&s)
	s.OrganizationID = claims.OrganizationID
	s.SmsEnabled = body.SmsEnabled
	s.Phones = strings.Join(phones, ",")
	uid := claims.UserID
	s.UpdatedBy = &uid
	if err := config.DB.Save(&s).Error; err != nil {
		return utils.ErrorResponse(c, "Could not save alert settings.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, opsSettingsView(s), "Router alert settings saved.")
}
