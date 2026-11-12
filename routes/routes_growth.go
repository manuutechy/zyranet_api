package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/handlers"
	"github.com/zyranet/zyranet-api/middleware"
)

// registerGrowthRoutes mounts Reports › Growth and Network › Watch. Called
// before the /zones/:id routes so /zones/watch isn't read as a zone id.
func registerGrowthRoutes(admin fiber.Router, adminAuth fiber.Handler) {
	// The growth plan is ISP-wide money: owner and finance see it, only the owner edits it.
	admin.Get("/reports/growth", adminAuth, middleware.RequireRoles("super_admin", "finance"), handlers.ReportGrowth)
	admin.Put("/reports/growth/settings", adminAuth, middleware.IsSuperAdmin(), handlers.ReportGrowthSettings)

	// Network: owner and zone managers (own zone only).
	fleet := middleware.RequireRoles("super_admin", "zone_manager")
	admin.Get("/zones/watch", adminAuth, fleet, handlers.ZonesWatch)
	admin.Get("/network/live", adminAuth, fleet, handlers.NetworkLive)
	admin.Get("/zones/:id/watch", adminAuth, fleet, handlers.ZoneWatchSeries)
	admin.Put("/zones/:id/watch-settings", adminAuth, fleet, handlers.ZoneWatchSettings)
}
