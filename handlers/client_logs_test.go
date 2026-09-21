package handlers

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
)

func createRadiusSchema(t *testing.T) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE radcheck (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL DEFAULT '', attribute TEXT NOT NULL DEFAULT '', op TEXT NOT NULL DEFAULT '==', value TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE radreply (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL DEFAULT '', attribute TEXT NOT NULL DEFAULT '', op TEXT NOT NULL DEFAULT '=', value TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE nas (id INTEGER PRIMARY KEY AUTOINCREMENT, nasname TEXT NOT NULL, shortname TEXT, type TEXT DEFAULT 'other', ports INTEGER, secret TEXT NOT NULL DEFAULT 'secret', server TEXT, community TEXT, description TEXT)`,
		`CREATE TABLE radacct (radacctid INTEGER PRIMARY KEY AUTOINCREMENT, acctsessionid TEXT, acctuniqueid TEXT, username TEXT, nasipaddress TEXT, acctstarttime DATETIME, acctupdatetime DATETIME, acctstoptime DATETIME, acctsessiontime INTEGER, acctinputoctets INTEGER, acctoutputoctets INTEGER, callingstationid TEXT, framedipaddress TEXT, acctterminatecause TEXT)`,
		`CREATE TABLE radpostauth (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, pass TEXT, reply TEXT, authdate DATETIME)`,
	} {
		if err := config.DB.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	services.ResetRadiusTableCache()
	t.Cleanup(services.ResetRadiusTableCache)
}

func logsApp(role string, orgID uint, zoneID *uint) *fiber.App {
	app := fiber.New()
	adm := app.Group("/admin", func(c *fiber.Ctx) error {
		c.Locals("claims", &middleware.Claims{Role: role, OrganizationID: orgID, ZoneID: zoneID, Type: "admin"})
		return c.Next()
	})
	adm.Get("/logs/sessions", ClientSessionsLog)
	adm.Get("/logs/sign-ins", ClientSignInsLog)
	adm.Get("/customers/:id/activity", CustomerActivity)
	adm.Get("/zones/:id/queue-tuning-script", ZoneQueueTuningScript)
	return app
}

func lcall(t *testing.T, app *fiber.App, path string) (int, map[string]interface{}, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	raw := string(buf[:n])
	out := map[string]interface{}{}
	_ = json.Unmarshal([]byte(raw), &out)
	return resp.StatusCode, out, raw
}

// radiusFor gives a customer a RADIUS account and a finished + a live session.
func radiusFor(t *testing.T, c models.Customer, mac string) {
	t.Helper()
	config.DB.Create(&models.RadiusAccount{Username: mac, CustomerID: c.ID, ZoneID: c.ZoneID, Kind: "hotspot", Active: true})
	start := time.Now().Add(-3 * time.Hour)
	stop := time.Now().Add(-2 * time.Hour)
	config.DB.Exec(`INSERT INTO radacct (acctsessionid, acctuniqueid, username, acctstarttime, acctstoptime, acctsessiontime, acctinputoctets, acctoutputoctets, callingstationid, framedipaddress, acctterminatecause)
		VALUES (?, ?, ?, ?, ?, 3600, 1048576, 52428800, ?, '10.5.50.20', 'User-Request')`, "s1-"+mac, "u1-"+mac, mac, start, stop, mac)
	config.DB.Exec(`INSERT INTO radacct (acctsessionid, acctuniqueid, username, acctstarttime, acctsessiontime, acctinputoctets, acctoutputoctets, callingstationid, framedipaddress)
		VALUES (?, ?, ?, ?, 600, 2048, 4096, ?, '10.5.50.21')`, "s2-"+mac, "u2-"+mac, mac, time.Now().Add(-10*time.Minute), mac)
}

func TestSessionsLog_IsIsolatedPerISP(t *testing.T) {
	setupC2BTestDB(t)
	createRadiusSchema(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	ca := seedCustomer(t, a.ZoneID, "Alice", "254711000001", "ZYR#A")
	cb := seedCustomer(t, b.ZoneID, "Bob", "254722000002", "ZYR#B")
	radiusFor(t, ca, "AA:AA:AA:AA:AA:01")
	radiusFor(t, cb, "BB:BB:BB:BB:BB:02")

	st, out, raw := lcall(t, logsApp("super_admin", a.OrgID, nil), "/admin/logs/sessions")
	if st != 200 {
		t.Fatalf("status %d", st)
	}
	if strings.Contains(raw, "Bob") || strings.Contains(raw, "BB:BB:BB:BB:BB:02") {
		t.Fatal("ISP A can see ISP B's customer or device in its session log")
	}
	data := out["data"].(map[string]interface{})
	if got := len(data["sessions"].([]interface{})); got != 2 {
		t.Errorf("ISP A sees %d sessions, want its own 2", got)
	}
	sum := data["summary"].(map[string]interface{})
	if sum["active_now"] != 1.0 || sum["bytes_down"] != float64(52428800+4096) || sum["bytes_up"] != float64(1048576+2048) {
		t.Errorf("summary = %v", sum)
	}
	first := data["sessions"].([]interface{})[0].(map[string]interface{})
	if first["customer_name"] != "Alice" || first["active"] != true {
		t.Errorf("newest session should be Alice's live one: %v", first)
	}

	// Asking for the other ISP's zone by id still shows nothing of theirs.
	_, _, raw = lcall(t, logsApp("super_admin", a.OrgID, nil), fmt.Sprintf("/admin/logs/sessions?zone_id=%d", b.ZoneID))
	if strings.Contains(raw, "Bob") {
		t.Error("filtering by another ISP's zone id leaked its sessions")
	}
	_, _, raw = lcall(t, logsApp("super_admin", a.OrgID, nil), fmt.Sprintf("/admin/logs/sessions?customer_id=%d", cb.ID))
	if strings.Contains(raw, "Bob") {
		t.Error("filtering by another ISP's customer id leaked its sessions")
	}
}

func TestSessionsLog_FiltersAndZoneManagerScope(t *testing.T) {
	setupC2BTestDB(t)
	createRadiusSchema(t)
	i := seedISP(t, "a", "", 2)
	var zones []models.Zone
	config.DB.Where("organization_id = ?", i.OrgID).Order("id").Find(&zones)
	c1 := seedCustomer(t, zones[0].ID, "Alice", "254711000001", "ZYR#A")
	c2 := seedCustomer(t, zones[1].ID, "Carol", "254733000003", "ZYR#C")
	radiusFor(t, c1, "AA:AA:AA:AA:AA:01")
	radiusFor(t, c2, "CC:CC:CC:CC:CC:03")
	admin := logsApp("super_admin", i.OrgID, nil)

	count := func(app *fiber.App, q string) int {
		_, out, _ := lcall(t, app, "/admin/logs/sessions"+q)
		return len(out["data"].(map[string]interface{})["sessions"].([]interface{}))
	}
	if n := count(admin, ""); n != 4 {
		t.Errorf("all: %d", n)
	}
	if n := count(admin, "?status=active"); n != 2 {
		t.Errorf("active: %d", n)
	}
	if n := count(admin, "?status=ended"); n != 2 {
		t.Errorf("ended: %d", n)
	}
	if n := count(admin, "?q=Carol"); n != 2 {
		t.Errorf("search by name: %d", n)
	}
	if n := count(admin, "?q=254711000001"); n != 2 {
		t.Errorf("search by phone: %d", n)
	}
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if n := count(admin, "?from="+tomorrow); n != 0 {
		t.Errorf("from tomorrow: %d", n)
	}
	// A zone manager only sees their own zone.
	zm := logsApp("zone_manager", i.OrgID, &zones[0].ID)
	if n := count(zm, ""); n != 2 {
		t.Errorf("zone manager sees %d sessions, want only zone 1's 2", n)
	}
	_, _, raw := lcall(t, zm, "/admin/logs/sessions")
	if strings.Contains(raw, "Carol") {
		t.Error("a zone manager can see another zone's customer")
	}
}

func TestSessionsLog_WithoutRadiusInstalled(t *testing.T) {
	setupC2BTestDB(t) // no RADIUS tables
	services.ResetRadiusTableCache()
	t.Cleanup(services.ResetRadiusTableCache)
	i := seedISP(t, "a", "", 1)
	st, out, _ := lcall(t, logsApp("super_admin", i.OrgID, nil), "/admin/logs/sessions")
	if st != 200 || out["data"].(map[string]interface{})["radius_enabled"] != false {
		t.Errorf("status %d out %v — should report RADIUS not enabled, not error", st, out)
	}
}

func TestSignInsLog_NeverExposesAttemptedPasswords(t *testing.T) {
	setupC2BTestDB(t)
	createRadiusSchema(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	ca := seedCustomer(t, a.ZoneID, "Alice", "254711000001", "ZYR#A")
	cb := seedCustomer(t, b.ZoneID, "Bob", "254722000002", "ZYR#B")
	config.DB.Create(&models.RadiusAccount{Username: "AA:AA:AA:AA:AA:01", CustomerID: ca.ID, ZoneID: a.ZoneID, Active: true})
	config.DB.Create(&models.RadiusAccount{Username: "BB:BB:BB:BB:BB:02", CustomerID: cb.ID, ZoneID: b.ZoneID, Active: true})
	now := time.Now()
	config.DB.Exec(`INSERT INTO radpostauth (username, pass, reply, authdate) VALUES ('AA:AA:AA:AA:AA:01','hunter2-secret','Access-Accept',?)`, now.Add(-2*time.Hour))
	config.DB.Exec(`INSERT INTO radpostauth (username, pass, reply, authdate) VALUES ('AA:AA:AA:AA:AA:01','wrong-guess','Access-Reject',?)`, now.Add(-time.Hour))
	config.DB.Exec(`INSERT INTO radpostauth (username, pass, reply, authdate) VALUES ('BB:BB:BB:BB:BB:02','x','Access-Reject',?)`, now)

	st, out, raw := lcall(t, logsApp("super_admin", a.OrgID, nil), "/admin/logs/sign-ins")
	if st != 200 {
		t.Fatalf("status %d", st)
	}
	if strings.Contains(raw, "hunter2-secret") || strings.Contains(raw, "wrong-guess") {
		t.Fatal("an attempted password appears in the sign-in log")
	}
	if strings.Contains(raw, "Bob") {
		t.Fatal("ISP A can see ISP B's sign-ins")
	}
	rows := out["data"].(map[string]interface{})["sign_ins"].([]interface{})
	if len(rows) != 2 || rows[0].(map[string]interface{})["accepted"] != false || rows[1].(map[string]interface{})["accepted"] != true {
		t.Errorf("rows (newest first: refused, then accepted): %v", rows)
	}
	_, out, _ = lcall(t, logsApp("super_admin", a.OrgID, nil), "/admin/logs/sign-ins?result=rejected")
	if n := len(out["data"].(map[string]interface{})["sign_ins"].([]interface{})); n != 1 {
		t.Errorf("rejected filter: %d", n)
	}
}

func TestCustomerActivity_TimelineAndTenantSafety(t *testing.T) {
	setupC2BTestDB(t)
	createRadiusSchema(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	cust := seedCustomer(t, a.ZoneID, "Alice", "254711000001", "ZYR#A")
	other := seedCustomer(t, b.ZoneID, "Bob", "254722000002", "ZYR#B")

	rc := "RCPT1"
	config.DB.Create(&models.Payment{CustomerID: &cust.ID, ZoneID: a.ZoneID, Phone: "254711000001", Amount: 100, Method: "mpesa", Status: "completed", MpesaReceiptNumber: &rc})
	config.DB.Create(&models.CreditLog{CustomerID: cust.ID, Amount: 50, Type: "credit"})
	orgA := a.OrgID
	config.DB.Create(&models.SmsLog{OrganizationID: &orgA, Phone: "254711000001", Message: "Your plan is active", Status: "sent"})
	orgB := b.OrgID
	config.DB.Create(&models.SmsLog{OrganizationID: &orgB, Phone: "254711000001", Message: "SECRET FROM ANOTHER ISP", Status: "sent"})
	radiusFor(t, cust, "AA:AA:AA:AA:AA:01")
	config.DB.Exec(`INSERT INTO radpostauth (username, pass, reply, authdate) VALUES ('AA:AA:AA:AA:AA:01','pw','Access-Reject',?)`, time.Now())

	app := logsApp("super_admin", a.OrgID, nil)
	st, out, raw := lcall(t, app, fmt.Sprintf("/admin/customers/%d/activity", cust.ID))
	if st != 200 {
		t.Fatalf("status %d: %s", st, raw)
	}
	if strings.Contains(raw, "SECRET FROM ANOTHER ISP") {
		t.Fatal("another ISP's SMS to the same phone number appears in this customer's timeline")
	}
	kinds := map[string]int{}
	items := out["data"].(map[string]interface{})["items"].([]interface{})
	for _, it := range items {
		kinds[it.(map[string]interface{})["kind"].(string)]++
	}
	for k, want := range map[string]int{"payment": 1, "credit": 1, "sms": 1, "session": 2, "sign_in": 1} {
		if kinds[k] != want {
			t.Errorf("%s items = %d, want %d (all: %v)", k, kinds[k], want, kinds)
		}
	}
	// Newest first.
	var last time.Time
	for i, it := range items {
		ts, _ := time.Parse(time.RFC3339Nano, it.(map[string]interface{})["time"].(string))
		if i > 0 && ts.After(last) {
			t.Errorf("timeline out of order at %d", i)
		}
		last = ts
	}
	if !strings.Contains(raw, "Online now") || !strings.Contains(raw, "Sign-in refused") {
		t.Error("expected the live session and the refused sign-in in the timeline")
	}

	// Another ISP's customer is simply not found.
	if st, _, _ := lcall(t, app, fmt.Sprintf("/admin/customers/%d/activity", other.ID)); st != 404 {
		t.Errorf("another ISP's customer: %d, want 404", st)
	}
}

func TestQueueTuningScript_ScopedValidatedAndRemembered(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	app := logsApp("super_admin", a.OrgID, nil)

	st, _, raw := lcall(t, app, fmt.Sprintf("/admin/zones/%d/queue-tuning-script?down=50&up=20", a.ZoneID))
	if st != 200 || !strings.Contains(raw, "max-limit=18400k/46000k") {
		t.Fatalf("status %d: %s", st, raw)
	}
	var z models.Zone
	config.DB.First(&z, a.ZoneID)
	if z.UplinkDownMbps != 50 || z.UplinkUpMbps != 20 {
		t.Errorf("uplink not remembered: %d/%d", z.UplinkDownMbps, z.UplinkUpMbps)
	}
	if st, _, _ := lcall(t, app, fmt.Sprintf("/admin/zones/%d/queue-tuning-script?down=50&up=20", b.ZoneID)); st != 404 {
		t.Errorf("another ISP's zone: %d, want 404", st)
	}
	if st, _, _ := lcall(t, app, fmt.Sprintf("/admin/zones/%d/queue-tuning-script?down=0&up=20", a.ZoneID)); st != 422 {
		t.Errorf("missing speed: %d, want 422", st)
	}
}

func radiusPlatformApp() *fiber.App {
	app := fiber.New()
	p := app.Group("/platform")
	p.Get("/radius/status", PlatformRadiusStatus)
	p.Post("/zones/:id/radius", PlatformZoneRadiusSet)
	p.Get("/zones/:id/radius-script", PlatformZoneRadiusScript)
	return app
}

func rpost(t *testing.T, app *fiber.App, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<16)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

func TestPlatformRadius_EnableDisableAndScript(t *testing.T) {
	setupC2BTestDB(t)
	InitRadiusService(services.NewRadiusService())
	old := config.Config.RadiusServerAddr
	config.Config.RadiusServerAddr = "10.200.0.1" // the default the real config loader applies
	t.Cleanup(func() { config.Config.RadiusServerAddr = old })
	i := seedISP(t, "a", "", 1)
	config.DB.Model(&models.Zone{}).Where("id = ?", i.ZoneID).Update("router_ip", "10.200.0.2")
	app := radiusPlatformApp()
	path := fmt.Sprintf("/platform/zones/%d/radius", i.ZoneID)

	// Without the RADIUS server's tables there is nothing to enable.
	services.ResetRadiusTableCache()
	if st, _ := rpost(t, app, path, `{"enabled":true}`); st != 422 {
		t.Errorf("enable before RADIUS is installed: %d, want 422", st)
	}

	createRadiusSchema(t)
	cust := seedCustomer(t, i.ZoneID, "Alice", "254711000001", "ZYR#A")
	mac := "aa:bb:cc:dd:ee:01"
	exp := time.Now().Add(24 * time.Hour)
	config.DB.Model(&cust).Updates(map[string]interface{}{"mac_address": mac, "expires_at": exp, "status": "active"})

	st, body := rpost(t, app, path, `{"enabled":true}`)
	if st != 200 || !strings.Contains(body, `"auth_mode":"radius"`) || !strings.Contains(body, "server_reload_needed") {
		t.Fatalf("enable: %d %s", st, body)
	}
	var z models.Zone
	config.DB.First(&z, i.ZoneID)
	if z.AuthMode != "radius" || len(z.RadiusSecret) < 16 {
		t.Fatalf("zone after enable: mode=%q secret len=%d", z.AuthMode, len(z.RadiusSecret))
	}
	if strings.Contains(body, z.RadiusSecret) {
		t.Error("the RADIUS secret was returned by the enable call")
	}
	var nas int64
	config.DB.Table("nas").Where("nasname = ? AND secret = ?", "10.200.0.2", z.RadiusSecret).Count(&nas)
	if nas != 1 {
		t.Error("the router should be registered as a RADIUS client")
	}
	var chk int64
	config.DB.Table("radcheck").Where("username = ?", "AA:BB:CC:DD:EE:01").Count(&chk)
	if chk == 0 {
		t.Error("enabling should immediately sync the zone's paid-up customers")
	}

	// The status endpoint reports the zone but never its secret.
	_, out, raw := lcall(t, app, "/platform/radius/status")
	if strings.Contains(raw, z.RadiusSecret) {
		t.Error("the secret appears in the status response")
	}
	if out["data"].(map[string]interface{})["installed"] != true {
		t.Errorf("status: %s", raw)
	}

	// The script carries the secret (it must, to configure the router).
	_, _, script := lcall(t, app, fmt.Sprintf("/platform/zones/%d/radius-script", i.ZoneID))
	if !strings.Contains(script, `secret="`+z.RadiusSecret+`"`) || !strings.Contains(script, "use-radius=yes") {
		t.Errorf("script: %s", script)
	}
	_, _, rollback := lcall(t, app, fmt.Sprintf("/platform/zones/%d/radius-script?rollback=1", i.ZoneID))
	if !strings.Contains(rollback, "use-radius=no") || strings.Contains(rollback, z.RadiusSecret) {
		t.Errorf("rollback script: %s", rollback)
	}

	// Disabling removes the credentials and the client entry.
	if st, _ := rpost(t, app, path, `{"enabled":false}`); st != 200 {
		t.Fatalf("disable: %d", st)
	}
	config.DB.Table("radcheck").Where("username = ?", "AA:BB:CC:DD:EE:01").Count(&chk)
	config.DB.Table("nas").Count(&nas)
	config.DB.First(&z, i.ZoneID)
	if chk != 0 || nas != 0 || z.AuthMode != "api" {
		t.Errorf("after disable: radcheck=%d nas=%d mode=%q", chk, nas, z.AuthMode)
	}
}

func TestPlatformRadius_ZoneNeedsARouterAddress(t *testing.T) {
	setupC2BTestDB(t)
	InitRadiusService(services.NewRadiusService())
	createRadiusSchema(t)
	i := seedISP(t, "a", "", 1)
	config.DB.Model(&models.Zone{}).Where("id = ?", i.ZoneID).Update("router_ip", "")
	if st, _ := rpost(t, radiusPlatformApp(), fmt.Sprintf("/platform/zones/%d/radius", i.ZoneID), `{"enabled":true}`); st != 422 {
		t.Errorf("zone with no router address: %d, want 422", st)
	}
}
