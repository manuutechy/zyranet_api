package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// insightsMaxDays caps one report so a single request can't scan years of rows.
const insightsMaxDays = 400

// ReportInsights is the dashboard's detailed charts for a range of EAT days:
// money, payment success, failure reasons, packages, busy hours, buyers and
// router health. ?date_from=YYYY-MM-DD|all &date_to=YYYY-MM-DD &zone_id=
// Money, so super_admin, finance and zone_manager (own zone) only.
func ReportInsights(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}
	zoneID, ok := opsZoneScope(c, claims)
	if !ok {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	scope := services.InsightsScope{OrganizationID: claims.OrganizationID, ZoneID: zoneID, OnlyZoneIDs: areaOnlyZones(c)}

	now := time.Now()
	today := now.In(services.EAT)
	to, err := time.ParseInLocation("2006-01-02", c.Query("date_to", today.Format("2006-01-02")), services.EAT)
	if err != nil {
		return utils.ErrorResponse(c, "date_to must be YYYY-MM-DD.", "", fiber.StatusUnprocessableEntity)
	}
	fromQ := c.Query("date_from", to.AddDate(0, 0, -29).Format("2006-01-02"))
	var from time.Time
	if fromQ == "all" {
		// From the first paid sale (or 30 days if there is none yet).
		first, ferr := services.BuildInsights(config.DB, scope, to, to, now)
		if ferr != nil {
			return utils.ErrorResponse(c, "Could not load the report.", "", fiber.StatusInternalServerError)
		}
		from = to.AddDate(0, 0, -29)
		if first.FirstSale != "" {
			from, _ = time.ParseInLocation("2006-01-02", first.FirstSale, services.EAT)
		}
	} else if from, err = time.ParseInLocation("2006-01-02", fromQ, services.EAT); err != nil {
		return utils.ErrorResponse(c, "date_from must be YYYY-MM-DD or all.", "", fiber.StatusUnprocessableEntity)
	}
	if from.After(to) {
		return utils.ErrorResponse(c, "date_from must be on or before date_to.", "", fiber.StatusUnprocessableEntity)
	}
	if to.Sub(from).Hours()/24 >= insightsMaxDays {
		from = to.AddDate(0, 0, -(insightsMaxDays - 1))
	}

	out, err := services.BuildInsights(config.DB, scope, from, to, now)
	if err != nil {
		return utils.ErrorResponse(c, "Could not load the report.", "", fiber.StatusInternalServerError)
	}
	c.Set("Cache-Control", "no-store")
	return utils.SuccessResponse(c, out, "")
}
