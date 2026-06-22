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
	UserID     uint   `json:"user_id,omitempty"`
	CustomerID uint   `json:"customer_id,omitempty"`
	Role       string `json:"role,omitempty"`
	ZoneID     *uint  `json:"zone_id,omitempty"`
	Type       string `json:"type"` // "admin" or "customer"
	jwt.RegisteredClaims
}

const (
	AdminCookieName    = "zyra_admin_token"
	CustomerCookieName = "zyra_customer_token"
)

// SetAuthCookie writes an httpOnly session cookie carrying the JWT. The
// cookie's Domain is shared across admin./portal./api. subdomains in
// production (via COOKIE_DOMAIN) so all three can read a session set by the API.
func SetAuthCookie(c *fiber.Ctx, name, token string) {
	c.Cookie(&fiber.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		Domain:   config.Config.CookieDomain,
		Expires:  time.Now().Add(config.Config.JWTExpiry),
		HTTPOnly: true,
		Secure:   config.Config.AppEnv != "local",
		SameSite: "Lax",
	})
}

// ClearAuthCookie deletes a previously-set auth cookie on logout.
func ClearAuthCookie(c *fiber.Ctx, name string) {
	c.Cookie(&fiber.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Domain:   config.Config.CookieDomain,
		Expires:  time.Now().Add(-time.Hour),
		HTTPOnly: true,
		Secure:   config.Config.AppEnv != "local",
		SameSite: "Lax",
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
		c.Locals("claims", claims)
		c.Locals("userID", claims.UserID)
		c.Locals("role", claims.Role)
		c.Locals("zoneID", claims.ZoneID)
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
func GenerateAdminToken(userID uint, role string, zoneID *uint) (string, error) {
	claims := Claims{
		UserID: userID,
		Role:   role,
		ZoneID: zoneID,
		Type:   "admin",
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

// GetClaims is a helper to retrieve claims from Fiber context.
func GetClaims(c *fiber.Ctx) *Claims {
	if v := c.Locals("claims"); v != nil {
		if claims, ok := v.(*Claims); ok {
			return claims
		}
	}
	return nil
}
