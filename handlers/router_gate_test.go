package handlers

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
)

// gateApp exposes zoneTokenAuthorized the way the router-facing endpoints use it.
func gateApp() *fiber.App {
	app := fiber.New()
	app.Get("/z/:id", func(c *fiber.Ctx) error {
		var zone models.Zone
		if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
			return c.SendStatus(404)
		}
		if !zoneTokenAuthorized(c, &zone) {
			return c.SendStatus(403)
		}
		return c.SendStatus(200)
	})
	return app
}

func gateStatus(t *testing.T, path string) int {
	t.Helper()
	resp, err := gateApp().Test(httptest.NewRequest("GET", path, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func seedRouterZone(t *testing.T) models.Zone {
	t.Helper()
	setupC2BTestDB(t)
	old := config.Config.AllowLegacyRouterRequests
	config.Config.AllowLegacyRouterRequests = false
	t.Cleanup(func() { config.Config.AllowLegacyRouterRequests = old })
	org := models.Organization{Name: "ISP", Slug: "isp"}
	config.DB.Create(&org)
	z := models.Zone{Name: "Z", Location: "L", RouterName: "r", RouterIP: "196.0.0.9", OrganizationID: org.ID}
	if err := config.DB.Create(&z).Error; err != nil {
		t.Fatal(err)
	}
	config.DB.First(&z, z.ID) // BeforeCreate generated the token
	return z
}

func legacyStamp(t *testing.T, id uint) *time.Time {
	t.Helper()
	var z models.Zone
	config.DB.First(&z, id)
	return z.LegacyRequestAt
}

// Regression: a request with no token at all used to be waved through, so the
// token protected nothing — any zone's setup script (with live customer and
// voucher credentials) could be read by iterating sequential zone ids.
func TestRouterGate_RequiresAValidToken(t *testing.T) {
	z := seedRouterZone(t)

	if st := gateStatus(t, fmt.Sprintf("/z/%d?token=%s", z.ID, z.ProvisionToken)); st != 200 {
		t.Errorf("valid token: %d, want 200", st)
	}
	if st := gateStatus(t, fmt.Sprintf("/z/%d", z.ID)); st != 403 {
		t.Errorf("no token: %d, want 403", st)
	}
	if st := gateStatus(t, fmt.Sprintf("/z/%d?token=", z.ID)); st != 403 {
		t.Errorf("blank token: %d, want 403", st)
	}
	if st := gateStatus(t, fmt.Sprintf("/z/%d?token=wrong", z.ID)); st != 403 {
		t.Errorf("wrong token: %d, want 403", st)
	}
	// Source IP is not a credential: a wrong token from the router's own IP is still refused.
	req := httptest.NewRequest("GET", fmt.Sprintf("/z/%d?token=wrong", z.ID), nil)
	req.Header.Set("CF-Connecting-IP", z.RouterIP)
	req.Header.Set("X-Forwarded-For", "10.200.1.1")
	if resp, _ := gateApp().Test(req, -1); resp.StatusCode != 403 {
		t.Errorf("wrong token from the router's IP: %d, want 403", resp.StatusCode)
	}
}

func TestRouterGate_RecordsLegacyRoutersAndOnlyWritesOncePerWindow(t *testing.T) {
	z := seedRouterZone(t)

	// A good request leaves no mark.
	gateStatus(t, fmt.Sprintf("/z/%d?token=%s", z.ID, z.ProvisionToken))
	if legacyStamp(t, z.ID) != nil {
		t.Fatal("a valid-token request must not mark the zone as legacy")
	}

	gateStatus(t, fmt.Sprintf("/z/%d", z.ID))
	first := legacyStamp(t, z.ID)
	if first == nil {
		t.Fatal("a token-less request should be recorded on its zone")
	}
	gateStatus(t, fmt.Sprintf("/z/%d", z.ID)) // polled again a moment later
	if second := legacyStamp(t, z.ID); !second.Equal(*first) {
		t.Error("repeat requests inside the window should not rewrite the row")
	}
}

func TestRouterGate_MigrationEscapeHatch(t *testing.T) {
	z := seedRouterZone(t)
	config.Config.AllowLegacyRouterRequests = true

	if st := gateStatus(t, fmt.Sprintf("/z/%d", z.ID)); st != 200 {
		t.Errorf("hatch on, no token: %d, want 200 (old routers keep working during migration)", st)
	}
	if legacyStamp(t, z.ID) == nil {
		t.Error("requests served through the hatch must still be recorded, so the list shows what's left to migrate")
	}
	if st := gateStatus(t, fmt.Sprintf("/z/%d?token=wrong", z.ID)); st != 403 {
		t.Errorf("hatch on, wrong token: %d, want 403 — the hatch is for missing tokens only", st)
	}
}

func TestPlatformLegacyRouters_ListsRecentOnly(t *testing.T) {
	z := seedRouterZone(t)
	old := models.Zone{Name: "Old", Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: z.OrganizationID}
	config.DB.Create(&old)
	stale := time.Now().Add(-90 * 24 * time.Hour)
	config.DB.Model(&old).UpdateColumn("legacy_request_at", stale)
	gateStatus(t, fmt.Sprintf("/z/%d", z.ID)) // z becomes legacy now

	app := fiber.New()
	app.Get("/routers/legacy", PlatformLegacyRouters)
	resp, _ := app.Test(httptest.NewRequest("GET", "/routers/legacy", nil), -1)
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	got := string(body[:n])
	if !contains(got, `"zone_name":"Z"`) || contains(got, `"zone_name":"Old"`) || !contains(got, `"enforcing":true`) {
		t.Errorf("unexpected list: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && (len(s) >= len(sub)) && indexOf(s, sub) >= 0
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---- weak (MAC-recognised) customer sessions ----

func TestWeakDeviceSession_CanReadButNotChangeOrSpend(t *testing.T) {
	setupC2BTestDB(t)
	config.Config.JWTSecret = "device-test-secret-device-test-secret"
	config.Config.JWTExpiry = time.Hour

	app := fiber.New()
	cust := app.Group("/customer", middleware.CustomerAuth())
	cust.Get("/profile", func(c *fiber.Ctx) error { return c.SendStatus(200) })
	cust.Put("/profile", middleware.RequireStrongCustomerAuth(), func(c *fiber.Ctx) error { return c.SendStatus(200) })
	cust.Post("/purchase-credit", middleware.RequireStrongCustomerAuth(), func(c *fiber.Ctx) error { return c.SendStatus(200) })

	strong, _ := middleware.GenerateCustomerToken(7)
	weak, _ := middleware.GenerateDeviceCustomerToken(7)
	do := func(method, path, token string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	for _, tc := range []struct {
		method, path string
		strong, weak int
	}{
		{"GET", "/customer/profile", 200, 200}, // recognition UX keeps working
		{"PUT", "/customer/profile", 200, 403}, // can't swap the phone number to take over the account
		{"POST", "/customer/purchase-credit", 200, 403},
	} {
		if got := do(tc.method, tc.path, strong); got != tc.strong {
			t.Errorf("%s %s with an OTP/payment session: %d, want %d", tc.method, tc.path, got, tc.strong)
		}
		if got := do(tc.method, tc.path, weak); got != tc.weak {
			t.Errorf("%s %s with a MAC-recognised session: %d, want %d", tc.method, tc.path, got, tc.weak)
		}
	}
}

func TestCustomerAuthByDevice_IssuesAWeakSession(t *testing.T) {
	setupC2BTestDB(t)
	config.Config.JWTSecret = "device-test-secret-device-test-secret"
	config.Config.JWTExpiry = time.Hour
	a := seedISP(t, "isp", "", 1)
	c := seedCustomer(t, a.ZoneID, "Sub", "254711000001", "ZYR#S1")
	mac := "aa:bb:cc:dd:ee:ff"
	exp := time.Now().Add(24 * time.Hour)
	config.DB.Model(&c).Updates(map[string]interface{}{"mac_address": mac, "expires_at": exp, "status": "active"})

	app := fiber.New()
	app.Get("/customer/auth/device", CustomerAuthByDevice)
	resp, err := app.Test(httptest.NewRequest("GET", "/customer/auth/device?mac="+mac, nil), -1)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("device auth failed: %v %v", err, resp)
	}
	var token string
	for _, ck := range resp.Cookies() {
		if ck.Name == middleware.CustomerCookieName {
			token = ck.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie issued")
	}
	claims := &middleware.Claims{}
	if _, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (interface{}, error) {
		return []byte(config.Config.JWTSecret), nil
	}); err != nil {
		t.Fatal(err)
	}
	if claims.AuthMethod != "device" || claims.CustomerID != c.ID {
		t.Errorf("claims = %+v, want a weak (device) session for the customer", claims)
	}
}
