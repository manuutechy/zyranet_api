package handlers

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
)

func authzApp() *fiber.App {
	app := fiber.New()
	adm := app.Group("/admin", middleware.AdminAuth())
	adm.Get("/me", Me)
	adm.Post("/logout", Logout)
	adm.Put("/users/:id", UserUpdate)
	adm.Post("/customers/cleanup-inactive", CustomerCleanupInactive)
	adm.Post("/customers/:id/add-credit", CustomerAddCredit)
	adm.Post("/zones/:id/exec", ZoneExecCommand)
	return app
}

func doAs(t *testing.T, app *fiber.App, method, path, token, body string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func staffUser(t *testing.T, orgID uint, role string) (models.User, string) {
	t.Helper()
	u := models.User{Name: role, Email: fmt.Sprintf("%s-%d-%d@example.com", role, orgID, time.Now().UnixNano()), Password: "x", Role: role, Status: "active", OrganizationID: orgID}
	if err := config.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	tok, err := middleware.GenerateAdminToken(u.ID, u.Role, u.ZoneID, u.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	return u, tok
}

func TestAdminSessions_RevokedByLogoutPasswordChangeAndDisable(t *testing.T) {
	setupTenantTest(t)
	app := authzApp()
	a := seedISP(t, "a", "", 1)
	owner, ownerTok := staffUser(t, a.OrgID, "super_admin")
	agent, agentTok := staffUser(t, a.OrgID, "field_agent")

	if st := doAs(t, app, "GET", "/admin/me", agentTok, ""); st != 200 {
		t.Fatalf("fresh token: %d", st)
	}
	// Password change by the owner ends the agent's sessions.
	if st := doAs(t, app, "PUT", fmt.Sprintf("/admin/users/%d", agent.ID), ownerTok, `{"password":"new-pass-123"}`); st != 200 {
		t.Fatalf("password change: %d", st)
	}
	if st := doAs(t, app, "GET", "/admin/me", agentTok, ""); st != 401 {
		t.Errorf("token issued before a password change still works: %d", st)
	}
	// A new login works until the account is disabled.
	agentTok2, _ := middleware.GenerateAdminToken(agent.ID, agent.Role, nil, agent.OrganizationID)
	if st := doAs(t, app, "GET", "/admin/me", agentTok2, ""); st != 200 {
		t.Fatalf("new token: %d", st)
	}
	config.DB.Model(&models.User{}).Where("id = ?", agent.ID).Update("status", "inactive")
	if st := doAs(t, app, "GET", "/admin/me", agentTok2, ""); st != 401 {
		t.Errorf("disabled user's token still works: %d", st)
	}

	// Logout kills the token even if someone copied it.
	if st := doAs(t, app, "POST", "/admin/logout", ownerTok, ""); st != 200 {
		t.Fatalf("logout: %d", st)
	}
	if st := doAs(t, app, "GET", "/admin/me", ownerTok, ""); st != 401 {
		t.Errorf("token still valid after logout: %d", st)
	}
	_ = owner
}

func TestAdminRoles_DemotionTakesEffectImmediately(t *testing.T) {
	setupTenantTest(t)
	app := authzApp()
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "C", "254711000001", "ZYR#C")
	u, tok := staffUser(t, a.OrgID, "super_admin")
	// Demoted directly in the database (no token bump): the DB role wins.
	config.DB.Model(&models.User{}).Where("id = ?", u.ID).Update("role", "field_agent")
	if st := doAs(t, app, "POST", fmt.Sprintf("/admin/customers/%d/add-credit", c.ID), tok, `{"amount":50,"type":"credit"}`); st != 403 {
		t.Errorf("demoted user minted credit: %d", st)
	}
}

func TestMoneyAndRouterActions_OwnerOnly(t *testing.T) {
	setupTenantTest(t)
	initMoneyServices(t, "")
	app := authzApp()
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "C", "254711000001", "ZYR#C")
	_, ownerTok := staffUser(t, a.OrgID, "super_admin")

	for _, role := range []string{"zone_manager", "finance", "field_agent"} {
		u, tok := staffUser(t, a.OrgID, role)
		if st := doAs(t, app, "POST", fmt.Sprintf("/admin/customers/%d/add-credit", c.ID), tok, `{"amount":500,"type":"credit"}`); st != 403 {
			t.Errorf("%s minting credit: %d, want 403", role, st)
		}
		if st := doAs(t, app, "POST", "/admin/customers/cleanup-inactive", tok, ""); st != 403 {
			t.Errorf("%s cleanup: %d, want 403", role, st)
		}
		// zone_manager of this zone passes the zone check but not the role check.
		config.DB.Model(&models.Zone{}).Where("id = ?", a.ZoneID).Update("manager_id", u.ID)
		if st := doAs(t, app, "POST", fmt.Sprintf("/admin/zones/%d/exec", a.ZoneID), tok, `{"command":"/system/reset-configuration"}`); st != 403 {
			t.Errorf("%s router exec: %d, want 403", role, st)
		}
	}
	if balance(t, c.ID) != 0 {
		t.Fatal("credit was minted")
	}

	// The owner can; debits never overdraw.
	if st := doAs(t, app, "POST", fmt.Sprintf("/admin/customers/%d/add-credit", c.ID), ownerTok, `{"amount":50,"type":"credit"}`); st != 200 {
		t.Fatalf("owner credit: %d", st)
	}
	if st := doAs(t, app, "POST", fmt.Sprintf("/admin/customers/%d/add-credit", c.ID), ownerTok, `{"amount":80,"type":"debit"}`); st != 422 {
		t.Errorf("overdraw debit: %d, want 422", st)
	}
	if got := balance(t, c.ID); got != 50 {
		t.Errorf("balance %v, want 50", got)
	}
}

func TestCleanupInactive_OnlyTouchesTheCallersISP(t *testing.T) {
	setupTenantTest(t)
	app := authzApp()
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	old := time.Now().Add(-30 * 24 * time.Hour)
	ga := seedCustomer(t, a.ZoneID, "GA", "GUEST-A", "ZYR#GUEST-A")
	gb := seedCustomer(t, b.ZoneID, "GB", "GUEST-B", "ZYR#GUEST-B")
	config.DB.Model(&models.Customer{}).Where("id IN ?", []uint{ga.ID, gb.ID}).Updates(map[string]interface{}{"status": "expired", "created_at": old})
	_, ownerA := staffUser(t, a.OrgID, "super_admin")

	if st := doAs(t, app, "POST", "/admin/customers/cleanup-inactive", ownerA, ""); st != 200 {
		t.Fatalf("cleanup: %d", st)
	}
	var n int64
	config.DB.Unscoped().Model(&models.Customer{}).Where("id = ?", gb.ID).Count(&n)
	if n != 1 {
		t.Errorf("ISP A's cleanup deleted ISP B's customer")
	}
	config.DB.Unscoped().Model(&models.Customer{}).Where("id = ?", ga.ID).Count(&n)
	if n != 0 {
		t.Errorf("ISP A's own stale guest was not cleaned up")
	}
}

func TestPlatformSessions_RoleAndRevocation(t *testing.T) {
	setupTenantTest(t)
	app := fiber.New()
	app.Post("/p/logout", middleware.PlatformAuth(), PlatformLogout)
	app.Get("/p/me", middleware.PlatformAuth(), PlatformMe)
	app.Post("/p/daraja", middleware.PlatformAuth(), middleware.RequirePlatformOwner(), func(c *fiber.Ctx) error { return c.SendStatus(204) })
	app.Patch("/p/staff/:id", middleware.PlatformAuth(), middleware.RequirePlatformOwner(), PlatformStaffUpdate)

	ownerU := models.PlatformUser{Name: "O", Email: "o@zyra.test", Password: "x", Status: "active"} // role from the column default
	staffU := models.PlatformUser{Name: "S", Email: "s@zyra.test", Password: "x", Status: "active", Role: "staff"}
	config.DB.Create(&ownerU)
	config.DB.Create(&staffU)
	config.DB.First(&ownerU, ownerU.ID)
	if ownerU.Role != "owner" {
		t.Fatalf("existing/default platform users must be owners, got %q", ownerU.Role)
	}
	ownerTok, _ := middleware.GeneratePlatformToken(ownerU.ID)
	staffTok, _ := middleware.GeneratePlatformToken(staffU.ID)

	if st := doAs(t, app, "POST", "/p/daraja", staffTok, ""); st != 403 {
		t.Errorf("staff edited Daraja keys: %d", st)
	}
	if st := doAs(t, app, "POST", "/p/daraja", ownerTok, ""); st != 204 {
		t.Errorf("owner blocked from Daraja keys: %d", st)
	}
	// Disabling staff ends their session.
	if st := doAs(t, app, "PATCH", fmt.Sprintf("/p/staff/%d", staffU.ID), ownerTok, `{"status":"inactive"}`); st != 200 {
		t.Fatalf("disable staff: %d", st)
	}
	if st := doAs(t, app, "GET", "/p/me", staffTok, ""); st != 401 {
		t.Errorf("disabled platform user still signed in: %d", st)
	}
	// An owner can't demote themselves.
	if st := doAs(t, app, "PATCH", fmt.Sprintf("/p/staff/%d", ownerU.ID), ownerTok, `{"role":"staff"}`); st != 400 {
		t.Errorf("self-demotion: %d, want 400", st)
	}
	// Logout revokes.
	doAs(t, app, "POST", "/p/logout", ownerTok, "")
	if st := doAs(t, app, "GET", "/p/me", ownerTok, ""); st != 401 {
		t.Errorf("platform token valid after logout: %d", st)
	}
}
