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

// parseLineReport reads the Network › Watch fields of a heartbeat. Anything
// missing or malformed is nil (old scripts, or a RouterOS command that failed).
func parseLineReport(c *fiber.Ctx) services.LineReport {
	u64 := func(k string) *uint64 {
		v, err := strconv.ParseUint(strings.TrimSpace(c.Query(k)), 10, 64)
		if err != nil {
			return nil
		}
		return &v
	}
	small := func(k string, max int) *int {
		v, err := strconv.Atoi(strings.TrimSpace(c.Query(k)))
		if err != nil || v < 0 || v > max {
			return nil
		}
		return &v
	}
	var r services.LineReport
	r.RxBytes, r.TxBytes = u64("wanrx"), u64("wantx")
	if r.RxBytes == nil || r.TxBytes == nil {
		r.RxBytes, r.TxBytes = nil, nil
	}
	r.PPPoE = small("pppoe", 1000000)
	if r.HasWatchData() {
		r.Hotspot = small("clients", 1000000)
		r.CPU = small("cpu", 100)
		total, _ := strconv.ParseFloat(c.Query("totalmem"), 64)
		free, _ := strconv.ParseFloat(c.Query("freemem"), 64)
		if total > 0 && free >= 0 && free <= total {
			m := int((total-free)/total*100 + 0.5)
			r.MemPct = &m
		}
	}
	if olt := strings.TrimSpace(c.Query("olt")); olt != "" {
		if n, err := strconv.Atoi(olt); err == nil && n >= 0 {
			ok := n > 0
			r.OltOK = &ok
			// RouterOS prints avg-rtt as "1", "1ms" or "00:00:00.001".
			if ms, ok2 := parseRouterMs(c.Query("oltms")); ok2 && ok {
				r.OltMs = &ms
			}
		}
	}
	return r
}

func parseRouterMs(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if strings.Count(v, ":") == 2 {
		parts := strings.Split(v, ":")
		h, e1 := strconv.ParseFloat(parts[0], 64)
		m, e2 := strconv.ParseFloat(parts[1], 64)
		s, e3 := strconv.ParseFloat(parts[2], 64)
		if e1 != nil || e2 != nil || e3 != nil {
			return 0, false
		}
		return (h*3600 + m*60 + s) * 1000, true
	}
	v = strings.TrimSuffix(strings.TrimSuffix(v, "ms"), " ")
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 600000 {
		return 0, false
	}
	return f, true
}

func recordLineWatch(c *fiber.Ctx, zone *models.Zone, now time.Time) {
	r := parseLineReport(c)
	if !r.HasWatchData() {
		return
	}
	services.IngestLineReport(config.DB, zone, r, now)
}

// watchZoneScope: zone managers see only their zone, area managers the
// zones of their areas; nil = the whole ISP.
func watchZoneScope(c *fiber.Ctx, claims *middleware.Claims) ([]uint, bool) {
	if claims.AreaManager {
		return middleware.AreaZoneIDs(c), true
	}
	if claims.Role == "zone_manager" {
		if claims.ZoneID == nil {
			return nil, false
		}
		return []uint{*claims.ZoneID}, true
	}
	return nil, true
}

// ZonesWatch is Network › Watch: one row per router — line use now and the
// peak today, sessions, CPU/memory, OLT and last check-in.
func ZonesWatch(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	zoneIDs, ok := watchZoneScope(c, claims)
	if !ok {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	out, err := services.BuildZonesWatchIn(config.DB, claims.OrganizationID, zoneIDs, time.Now())
	if err != nil {
		return utils.ErrorResponse(c, "Could not load the routers.", "", fiber.StatusInternalServerError)
	}
	c.Set("Cache-Control", "no-store")
	return utils.SuccessResponse(c, out, "")
}

func watchZoneFor(c *fiber.Ctx) (*models.Zone, error) {
	claims := middleware.GetClaims(c)
	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, c.Params("id")).Error; err != nil {
		return nil, utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	if !middleware.ZoneInScope(c, zone.ID) || (claims.Role == "zone_manager" && !claims.AreaManager && claims.ZoneID == nil) {
		return nil, utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	return &zone, nil
}

// ZoneWatchSeries is one router's line use over the last ?hours= (1–168).
func ZoneWatchSeries(c *fiber.Ctx) error {
	zone, errResp := watchZoneFor(c)
	if zone == nil {
		return errResp
	}
	hours := c.QueryInt("hours", 24)
	if hours < 1 {
		hours = 1
	}
	if hours > 168 {
		hours = 168
	}
	out, err := services.BuildZoneWatchSeries(config.DB, zone, hours, time.Now())
	if err != nil {
		return utils.ErrorResponse(c, "Could not load the chart.", "", fiber.StatusInternalServerError)
	}
	c.Set("Cache-Control", "no-store")
	return utils.SuccessResponse(c, out, "")
}

// ZoneWatchSettings saves the zone's uplink speed and OLT address.
// Body: { uplink_down_mbps, uplink_up_mbps, olt_ip } (each optional).
func ZoneWatchSettings(c *fiber.Ctx) error {
	zone, errResp := watchZoneFor(c)
	if zone == nil {
		return errResp
	}
	var body struct {
		Down  *int    `json:"uplink_down_mbps"`
		Up    *int    `json:"uplink_up_mbps"`
		OltIP *string `json:"olt_ip"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	upd := map[string]interface{}{}
	for _, v := range []struct {
		col string
		val *int
	}{{"uplink_down_mbps", body.Down}, {"uplink_up_mbps", body.Up}} {
		if v.val == nil {
			continue
		}
		if *v.val < 0 || *v.val > 100000 {
			return utils.ErrorResponse(c, "Line speed must be between 0 and 100,000 Mbps.", "Validation failed.", fiber.StatusUnprocessableEntity)
		}
		upd[v.col] = *v.val
	}
	if body.OltIP != nil {
		ip, err := services.NormalizeOltIP(*body.OltIP)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Validation failed.", fiber.StatusUnprocessableEntity)
		}
		upd["olt_ip"] = ip
	}
	if len(upd) == 0 {
		return utils.ErrorResponse(c, "Nothing to save.", "", fiber.StatusUnprocessableEntity)
	}
	if err := config.DB.Model(&models.Zone{}).Where("id = ?", zone.ID).Updates(upd).Error; err != nil {
		return utils.ErrorResponse(c, "Could not save.", "", fiber.StatusInternalServerError)
	}
	config.DB.First(zone, zone.ID)
	return utils.SuccessResponse(c, fiber.Map{
		"uplink_down_mbps": zone.UplinkDownMbps,
		"uplink_up_mbps":   zone.UplinkUpMbps,
		"olt_ip":           zone.OltIP,
	}, "Saved. The router picks up the OLT address on its next sync (about a minute).")
}
