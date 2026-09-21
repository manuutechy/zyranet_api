package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// legacyRouterRow is one zone whose router still calls in without a valid
// provisioning token.
type legacyRouterRow struct {
	ZoneID          uint       `json:"zone_id"`
	ZoneName        string     `json:"zone_name"`
	OrganizationID  uint       `json:"organization_id"`
	Organization    string     `json:"organization"`
	RouterIP        string     `json:"router_ip"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	LegacyRequestAt time.Time  `json:"legacy_request_at"`
}

// PlatformLegacyRouters lists zones whose router has called a /public/zones/*
// endpoint without a valid provisioning token recently (default last 30 days,
// ?days=N). Those routers run a pre-token setup script and stop syncing once
// token enforcement is on; the ISP re-runs the setup command from its Zones
// page (which embeds the token) to fix each one.
func PlatformLegacyRouters(c *fiber.Ctx) error {
	days := c.QueryInt("days", 30)
	if days < 1 || days > 365 {
		days = 30
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	var zones []models.Zone
	config.DB.Preload("Organization").
		Where("legacy_request_at IS NOT NULL AND legacy_request_at >= ?", since).
		Order("legacy_request_at DESC").Find(&zones)

	rows := make([]legacyRouterRow, 0, len(zones))
	for _, z := range zones {
		row := legacyRouterRow{
			ZoneID: z.ID, ZoneName: z.Name, OrganizationID: z.OrganizationID,
			RouterIP: z.RouterIP, LastSeenAt: z.LastSeenAt, LegacyRequestAt: *z.LegacyRequestAt,
		}
		if z.Organization != nil {
			row.Organization = z.Organization.Name
		}
		rows = append(rows, row)
	}
	return utils.SuccessResponse(c, fiber.Map{
		"enforcing": !config.Config.AllowLegacyRouterRequests,
		"routers":   rows,
	}, "")
}
