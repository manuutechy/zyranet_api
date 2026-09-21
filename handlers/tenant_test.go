package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"golang.org/x/crypto/bcrypt"
)

const (
	acmeHost        = "https://acme.zyranet.co.ke"
	betaHost        = "https://beta.zyranet.co.ke"
	sharedAdminHost = "https://admin.zyranet.co.ke"
)

func setupTenantTest(t *testing.T) {
	t.Helper()
	setupC2BTestDB(t)
	oldBase, oldSecret, oldEnv, oldExpiry := config.Config.BaseDomain, config.Config.JWTSecret, config.Config.AppEnv, config.Config.JWTExpiry
	config.Config.JWTExpiry = time.Hour
	config.Config.BaseDomain = "zyranet.co.ke"
	config.Config.JWTSecret = "tenant-test-secret-tenant-test-secret"
	config.Config.AppEnv = "test"
	middleware.InvalidateTenantCache()
	t.Cleanup(func() {
		config.Config.BaseDomain, config.Config.JWTSecret, config.Config.AppEnv, config.Config.JWTExpiry = oldBase, oldSecret, oldEnv, oldExpiry
		middleware.InvalidateTenantCache()
	})
}

func tenantApp() *fiber.App {
	app := fiber.New()
	app.Post("/login", Login)
	app.Get("/tenant", TenantPublic)
	app.Get("/check", OrganizationSubdomainCheck)
	app.Post("/orgs", OrganizationStore)
	app.Patch("/orgs/:id", OrganizationUpdate)
	adm := app.Group("/admin", middleware.AdminAuth())
	adm.Get("/me", Me)
	adm.Post("/users", UserStore)
	adm.Put("/users/:id", UserUpdate)
	return app
}

// call sends a request and returns status + decoded JSON body.
func call(t *testing.T, method, path, origin, bearer, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := tenantApp().Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func strp(s string) *string { return &s }

// seedTenant creates an ISP (with an optional subdomain) and one super_admin.
func seedTenant(t *testing.T, slug string, subdomain *string, status string) (org models.Organization, email string) {
	t.Helper()
	org = models.Organization{Name: slug, Slug: slug, Subdomain: subdomain, Status: status}
	if err := config.DB.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw-12345"), bcrypt.MinCost)
	email = slug + "@example.com"
	u := models.User{Name: slug, Email: email, Password: string(hash), Role: "super_admin", Status: "active", OrganizationID: org.ID}
	if err := config.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return org, email
}

func loginBody(email string) string {
	return fmt.Sprintf(`{"email":%q,"password":"pw-12345"}`, email)
}

func TestTenantLogin_LockedToOwnSubdomain(t *testing.T) {
	setupTenantTest(t)
	_, acmeEmail := seedTenant(t, "acme", strp("acme"), "active")
	seedTenant(t, "beta", strp("beta"), "active")

	if st, _ := call(t, "POST", "/login", acmeHost, "", loginBody(acmeEmail)); st != 200 {
		t.Errorf("acme staff on acme's subdomain: status %d, want 200", st)
	}
	if st, _ := call(t, "POST", "/login", betaHost, "", loginBody(acmeEmail)); st != 401 {
		t.Errorf("acme staff on beta's subdomain: status %d, want 401", st)
	}
	// The shared admin host is unrestricted, so existing ISPs keep working.
	if st, _ := call(t, "POST", "/login", sharedAdminHost, "", loginBody(acmeEmail)); st != 200 {
		t.Errorf("acme staff on the shared admin host: status %d, want 200", st)
	}
	// Non-browser client / localhost: no Origin, no restriction.
	if st, _ := call(t, "POST", "/login", "", "", loginBody(acmeEmail)); st != 200 {
		t.Errorf("no Origin: status %d, want 200", st)
	}
}

func TestTenantLogin_UnknownAndSuspendedSubdomains(t *testing.T) {
	setupTenantTest(t)
	_, email := seedTenant(t, "acme", strp("acme"), "active")
	_, suspendedEmail := seedTenant(t, "gone", strp("gone"), "suspended")

	if st, _ := call(t, "POST", "/login", "https://typo.zyranet.co.ke", "", loginBody(email)); st != 404 {
		t.Errorf("unknown subdomain: status %d, want 404", st)
	}
	if st, _ := call(t, "POST", "/login", "https://gone.zyranet.co.ke", "", loginBody(suspendedEmail)); st != 403 {
		t.Errorf("suspended ISP: status %d, want 403", st)
	}
}

func TestTenantSession_CookieFromOneISPIsRejectedOnAnother(t *testing.T) {
	setupTenantTest(t)
	acme, acmeEmail := seedTenant(t, "acme", strp("acme"), "active")
	seedTenant(t, "beta", strp("beta"), "active")

	_, out := call(t, "POST", "/login", acmeHost, "", loginBody(acmeEmail))
	token, _ := out["data"].(map[string]interface{})["token"].(string)
	if token == "" {
		t.Fatalf("no token in login response: %v", out)
	}
	_ = acme

	if st, _ := call(t, "GET", "/admin/me", acmeHost, token, ""); st != 200 {
		t.Errorf("own subdomain: status %d, want 200", st)
	}
	if st, _ := call(t, "GET", "/admin/me", betaHost, token, ""); st != 403 {
		t.Errorf("another ISP's subdomain: status %d, want 403", st)
	}
	if st, _ := call(t, "GET", "/admin/me", "https://nobody.zyranet.co.ke", token, ""); st != 403 {
		t.Errorf("unknown subdomain: status %d, want 403", st)
	}
	if st, _ := call(t, "GET", "/admin/me", sharedAdminHost, token, ""); st != 200 {
		t.Errorf("shared admin host: status %d, want 200", st)
	}
}

func TestPlatform_CreateOrgWithSubdomain(t *testing.T) {
	setupTenantTest(t)
	body := `{"name":"Acme ISP","slug":"acme","subdomain":" ACME ","admin_email":"boss@acme.test","admin_password":"pw-12345"}`
	st, out := call(t, "POST", "/orgs", "", "", body)
	if st != 201 {
		t.Fatalf("status %d: %v", st, out)
	}
	data := out["data"].(map[string]interface{})
	org := data["organization"].(map[string]interface{})
	if org["subdomain"] != "acme" || org["admin_url"] != "https://acme.zyranet.co.ke" {
		t.Errorf("subdomain/admin_url = %v / %v", org["subdomain"], org["admin_url"])
	}
	if data["admin_user"].(map[string]interface{})["login_url"] != "https://acme.zyranet.co.ke" {
		t.Errorf("admin user login_url = %v", data["admin_user"])
	}

	// The new ISP's origin is recognised immediately (cache invalidated on create).
	if !middleware.IsTenantOrigin(acmeHost) {
		t.Error("new ISP's origin should be allowed right after creation")
	}
}

func TestPlatform_SubdomainValidation(t *testing.T) {
	setupTenantTest(t)
	seedTenant(t, "acme", strp("acme"), "active")

	create := func(sub string) int {
		st, _ := call(t, "POST", "/orgs", "", "", fmt.Sprintf(
			`{"name":"X","slug":"x-%s","subdomain":%q,"admin_email":"a-%s@x.test","admin_password":"pw-12345"}`, sub, sub, sub))
		return st
	}
	if st := create("acme"); st != 409 {
		t.Errorf("duplicate subdomain: %d, want 409", st)
	}
	if st := create("ACME"); st != 409 {
		t.Errorf("duplicate subdomain differing by case: %d, want 409", st)
	}
	for _, bad := range []string{"admin", "captive", "a", "has space", "-bad"} {
		if st := create(bad); st != 422 {
			t.Errorf("subdomain %q: %d, want 422", bad, st)
		}
	}
}

func TestPlatform_UpdateSetChangeAndClearSubdomain(t *testing.T) {
	setupTenantTest(t)
	a, _ := seedTenant(t, "one", nil, "active")
	b, _ := seedTenant(t, "two", nil, "active")

	patch := func(id uint, sub string) (int, map[string]interface{}) {
		return call(t, "PATCH", fmt.Sprintf("/orgs/%d", id), "", "", fmt.Sprintf(`{"subdomain":%q}`, sub))
	}
	if st, _ := patch(a.ID, "first"); st != 200 {
		t.Fatalf("set: %d", st)
	}
	if st, _ := patch(b.ID, "first"); st != 409 {
		t.Errorf("taking another ISP's subdomain: %d, want 409", st)
	}
	if st, _ := patch(a.ID, "first"); st != 200 {
		t.Errorf("re-saving its own subdomain must not conflict: %d", st)
	}
	// Two ISPs with no subdomain must coexist (NULL, not '' under the unique index).
	if st, _ := patch(a.ID, ""); st != 200 {
		t.Errorf("clear a: %d", st)
	}
	if st, _ := patch(b.ID, ""); st != 200 {
		t.Errorf("clear b: %d", st)
	}
	var got models.Organization
	config.DB.First(&got, a.ID)
	if got.Subdomain != nil {
		t.Errorf("cleared subdomain should be NULL, got %q", *got.Subdomain)
	}
	if middleware.IsTenantOrigin("https://first.zyranet.co.ke") {
		t.Error("a cleared subdomain must stop being allowed")
	}
}

func TestPlatform_SubdomainCheck(t *testing.T) {
	setupTenantTest(t)
	seedTenant(t, "acme", strp("acme"), "active")
	for q, wantAvail := range map[string]bool{"fresh": true, "acme": false, "admin": false, "x": false} {
		_, out := call(t, "GET", "/check?subdomain="+q, "", "", "")
		if got := out["data"].(map[string]interface{})["available"]; got != wantAvail {
			t.Errorf("check %q: available=%v, want %v", q, got, wantAvail)
		}
	}
}

func TestTenantPublic(t *testing.T) {
	setupTenantTest(t)
	org, _ := seedTenant(t, "acme", strp("acme"), "active")
	config.DB.Model(&org).Updates(map[string]interface{}{"captive_portal_company_name": "Acme Wi-Fi", "captive_portal_primary_color": "#123456"})

	st, out := call(t, "GET", "/tenant?subdomain=acme", "", "", "")
	data, _ := out["data"].(map[string]interface{})
	if st != 200 || data["name"] != "Acme Wi-Fi" || data["primary_color"] != "#123456" {
		t.Errorf("status %d data %v", st, out)
	}
	if _, leaked := data["id"]; leaked {
		t.Error("public tenant info must not expose internal ids")
	}
	if st, _ := call(t, "GET", "/tenant?subdomain=nope", "", "", ""); st != 404 {
		t.Errorf("unknown subdomain: %d, want 404", st)
	}
	if st, _ := call(t, "GET", "/tenant", acmeHost, "", ""); st != 200 {
		t.Errorf("resolved from Origin: %d, want 200", st)
	}
}

func TestIsTenantOrigin(t *testing.T) {
	setupTenantTest(t)
	seedTenant(t, "acme", strp("acme"), "active")
	for origin, want := range map[string]bool{
		"https://acme.zyranet.co.ke":          true,
		"https://beta.zyranet.co.ke":          false, // no such ISP
		"https://admin.zyranet.co.ke":         false, // platform host: handled by the static list
		"https://acme.evil.com":               false,
		"https://acme.zyranet.co.ke.evil.com": false,
		"not a url":                           false,
	} {
		if got := middleware.IsTenantOrigin(origin); got != want {
			t.Errorf("IsTenantOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestUserStore_ZoneMustBelongToTheCallersISP(t *testing.T) {
	setupTenantTest(t)
	acme, acmeEmail := seedTenant(t, "acme", strp("acme"), "active")
	beta, _ := seedTenant(t, "beta", strp("beta"), "active")
	acmeZone := models.Zone{Name: "AZ", Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: acme.ID}
	betaZone := models.Zone{Name: "BZ", Location: "L", RouterName: "r", RouterIP: "2.2.2.2", OrganizationID: beta.ID}
	config.DB.Create(&acmeZone)
	config.DB.Create(&betaZone)

	_, out := call(t, "POST", "/login", acmeHost, "", loginBody(acmeEmail))
	token := out["data"].(map[string]interface{})["token"].(string)

	newUser := func(email, role string, zone uint) (int, map[string]interface{}) {
		return call(t, "POST", "/admin/users", acmeHost, token,
			fmt.Sprintf(`{"name":"N","email":%q,"password":"pw-12345","role":%q,"zone_id":%d}`, email, role, zone))
	}
	if st, _ := newUser("x1@acme.test", "zone_manager", betaZone.ID); st != 422 {
		t.Errorf("zone of another ISP: %d, want 422", st)
	}
	if st, _ := newUser("x2@acme.test", "godmode", acmeZone.ID); st != 422 {
		t.Errorf("unknown role: %d, want 422", st)
	}
	st, created := newUser("x3@acme.test", "zone_manager", acmeZone.ID)
	if st != 201 {
		t.Fatalf("valid user: %d %v", st, created)
	}
	if created["data"].(map[string]interface{})["login_url"] != "https://acme.zyranet.co.ke" {
		t.Errorf("new user should be told where to log in, got %v", created["data"])
	}

	// Same guard on update.
	id := int(created["data"].(map[string]interface{})["id"].(float64))
	st, _ = call(t, "PUT", fmt.Sprintf("/admin/users/%d", id), acmeHost, token, fmt.Sprintf(`{"zone_id":%d}`, betaZone.ID))
	if st != 422 {
		t.Errorf("moving a user into another ISP's zone: %d, want 422", st)
	}
	st, _ = call(t, "PUT", fmt.Sprintf("/admin/users/%d", id), acmeHost, token, `{"role":"super_admin"}`)
	if st != 200 {
		t.Errorf("valid role change: %d, want 200", st)
	}
}

func onboard(t *testing.T, body string) (int, map[string]interface{}) {
	t.Helper()
	return call(t, "POST", "/orgs", "", "", body)
}

func orgCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	config.DB.Unscoped().Model(&models.Organization{}).Count(&n)
	return n
}

func TestOnboarding_DefaultsSlugGeneratesPasswordAndFallsBackToSharedAdmin(t *testing.T) {
	setupTenantTest(t)
	st, out := onboard(t, `{"name":"Coastline Networks","admin_email":"Boss@Coastline.test"}`)
	if st != 201 {
		t.Fatalf("status %d: %v", st, out)
	}
	data := out["data"].(map[string]interface{})
	org := data["organization"].(map[string]interface{})
	if org["slug"] != "coastline-networks" {
		t.Errorf("slug = %v, want it derived from the name", org["slug"])
	}
	if data["login_url"] != "https://admin.zyranet.co.ke" {
		t.Errorf("no subdomain → login_url = %v, want the shared admin site", data["login_url"])
	}
	temp, _ := data["temp_password"].(string)
	if len(temp) < 10 {
		t.Fatalf("expected a generated temp_password, got %q", temp)
	}
	// The generated password really works, and the email was normalised.
	if st, _ := call(t, "POST", "/login", "", "", fmt.Sprintf(`{"email":"boss@coastline.test","password":%q}`, temp)); st != 200 {
		t.Errorf("login with the generated password: %d, want 200", st)
	}
}

func TestOnboarding_ChosenPasswordIsNotEchoedBack(t *testing.T) {
	setupTenantTest(t)
	_, out := onboard(t, `{"name":"Acme","admin_email":"a@acme.test","admin_password":"chosen-pass-1"}`)
	if _, present := out["data"].(map[string]interface{})["temp_password"]; present {
		t.Error("a password the operator chose must not be returned")
	}
}

func TestOnboarding_IsAtomic_TakenAdminEmailLeavesNoISPBehind(t *testing.T) {
	setupTenantTest(t)
	seedTenant(t, "existing", nil, "active") // owns existing@example.com
	before := orgCount(t)

	st, out := onboard(t, `{"name":"Newco","admin_email":"existing@example.com","subdomain":"newco"}`)
	if st != 409 || out["field"] != "admin_email" {
		t.Errorf("status %d field %v, want 409 on admin_email", st, out["field"])
	}
	if after := orgCount(t); after != before {
		t.Errorf("%d organization(s) created despite the failure — onboarding must be all-or-nothing", after-before)
	}
	// And the slug/subdomain are still free to use for a valid retry.
	if st, _ := onboard(t, `{"name":"Newco","admin_email":"fresh@newco.test","subdomain":"newco"}`); st != 201 {
		t.Errorf("retry after a rejected attempt: %d, want 201", st)
	}
}

func TestOnboarding_ValidationNamesTheField(t *testing.T) {
	setupTenantTest(t)
	seedTenant(t, "taken", strp("takensub"), "active")
	for name, tc := range map[string]struct {
		body   string
		status int
		field  string
	}{
		"missing name":   {`{"admin_email":"a@x.test"}`, 422, "name"},
		"missing email":  {`{"name":"X"}`, 422, "admin_email"},
		"bad email":      {`{"name":"X","admin_email":"not-an-email"}`, 422, "admin_email"},
		"short password": {`{"name":"X","admin_email":"a@x.test","admin_password":"short"}`, 422, "admin_password"},
		"bad slug":       {`{"name":"X","slug":"Bad Slug!","admin_email":"a@x.test"}`, 422, "slug"},
		"taken slug":     {`{"name":"X","slug":"taken","admin_email":"a@x.test"}`, 409, "slug"},
		"reserved sub":   {`{"name":"X","admin_email":"a@x.test","subdomain":"admin"}`, 422, "subdomain"},
		"taken sub":      {`{"name":"X","admin_email":"a@x.test","subdomain":"takensub"}`, 409, "subdomain"},
	} {
		st, out := onboard(t, tc.body)
		if st != tc.status || out["field"] != tc.field {
			t.Errorf("%s: status %d field %v, want %d %s", name, st, out["field"], tc.status, tc.field)
		}
	}
}

func TestOnboarding_EmailOfADeletedUserCanBeReused(t *testing.T) {
	setupTenantTest(t)
	_, email := seedTenant(t, "old", nil, "active")
	config.DB.Where("email = ?", email).Delete(&models.User{}) // soft delete

	if st, out := onboard(t, fmt.Sprintf(`{"name":"Reborn","admin_email":%q}`, email)); st != 201 {
		t.Errorf("reusing a soft-deleted user's email: %d %v, want 201", st, out)
	}
}

// The live admin site is bit1.<base domain>, which looks exactly like an ISP
// subdomain. It must be treated as a platform host, or every login there breaks.
func TestPlatformHostsAreNeverTreatedAsISPSubdomains(t *testing.T) {
	setupTenantTest(t)
	old := config.Config.AllowedOrigins
	config.Config.AllowedOrigins = []string{"https://bit1.zyranet.co.ke", "https://platform.zyranet.co.ke"}
	t.Cleanup(func() { config.Config.AllowedOrigins = old })
	_, email := seedTenant(t, "acme", strp("acme"), "active")
	const liveAdmin = "https://bit1.zyranet.co.ke"

	_, out := call(t, "POST", "/login", liveAdmin, "", loginBody(email))
	if out["success"] != true {
		t.Fatalf("login from the live admin host failed: %v", out)
	}
	token := out["data"].(map[string]interface{})["token"].(string)
	if st, _ := call(t, "GET", "/admin/me", liveAdmin, token, ""); st != 200 {
		t.Errorf("authenticated request from the live admin host: %d, want 200", st)
	}
	if middleware.IsTenantOrigin(liveAdmin) {
		t.Error("the live admin host must not count as an ISP origin")
	}

	// ...and no ISP can be given that name.
	_, check := call(t, "GET", "/check?subdomain=bit1", "", "", "")
	if check["data"].(map[string]interface{})["available"] != false {
		t.Error("the platform's own hostname must not be available as an ISP subdomain")
	}
	if st, _ := onboard(t, `{"name":"X","admin_email":"x@x.test","subdomain":"bit1"}`); st != 422 {
		t.Errorf("onboarding with subdomain bit1: %d, want 422", st)
	}
}

// The live admin site (bit1.) and the platform's other sites must be reported
// as platform hosts — not "unknown ISP" — or the login page there is blocked.
func TestTenantPublic_PlatformHostsAreNotUnknownPortals(t *testing.T) {
	setupTenantTest(t)
	old := config.Config.AllowedOrigins
	config.Config.AllowedOrigins = []string{"https://bit1.zyranet.co.ke"}
	t.Cleanup(func() { config.Config.AllowedOrigins = old })

	for _, label := range []string{"bit1", "admin", "platform", "captive", "API"} {
		st, out := call(t, "GET", "/tenant?subdomain="+label, "", "", "")
		if st != 200 || out["data"].(map[string]interface{})["platform"] != true {
			t.Errorf("%q: status %d %v — want 200 with platform:true", label, st, out)
		}
	}
	// A genuinely unknown label is still a 404, and a real ISP still resolves.
	if st, _ := call(t, "GET", "/tenant?subdomain=typo123", "", "", ""); st != 404 {
		t.Errorf("unknown label: %d, want 404", st)
	}
	seedTenant(t, "acme", strp("acme"), "active")
	if st, out := call(t, "GET", "/tenant?subdomain=acme", "", "", ""); st != 200 || out["data"].(map[string]interface{})["platform"] == true {
		t.Errorf("a real ISP must not be reported as a platform host: %d %v", st, out)
	}
}

// Editing an organization from the platform app: the exact body its edit form sends.
func TestPlatform_EditFormBodyWithSubdomainSucceeds(t *testing.T) {
	setupTenantTest(t)
	org, _ := seedTenant(t, "acme", nil, "active")
	body := `{"name":"Acme ISP","subdomain":"acme","contact_email":"ops@acme.test","contact_phone":"","billing_rate_per_customer":0,"commission_percent":null}`
	st, out := call(t, "PATCH", fmt.Sprintf("/orgs/%d", org.ID), "", "", body)
	if st != 200 {
		t.Fatalf("edit form body: %d %v", st, out)
	}
	if got := out["data"].(map[string]interface{}); got["subdomain"] != "acme" || got["admin_url"] != "https://acme.zyranet.co.ke" {
		t.Errorf("after edit: %v", got)
	}
}

func TestPlatform_DirectSettlementNeedsADestination(t *testing.T) {
	setupTenantTest(t)
	org, _ := seedTenant(t, "acme", nil, "active")
	path := fmt.Sprintf("/orgs/%d", org.ID)
	if st, _ := call(t, "PATCH", path, "", "", `{"direct_settlement":true}`); st != 422 {
		t.Errorf("no destination: %d, want 422", st)
	}
	config.DB.Model(&org).Updates(map[string]interface{}{"settlement_type": "till", "settlement_till_number": "555666"})
	if st, out := call(t, "PATCH", path, "", "", `{"direct_settlement":true}`); st != 200 || out["data"].(map[string]interface{})["direct_settlement"] != true {
		t.Errorf("with a till: %d %v", st, out)
	}
	if st, _ := call(t, "PATCH", path, "", "", `{"direct_settlement":"yes"}`); st != 422 {
		t.Errorf("non-boolean: %d, want 422", st)
	}
}

func TestISPSavingDestinationSwitchesOnDirectSettlement(t *testing.T) {
	setupTenantTest(t)
	org, _ := seedTenant(t, "acme", nil, "active")
	app := fiber.New()
	app.Post("/settings/mpesa", func(c *fiber.Ctx) error {
		c.Locals("claims", &middleware.Claims{Role: "super_admin", OrganizationID: org.ID, Type: "admin"})
		return c.Next()
	}, OrganizationMpesaUpdate)
	req := httptest.NewRequest("POST", "/settings/mpesa", strings.NewReader(`{"mode":"platform","settlement_type":"till","settlement_till_number":" 555666 "}`))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := app.Test(req, -1)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got models.Organization
	config.DB.First(&got, org.ID)
	if !got.DirectSettlement || got.SettlementTillNumber != "555666" {
		t.Errorf("after saving a till: direct=%v till=%q", got.DirectSettlement, got.SettlementTillNumber)
	}
}
