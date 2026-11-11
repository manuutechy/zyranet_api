package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/handlers"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
)

// registerInstallsRoutes mounts home installs (crews, jobs, cost report) and
// equipment stock. It also fences installer logins into /installs/*; call it
// before the other admin routes so the fence runs first. Role rules per
// endpoint live in handlers/installs.go and handlers/stock.go.
func registerInstallsRoutes(v1 fiber.Router) {
	v1.Use(middleware.InstallerFence("/api/v1"))

	auth := middleware.AdminAuth()
	v1.Get("/installs/options", auth, handlers.InstallOptions)
	v1.Get("/installs/summary", auth, handlers.InstallSummary)
	v1.Get("/installs/settings", auth, handlers.InstallSettingsShow)
	v1.Post("/installs/settings", auth, handlers.InstallSettingsUpdate)
	v1.Post("/installs/check-live", auth, handlers.InstallCheckLive)

	v1.Get("/installs/crews", auth, handlers.CrewIndex)
	v1.Post("/installs/crews", auth, handlers.CrewStore)
	v1.Put("/installs/crews/:id", auth, handlers.CrewUpdate)
	v1.Post("/installs/crews/:id/members", auth, handlers.CrewMemberStore)
	v1.Delete("/installs/crews/:id/members/:userId", auth, handlers.CrewMemberRemove)

	v1.Get("/installs/jobs", auth, handlers.InstallJobIndex)
	v1.Post("/installs/jobs", auth, handlers.InstallJobStore)
	v1.Get("/installs/jobs/:id", auth, handlers.InstallJobShow)
	v1.Put("/installs/jobs/:id", auth, handlers.InstallJobUpdate)
	v1.Post("/installs/jobs/:id/schedule", auth, handlers.InstallJobSchedule)
	v1.Post("/installs/jobs/:id/assign", auth, handlers.InstallJobAssign)
	v1.Post("/installs/jobs/:id/installed", auth, handlers.InstallJobInstalled)
	v1.Post("/installs/jobs/:id/cancel", auth, handlers.InstallJobCancel)

	v1.Get("/stock/items", auth, handlers.StockItemIndex)
	v1.Post("/stock/items", auth, handlers.StockItemStore)
	v1.Put("/stock/items/:id", auth, handlers.StockItemUpdate)
	v1.Get("/stock/items/:id/moves", auth, handlers.StockMoves)
	v1.Post("/stock/items/:id/purchase", auth, handlers.StockPurchase)
	v1.Post("/stock/items/:id/adjust", auth, handlers.StockChange(models.MoveAdjustment))
	v1.Post("/stock/items/:id/write-off", auth, handlers.StockChange(models.MoveWriteOff))
	v1.Post("/stock/items/:id/return", auth, handlers.StockChange(models.MoveReturned))
	v1.Get("/stock/onts", auth, handlers.OntIndex)
	v1.Post("/stock/onts/:id/faulty", auth, handlers.OntActionHandler("faulty"))
	v1.Post("/stock/onts/:id/take-back", auth, handlers.OntActionHandler("take_back"))
	v1.Post("/stock/onts/:id/good", auth, handlers.OntActionHandler("good"))
}
