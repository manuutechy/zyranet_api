package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/handlers"
	"github.com/zyranet/zyranet-api/middleware"
)

// registerAreaRoutes mounts areas (groups of zones), their price lists,
// money by area and area-manager assignments. Owner-only checks for changes
// are in the handlers; reads are scoped to what the caller may see.
func registerAreaRoutes(admin fiber.Router, adminAuth fiber.Handler) {
	seesMoney := middleware.RequireRoles("super_admin", "finance", "zone_manager")
	managesPackages := middleware.RequireRoles("super_admin", "zone_manager")

	admin.Get("/areas", adminAuth, handlers.AreaIndex)
	admin.Post("/areas", adminAuth, handlers.AreaStore)
	admin.Get("/areas/:id", adminAuth, handlers.AreaShow)
	admin.Put("/areas/:id", adminAuth, handlers.AreaUpdate)
	admin.Delete("/areas/:id", adminAuth, handlers.AreaDestroy)
	admin.Post("/areas/:id/zones", adminAuth, handlers.AreaAssignZones)
	admin.Delete("/areas/:id/zones/:zoneId", adminAuth, handlers.AreaRemoveZone)
	admin.Get("/areas/:id/plans", adminAuth, handlers.AreaPlanIndex)
	admin.Post("/areas/:id/plans", adminAuth, handlers.AreaPlanStore)
	admin.Put("/areas/:id/plans/:planId", adminAuth, handlers.AreaPlanUpdate)
	admin.Delete("/areas/:id/plans/:planId", adminAuth, handlers.AreaPlanDestroy)
	admin.Get("/areas/:id/apply-preview", adminAuth, handlers.AreaApplyPreview)
	admin.Post("/areas/:id/apply", adminAuth, handlers.AreaApply)

	admin.Post("/packages/:id/use-area-price", adminAuth, managesPackages, handlers.PackageUseAreaPrice)
	admin.Get("/reports/areas", adminAuth, seesMoney, handlers.ReportAreas)

	admin.Get("/user-areas", adminAuth, handlers.UserAreasIndex)
	admin.Get("/users/:id/areas", adminAuth, handlers.UserAreasShow)
	admin.Put("/users/:id/areas", adminAuth, handlers.UserAreasUpdate)
}
