package routes

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/handlers"
	"github.com/zyranet/zyranet-api/middleware"
)

// payLimiter throttles endpoints that trigger STK pushes / SMS sends, since
// each request costs real money (Daraja) or has a per-message cost (AT SMS).
func payLimiter(max int, window time.Duration) fiber.Handler {
	// Bypass rate limiting in local/test environment to avoid blocking test login/OTP requests
	if config.Config.AppEnv == "local" {
		return func(c *fiber.Ctx) error {
			return c.Next()
		}
	}
	return limiter.New(limiter.Config{
		Max:        max,
		Expiration: window,
		KeyGenerator: func(c *fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"success": false,
				"error":   "Too many requests. Please wait a moment and try again.",
				"message": "Rate limit exceeded.",
			})
		},
	})
}

// Register mounts all API routes on the Fiber app.
func Register(app *fiber.App) {
	v1 := app.Group("/api/v1")

	// ---- PUBLIC ROUTES (no auth) ----

	// Admin auth
	v1.Post("/auth/login", payLimiter(10, time.Minute), handlers.Login)

	// Customer portal auth
	v1.Post("/customer/auth/otp", payLimiter(3, time.Minute), handlers.RequestOtp)
	v1.Post("/customer/auth/verify", payLimiter(10, time.Minute), handlers.VerifyOtp)
	v1.Post("/customer/auth/guest", handlers.CustomerAuthGuest)
	v1.Post("/customer/auth/logout", handlers.CustomerLogout)

	// Public settings & packages
	v1.Get("/public/settings", handlers.SettingsPublic)
	v1.Get("/public/packages", handlers.PackagePublic)
	v1.Get("/payments/:id/invoice", handlers.PaymentInvoice)

	// M-Pesa STK push & callback
	v1.Post("/mpesa/stkpush", payLimiter(5, time.Minute), handlers.MpesaStkPush)
	v1.Post("/mpesa/callback", handlers.MpesaCallback)

	// Payment status check
	v1.Get("/payments/:id", handlers.PaymentShow)

	// Voucher redemption (public captive portal flow)
	v1.Post("/vouchers/redeem", handlers.VoucherRedeem)

	// Support tickets (public guest submission)
	v1.Post("/public/tickets", handlers.TicketStorePublic)

	// Hotspot Captive Portal Routes
	v1.Post("/hotspot/pay", payLimiter(5, time.Minute), handlers.HotspotPay)
	v1.Get("/hotspot/status/:reference", handlers.HotspotStatus)
	v1.Get("/hotspot/session", handlers.HotspotSession)
	v1.Post("/hotspot/logout", handlers.HotspotLogout)

	// ---- CUSTOMER JWT ROUTES ----
	customerAuth := middleware.CustomerAuth()
	v1.Get("/customer/profile", customerAuth, handlers.CustomerProfile)
	v1.Get("/customer/payments", customerAuth, handlers.CustomerAuthPayments)
	v1.Post("/customer/vouchers/redeem", customerAuth, handlers.VoucherRedeemAuthenticated)
	v1.Post("/customer/reconnect", customerAuth, handlers.CustomerReconnect)
	v1.Get("/customer/tickets", customerAuth, handlers.TicketCustomerList)
	v1.Post("/customer/tickets", customerAuth, handlers.TicketStoreCustomer)
	v1.Post("/customer/topup", customerAuth, handlers.CustomerTopUp)
	v1.Post("/customer/purchase-credit", customerAuth, handlers.CustomerPurchaseWithCredit)

	// ---- ADMIN JWT ROUTES ----
	// NOTE: AdminAuth is applied per-route (not via Group(prefix, middleware)).
	// Fiber's Group(prefix, handlers...) mounts those handlers as a blanket
	// Use() at that prefix, which would intercept EVERY request under
	// /api/v1 — including typos and unmatched paths — before they ever reach
	// the global 404 handler, since Use-middleware runs on prefix match alone.
	admin := v1.Group("")
	adminAuth := middleware.AdminAuth()

	// Auth
	admin.Post("/auth/logout", adminAuth, handlers.Logout)
	admin.Get("/auth/me", adminAuth, handlers.Me)

	// Zone alerts (special top-level routes)
	admin.Get("/zones/alerts", adminAuth, handlers.ZoneAlerts)
	admin.Post("/zones/alerts/:id/resolve", adminAuth, handlers.ZoneResolveAlert)

	// Zones
	admin.Get("/zones", adminAuth, handlers.ZoneIndex)
	admin.Post("/zones", adminAuth, handlers.ZoneStore)
	admin.Get("/zones/:id", adminAuth, handlers.ZoneShow)
	admin.Put("/zones/:id", adminAuth, handlers.ZoneUpdate)
	admin.Delete("/zones/:id", adminAuth, handlers.ZoneDestroy)
	admin.Get("/zones/:id/script", adminAuth, handlers.MikroTikScriptGenerate)
	admin.Get("/zones/:id/status", adminAuth, handlers.ZoneStatus)
	admin.Post("/zones/:id/test-connection", adminAuth, handlers.ZoneTestConnection)
	admin.Post("/zones/:id/push-config", adminAuth, handlers.ZonePushConfig)
	admin.Post("/zones/:id/disconnect-client", adminAuth, handlers.ZoneDisconnectClient)
	admin.Get("/zones/:id/active-sessions", adminAuth, handlers.ZoneActiveSessions)
	admin.Get("/zones/:id/stats-history", adminAuth, handlers.ZoneStatsHistory)
	admin.Post("/zones/:id/exec", adminAuth, handlers.ZoneExecCommand)

	// Packages — pricing/speed changes are restricted to super_admin and zone_manager
	managesPackages := middleware.RequireRoles("super_admin", "zone_manager")
	admin.Get("/packages", adminAuth, handlers.PackageIndex)
	admin.Post("/packages", adminAuth, managesPackages, handlers.PackageStore)
	admin.Get("/packages/:id", adminAuth, handlers.PackageShow)
	admin.Put("/packages/:id", adminAuth, managesPackages, handlers.PackageUpdate)
	admin.Delete("/packages/:id", adminAuth, managesPackages, handlers.PackageDestroy)
	admin.Post("/packages/:id/duplicate", adminAuth, managesPackages, handlers.PackageDuplicate)

	// Vouchers
	admin.Get("/vouchers", adminAuth, handlers.VoucherIndex)
	admin.Post("/vouchers/generate", adminAuth, handlers.VoucherGenerate)
	admin.Get("/vouchers/:id", adminAuth, handlers.VoucherShow)
	admin.Delete("/vouchers/:id", adminAuth, handlers.VoucherDestroy)

	// Customers
	admin.Get("/customers", adminAuth, handlers.CustomerIndex)
	admin.Post("/customers", adminAuth, handlers.CustomerStore)
	admin.Get("/customers/:id", adminAuth, handlers.CustomerShow)
	admin.Put("/customers/:id", adminAuth, handlers.CustomerUpdate)
	admin.Delete("/customers/:id", adminAuth, handlers.CustomerDestroy)
	admin.Post("/customers/:id/suspend", adminAuth, handlers.CustomerSuspend)
	admin.Post("/customers/:id/activate", adminAuth, handlers.CustomerActivate)
	admin.Get("/customers/:id/payments", adminAuth, handlers.CustomerPayments_Admin)
	admin.Get("/customers/:id/sessions", adminAuth, handlers.CustomerSessions)
	admin.Post("/customers/:id/add-credit", adminAuth, handlers.CustomerAddCredit)
	admin.Get("/customers/:id/credit-logs", adminAuth, handlers.CustomerCreditLogs)

	// Payments
	admin.Get("/payments", adminAuth, handlers.PaymentIndex)
	admin.Post("/payments/manual", adminAuth, handlers.PaymentRecordManual)
	admin.Post("/payments/:id/invoice/email", adminAuth, handlers.PaymentInvoiceEmail)
	admin.Post("/payments/:id/invoice/sms", adminAuth, handlers.PaymentInvoiceSMS)

	// Reports
	admin.Get("/reports/revenue", adminAuth, handlers.ReportRevenue)
	admin.Get("/reports/vouchers", adminAuth, handlers.ReportVouchers)
	admin.Get("/reports/zones", adminAuth, handlers.ReportZones)
	admin.Get("/reports/service-types", adminAuth, handlers.ReportServiceTypes)

	// Settings
	admin.Get("/settings", adminAuth, handlers.SettingsIndex)
	admin.Post("/settings", adminAuth, handlers.SettingsUpdate)
	admin.Post("/settings/upload", adminAuth, handlers.SettingsUploadImage)
	admin.Post("/settings/test-sms", adminAuth, handlers.TestSms)

	// Users
	admin.Get("/users", adminAuth, handlers.UserIndex)
	admin.Post("/users", adminAuth, handlers.UserStore)
	admin.Get("/users/:id", adminAuth, handlers.UserShow)
	admin.Put("/users/:id", adminAuth, handlers.UserUpdate)
	admin.Delete("/users/:id", adminAuth, handlers.UserDestroy)

	// Tickets
	admin.Get("/tickets", adminAuth, handlers.TicketIndex)
	admin.Get("/tickets/:id", adminAuth, handlers.TicketShow)
	admin.Put("/tickets/:id", adminAuth, handlers.TicketUpdate)
	admin.Delete("/tickets/:id", adminAuth, handlers.TicketDestroy)
}
