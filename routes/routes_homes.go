package routes

import (
	"net/url"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/limiter"
	"github.com/zyranet/zyranet-api/handlers"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
)

// homeLimiter is a rate limit for the public home (renew page) endpoints.
// Unlike payLimiter it also applies locally, so it can be tested. key picks
// the bucket (per IP, per customer number, per phone).
func homeLimiter(name string, max int, window time.Duration, failedOnly bool, key func(*fiber.Ctx) string) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:                    max,
		Expiration:             window,
		SkipSuccessfulRequests: failedOnly,
		KeyGenerator:           func(c *fiber.Ctx) string { return "home:" + name + ":" + key(c) },
		LimitReached: func(c *fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"success": false,
				"error":   "Too many tries. Please wait a few minutes and try again.",
				"message": "Rate limit exceeded.",
			})
		},
	})
}

func byIP(c *fiber.Ctx) string { return c.IP() }

// byHomeNo buckets by the customer number however it was typed.
func byHomeNo(c *fiber.Ctx) string {
	raw := c.Params("no")
	if u, err := url.PathUnescape(raw); err == nil {
		raw = u
	}
	if no, ok := models.ParseCustomerNo(raw); ok {
		return strconv.FormatUint(uint64(no), 10)
	}
	return "invalid:" + c.IP() // typos: per IP, never one shared bucket
}

func byPhone(c *fiber.Ctx) string { return bodyPhone(c) }

// registerHomeRoutes mounts home (fibre) account endpoints: the public renew
// page API and the admin "Add a home" / switch off / switch on actions.
func registerHomeRoutes(v1 fiber.Router, admin fiber.Router, adminAuth fiber.Handler) {
	// Public renew page — no login. A whole ISP's homes (and a hotspot site)
	// can reach us from one public IP, so per-IP limits are generous; misses
	// are what stop someone walking through numbers (most guesses fail the
	// check digit). Lookups: 120/min per IP, 60 misses per 10 min per IP,
	// 20/min per number.
	lookup := []fiber.Handler{
		homeLimiter("lookup-ip", 120, time.Minute, false, byIP),
		homeLimiter("lookup-miss", 60, 10*time.Minute, true, byIP),
		homeLimiter("lookup-no", 20, time.Minute, false, byHomeNo),
	}
	v1.Get("/public/home/:no", append(lookup, handlers.HomePublicLookup)...)
	// Prompts cost money and land on someone's phone: 30/min per IP,
	// 5 per 5 min per customer number and per phone.
	v1.Post("/public/home/:no/pay",
		homeLimiter("pay-ip", 30, time.Minute, false, byIP),
		homeLimiter("pay-no", 5, 5*time.Minute, false, byHomeNo),
		homeLimiter("pay-phone", 5, 5*time.Minute, false, byPhone),
		handlers.HomePublicPay)
	// Status is polled every few seconds while the customer pays.
	v1.Get("/public/home/:no/pay/:id",
		homeLimiter("status-ip", 600, time.Minute, false, byIP),
		homeLimiter("status-miss", 60, 10*time.Minute, true, byIP),
		handlers.HomePublicPayStatus)

	// Admin. Role and zone checks are in the handlers (owner / zone manager;
	// zone managers only their own zone; always the caller's ISP).
	manages := middleware.RequireRoles("super_admin", "zone_manager")
	admin.Post("/homes", adminAuth, manages, handlers.HomeStore)
	admin.Get("/homes/:id", adminAuth, handlers.HomeShow)
	admin.Post("/homes/:id/switch-off", adminAuth, manages, handlers.HomeSwitchOff)
	admin.Post("/homes/:id/switch-on", adminAuth, manages, handlers.HomeSwitchOn)
}
