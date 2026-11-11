package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/zyranet/zyranet-api/config"
)

func TestInstallerFence(t *testing.T) {
	old := config.Config.JWTSecret
	config.Config.JWTSecret = "fence-test-secret"
	defer func() { config.Config.JWTSecret = old }()

	app := fiber.New()
	v1 := app.Group("/api/v1")
	v1.Use(InstallerFence("/api/v1"))
	v1.All("/*", func(c *fiber.Ctx) error { return c.SendStatus(200) })

	tok := func(id uint, role string) string {
		s, _ := signToken(Claims{UserID: id, Role: role, OrganizationID: 1, Type: "admin",
			RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}})
		return s
	}
	installer, owner := tok(5, "installer"), tok(1, "super_admin")
	cases := []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/api/v1/installs/jobs", installer, 200},
		{"POST", "/api/v1/installs/jobs/3/installed", installer, 200},
		{"GET", "/api/v1/auth/me", installer, 200},
		{"GET", "/api/v1/settings", installer, 200},
		{"POST", "/api/v1/settings", installer, 403},
		{"GET", "/api/v1/customers", installer, 403},
		{"GET", "/api/v1/payments", installer, 403},
		{"GET", "/api/v1/stock/items", installer, 403},
		{"GET", "/api/v1/zones", installer, 403},
		{"GET", "/api/v1/customers", owner, 200},
		{"GET", "/api/v1/platform/auth/me", installer, 200},
		{"POST", "/api/v1/hotspot/pay", installer, 200},
		{"GET", "/api/v1/public/settings", installer, 200},
		{"GET", "/api/v1/payments", installer, 403},
		{"POST", "/api/v1/payments/manual", installer, 403},
		{"GET", "/api/v1/customers", "", 200}, // no session: AdminAuth decides
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != c.want {
			t.Errorf("%s %s (%v): got %d want %d", c.method, c.path, c.token != "", resp.StatusCode, c.want)
		}
	}
}
