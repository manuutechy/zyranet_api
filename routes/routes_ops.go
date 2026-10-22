package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/handlers"
	"github.com/zyranet/zyranet-api/middleware"
)

// registerOpsRoutes mounts the live-ops endpoints (live money view, owner
// router alerts). Kept in its own file so it merges cleanly with other
// branches touching routes.go.
func registerOpsRoutes(admin fiber.Router, adminAuth fiber.Handler) {
	// Money: owner, finance, zone manager (own zone). Never field agents.
	seesMoney := middleware.RequireRoles("super_admin", "finance", "zone_manager")
	admin.Get("/ops/live", adminAuth, seesMoney, handlers.OpsLive)

	// Router alert banner: every staff role (zone managers see their zone).
	admin.Get("/ops/alerts", adminAuth, handlers.OpsAlertsIndex)
	admin.Post("/ops/alerts/:id/ack", adminAuth, middleware.RequireRoles("super_admin", "zone_manager"), handlers.OpsAlertAck)

	// Who gets router alerts by SMS: owner only.
	admin.Get("/ops/alert-settings", adminAuth, middleware.IsSuperAdmin(), handlers.OpsAlertSettingsShow)
	admin.Post("/ops/alert-settings", adminAuth, middleware.IsSuperAdmin(), handlers.OpsAlertSettingsUpdate)
}
