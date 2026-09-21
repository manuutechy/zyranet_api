package middleware

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/utils"
)

// Claims holds the JWT payload fields.
type Claims struct {
	UserID         uint   `json:"user_id,omitempty"`
	CustomerID     uint   `json:"customer_id,omitempty"`
	PlatformUserID uint   `json:"platform_user_id,omitempty"`
	Role           string `json:"role,omitempty"`
	ZoneID         *uint  `json:"zone_id,omitempty"`
	OrganizationID uint   `json:"organization_id,omitempty"`
	Type           string `json:"type"` // "admin", "customer", or "platform"
	// AuthMethod is "device" for a customer session granted from a MAC address
	// alone (see handlers.CustomerAuthByDevice). A MAC is visible to anyone on
	// the same hotspot and trivial to spoof, so such a session is "weak": it
	// can look at the account but not change or spend from it
	// (RequireStrongCustomerAuth). Empty for sessions proven by OTP or payment.
	AuthMethod string `json:"auth_method,omitempty"`
	jwt.RegisteredClaims
}

const (
	AdminCookieName    = "zyra_admin_token"
	CustomerCookieName = "zyra_customer_token"
	PlatformCookieName = "zyra_platform_token"
)

// SetAuthCookie writes an httpOnly session cookie carrying the JWT. The
// cookie's Domain is shared across admin./portal./captive./api. subdomains
// in production (via COOKIE_DOMAIN) so all can read a session set by the API.
// SameSite=None is required for the captive portal (captive.zyranet.co.ke)
// to include the cookie on cross-origin fetch requests to api.zyranet.co.ke.
// Secure must be true whenever SameSite=None (enforced by browsers).
func SetAuthCookie(c *fiber.Ctx, name, token string) {
	isProduction := config.Config.AppEnv != "local"
	sameSite := "Lax"
	if isProduction {
		sameSite = "None"
	}
	c.Cookie(&fiber.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		Domain:   config.Config.CookieDomain,
		Expires:  time.Now().Add(config.Config.JWTExpiry),
		HTTPOnly: true,
		Secure:   isProduction,
		SameSite: sameSite,
	})
}

// ClearAuthCookie deletes a previously-set auth cookie on logout.
func ClearAuthCookie(c *fiber.Ctx, name string) {
	isProduction := config.Config.AppEnv != "local"
	sameSite := "Lax"
	if isProduction {
		sameSite = "None"
	}
	c.Cookie(&fiber.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Domain:   config.Config.CookieDomain,
		Expires:  time.Now().Add(-time.Hour),
		HTTPOnly: true,
		Secure:   isProduction,
		SameSite: sameSite,
	})
}

// AdminAuth validates the JWT token for admin panel users.
func AdminAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, err := extractAndValidate(c, AdminCookieName)
		if err != nil {
			return utils.ErrorResponse(c, "Unauthenticated.", "Invalid or missing token.", fiber.StatusUnauthorized)
		}
		if claims.Type != "admin" {
			return utils.ErrorResponse(c, "Forbidden.", "Admin access required.", fiber.StatusForbidden)
		}
		// On an ISP's own subdomain only that ISP's staff may act. The session
		// cookie is shared across *.BASE_DOMAIN, so without this a login from
		// one ISP's host would also work on another's.
		if sub, orgID, _, ok := TenantFromRequest(c); sub != "" && (!ok || orgID != claims.OrganizationID) {
			return utils.ErrorResponse(c, "Forbidden.", "This session does not belong to this ISP portal.", fiber.StatusForbidden)
		}
		c.Locals("claims", claims)
		c.Locals("userID", claims.UserID)
		c.Locals("role", claims.Role)
		c.Locals("zoneID", claims.ZoneID)
		c.Locals("organizationID", claims.OrganizationID)
		return c.Next()
	}
}

// PlatformAuth validates the JWT token for Super Admin (SA) platform users.
// It is deliberately separate from AdminAuth/CustomerAuth so a platform
// credential is never reachable through the per-ISP admin or customer
// login flows.
func PlatformAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, err := extractAndValidate(c, PlatformCookieName)
		if err != nil {
			return utils.ErrorResponse(c, "Unauthenticated.", "Invalid or missing token.", fiber.StatusUnauthorized)
		}
		if claims.Type != "platform" {
			return utils.ErrorResponse(c, "Forbidden.", "Platform access required.", fiber.StatusForbidden)
		}
		c.Locals("claims", claims)
		c.Locals("platformUserID", claims.PlatformUserID)
		return c.Next()
	}
}

// CustomerAuth validates the JWT token for customer portal users.
func CustomerAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		claims, err := extractAndValidate(c, CustomerCookieName)
		if err != nil {
			return utils.ErrorResponse(c, "Unauthenticated.", "Invalid or missing token.", fiber.StatusUnauthorized)
		}
		if claims.Type != "customer" {
			return utils.ErrorResponse(c, "Forbidden.", "Customer access required.", fiber.StatusForbidden)
		}
		c.Locals("claims", claims)
		c.Locals("customerID", claims.CustomerID)
		return c.Next()
	}
}

// GenerateAdminToken creates a signed JWT for an admin user.
func GenerateAdminToken(userID uint, role string, zoneID *uint, organizationID uint) (string, error) {
	claims := Claims{
		UserID:         userID,
		Role:           role,
		ZoneID:         zoneID,
		OrganizationID: organizationID,
		Type:           "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(config.Config.JWTExpiry)),
		},
	}
	return signToken(claims)
}

// GeneratePlatformToken creates a signed JWT for a Super Admin platform user.
func GeneratePlatformToken(platformUserID uint) (string, error) {
	claims := Claims{
		PlatformUserID: platformUserID,
		Type:           "platform",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(config.Config.JWTExpiry)),
		},
	}
	return signToken(claims)
}

// GenerateCustomerToken creates a signed JWT for a customer portal user.
func GenerateCustomerToken(customerID uint) (string, error) {
	claims := Claims{
		CustomerID: customerID,
		Type:       "customer",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(config.Config.JWTExpiry)),
		},
	}
	return signToken(claims)
}

// GenerateDeviceCustomerToken creates a customer session that was granted from
// a recognised MAC address only — a weak session, see Claims.AuthMethod.
func GenerateDeviceCustomerToken(customerID uint) (string, error) {
	claims := Claims{
		CustomerID: customerID,
		Type:       "customer",
		AuthMethod: "device",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(config.Config.JWTExpiry)),
		},
	}
	return signToken(claims)
}

// RequireStrongCustomerAuth rejects a customer session that came only from
// MAC-address recognition. Put it after CustomerAuth on anything that changes
// the account (contact details) or spends from it (credit).
func RequireStrongCustomerAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if claims, ok := c.Locals("claims").(*Claims); ok && claims.AuthMethod == "device" {
			return utils.ErrorResponse(c, "Please verify your phone number with a code to do this.", "Verification required.", fiber.StatusForbidden)
		}
		return c.Next()
	}
}

func signToken(claims Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(config.Config.JWTSecret))
}

// extractToken reads the JWT from the expected session cookie first, falling
// back to a Bearer Authorization header for any non-browser API consumers.
func extractToken(c *fiber.Ctx, cookieName string) string {
	if tok := c.Cookies(cookieName); tok != "" {
		return tok
	}
	authHeader := c.Get("Authorization")
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func extractAndValidate(c *fiber.Ctx, cookieName string) (*Claims, error) {
	tokenStr := extractToken(c, cookieName)
	if tokenStr == "" {
		return nil, fiber.ErrUnauthorized
	}

	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fiber.ErrUnauthorized
		}
		return []byte(config.Config.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		return nil, fiber.ErrUnauthorized
	}
	return claims, nil
}

// OptionalCustomerClaims returns the parsed customer claims from the
// customer session cookie (or Bearer header) if present and valid, or nil
// otherwise. Unlike CustomerAuth, it never fails the request — callers use
// it to prefer an authenticated customer's identity when a session happens
// to be present while still allowing fully anonymous access (e.g.
// MpesaStkPush's first-time/no-session purchase flow).
func OptionalCustomerClaims(c *fiber.Ctx) *Claims {
	claims, err := extractAndValidate(c, CustomerCookieName)
	if err != nil || claims.Type != "customer" {
		return nil
	}
	return claims
}

// GetClaims is a helper to retrieve claims from Fiber context.
func GetClaims(c *fiber.Ctx) *Claims {
	if v := c.Locals("claims"); v != nil {
		if claims, ok := v.(*Claims); ok {
			return claims
		}
	}
	return nil
}
