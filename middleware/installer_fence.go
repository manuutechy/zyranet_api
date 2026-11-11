package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/utils"
)

// InstallerFence keeps installer logins (field crews) inside the installs
// pages. An installer's session may only reach /auth/* (sign-in, me, logout),
// /installs/* (whose handlers scope every row to the installer's crew) and
// read the public settings; every other admin endpoint answers 403, so a
// crew phone can't list customers, payments, routers or vouchers.
//
// It reads the role from the signed session token without a database call. A
// role change bumps the user's token version (see RevokeAdminSessions), so a
// token that says "installer" belongs to someone who is still an installer;
// AdminAuth still checks the account on the routes themselves.
func InstallerFence(prefix string) fiber.Handler {
	allowed := []string{prefix + "/auth/", prefix + "/installs/", prefix + "/installs"}
	// Routes that never use the staff session (captive portal, customer app,
	// M-Pesa callbacks, router provisioning, the platform's own login). The
	// staff cookie is shared across subdomains, so an installer's phone on a
	// hotspot still sends it there; those must keep working.
	notStaff := []string{"/public/", "/customer/", "/hotspot/", "/platform/", "/c2b/", "/mpesa/",
		"/payouts/b2b/", "/payments/stkpush", "/payments/callback", "/payments/cyzora/", "/payments/verify-code", "/vouchers/redeem"}
	for i := range notStaff {
		notStaff[i] = prefix + notStaff[i]
	}
	allowed = append(allowed, notStaff...)
	return func(c *fiber.Ctx) error {
		claims, err := extractAndValidate(c, AdminCookieName)
		if err != nil || claims.Type != "admin" || claims.Role != "installer" {
			return c.Next()
		}
		path := c.Path()
		for _, p := range allowed {
			if path == p || strings.HasPrefix(path, p) {
				return c.Next()
			}
		}
		if c.Method() == fiber.MethodGet && path == prefix+"/settings" {
			return c.Next()
		}
		return utils.ErrorResponse(c, "Installer accounts can only open install jobs.", "Forbidden.", fiber.StatusForbidden)
	}
}
