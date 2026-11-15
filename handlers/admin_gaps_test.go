package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
)

func gapsApp() *fiber.App {
	app := fiber.New()
	adm := app.Group("/a", middleware.AdminAuth())
	adm.Get("/payments", PaymentIndex)
	adm.Get("/customers", CustomerIndex)
	adm.Get("/vouchers", VoucherIndex)
	adm.Get("/zones/:id", ZoneShow)
	adm.Get("/reports/zones", ReportZones)
	adm.Get("/reports/vouchers", ReportVouchers)
	return app
}

func rawCall(t *testing.T, app *fiber.App, path, tok string) (int, string) {
	t.Helper()
	st, out := agentCall(t, app, "GET", path, tok, "")
	return st, fmt.Sprint(out)
}

// 1. Credit plans are never takings in the payments summary.
func TestPaymentsSummary_ExcludesCredit(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	for _, p := range []models.Payment{
		{ZoneID: a.ZoneID, Phone: "1", Amount: 100, Method: "mpesa", Status: "completed"},
		{ZoneID: a.ZoneID, Phone: "1", Amount: 30, Method: "mpesa_c2b", Status: "completed"},
		{ZoneID: a.ZoneID, Phone: "1", Amount: 49, Method: "credit", Status: "completed"},
	} {
		p := p
		config.DB.Create(&p)
	}
	_, out := agentCall(t, gapsApp(), "GET", "/a/payments", owner, "")
	sum, _ := out["summary"].(map[string]interface{})
	if sum == nil || fmt.Sprint(sum["completed_amount"]) != "130" || fmt.Sprint(sum["completed_count"]) != "2" {
		t.Errorf("summary %v, want 130 from 2 (credit excluded)", sum)
	}
}

// 2. List rows carry the zone's id + name, never its router details.
func TestLists_ZoneIsBriefButZonePageIsFull(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	user := "router-admin-user"
	config.DB.Model(&models.Zone{}).Where("id = ?", a.ZoneID).Updates(map[string]interface{}{"router_ip": "10.9.8.7", "router_username": user, "router_port": 8799})
	seedCustomer(t, a.ZoneID, "C", "254700000001", "ZYR#C")
	config.DB.Create(&models.Payment{ZoneID: a.ZoneID, Phone: "1", Amount: 10, Method: "mpesa", Status: "completed"})
	if _, err := voucherSvcGlobal.Generate(a.ZoneID, pkgOf(t, a.ZoneID).ID, "single_use", 1); err != nil {
		t.Fatal(err)
	}
	_, owner := staffUser(t, a.OrgID, "super_admin")
	app := gapsApp()
	for _, p := range []string{"/a/customers", "/a/payments", "/a/vouchers"} {
		st, body := rawCall(t, app, p, owner)
		if st != 200 || strings.Contains(body, "10.9.8.7") || strings.Contains(body, user) || strings.Contains(body, "8799") || !strings.Contains(body, "acme-z0") {
			t.Errorf("%s leaks router details or lost the zone name: %d %s", p, st, body)
		}
	}
	if _, body := rawCall(t, app, fmt.Sprintf("/a/zones/%d", a.ZoneID), owner); !strings.Contains(body, "10.9.8.7") {
		t.Errorf("the zone page itself must still have the router address: %s", body)
	}
}

// 3. Zone comparison respects the EAT date range and the zone filter.
func TestReportZones_DateRangeAndZone(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	var zs []uint
	config.DB.Model(&models.Zone{}).Where("organization_id = ?", a.OrgID).Order("id").Pluck("id", &zs)
	day := func(s string, hh int) time.Time {
		d, _ := time.ParseInLocation("2006-01-02", s, time.FixedZone("EAT", 3*3600))
		return d.Add(time.Duration(hh) * time.Hour).In(time.Local)
	}
	for _, p := range []models.Payment{
		{ZoneID: zs[0], Phone: "1", Amount: 100, Method: "mpesa", Status: "completed", CreatedAt: day("2026-11-10", 1)}, // 01:00 EAT 10 Nov
		{ZoneID: zs[0], Phone: "1", Amount: 7, Method: "mpesa", Status: "completed", CreatedAt: day("2026-11-09", 23)},
		{ZoneID: zs[1], Phone: "1", Amount: 50, Method: "mpesa", Status: "completed", CreatedAt: day("2026-11-10", 12)},
	} {
		p := p
		config.DB.Create(&p)
	}
	_, owner := staffUser(t, a.OrgID, "super_admin")
	rev := func(q string) map[float64]float64 {
		_, out := agentCall(t, gapsApp(), "GET", "/a/reports/zones"+q, owner, "")
		m := map[float64]float64{}
		rows, _ := out["data"].([]interface{})
		for _, r := range rows {
			x := r.(map[string]interface{})
			m[x["zone_id"].(float64)] = x["revenue"].(float64)
		}
		return m
	}
	if got := rev("?date_from=2026-11-10&date_to=2026-11-10"); got[float64(zs[0])] != 100 || got[float64(zs[1])] != 50 {
		t.Errorf("10 Nov EAT: %v, want zone0 100 (not the 23:00 9 Nov 7) and zone1 50", got)
	}
	if got := rev(fmt.Sprintf("?date_from=2026-11-10&date_to=2026-11-10&zone_id=%d", zs[1])); len(got) != 1 || got[float64(zs[1])] != 50 {
		t.Errorf("zone filter: %v", got)
	}
	if got := rev(""); got[float64(zs[0])] != 107 {
		t.Errorf("all time: %v", got)
	}
	if st, _ := agentCall(t, gapsApp(), "GET", "/a/reports/zones?date_from=nope", owner, ""); st != 422 {
		t.Errorf("bad date: %d", st)
	}
}

// 4. The voucher report separates paid sales from free vouchers.
func TestReportVouchers_PaidVersusFree(t *testing.T) {
	app0, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	pkg := pkgOf(t, a.ZoneID)
	mk := func(status string) models.Voucher {
		v, err := voucherSvcGlobal.Generate(a.ZoneID, pkg.ID, "single_use", 1)
		if err != nil {
			t.Fatal(err)
		}
		config.DB.Model(&models.Voucher{}).Where("id = ?", v.ID).Update("status", status)
		return *v
	}
	paid := mk("depleted") // bought by M-Pesa
	config.DB.Create(&models.Payment{ZoneID: a.ZoneID, VoucherID: &paid.ID, Phone: "1", Amount: 100, Method: "mpesa", Status: "completed"})
	mk("depleted") // free, used
	mk("unused")   // free, unused
	agentID := newAgent(t, app0, owner, a.ZoneID, "")
	if st, out := agentCall(t, app0, "POST", fmt.Sprintf("/a/agents/%d/issue", agentID), owner,
		fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":1,"funding":"credit","idempotency_key":"gap-issue-0001"}`, a.ZoneID, pkg.ID)); st != 201 {
		t.Fatalf("issue: %d %v", st, out)
	}
	_, out := agentCall(t, gapsApp(), "GET", "/a/reports/vouchers", owner, "")
	d, _ := out["data"].(map[string]interface{})
	s, _ := d["sales"].(map[string]interface{})
	want := map[string]string{"paid_sold": "1", "paid_used": "1", "free_used": "1", "free_unused": "1", "with_agents": "1"}
	for k, v := range want {
		if s == nil || fmt.Sprint(s[k]) != v {
			t.Errorf("sales[%s] = %v, want %s (all: %v)", k, s[k], v, s)
		}
	}
}

// 5. No invented traffic: a router that doesn't answer is "unavailable",
// even with APP_ENV=local.
func TestInterfaceTraffic_NeverInventedLocally(t *testing.T) {
	old := config.Config.AppEnv
	config.Config.AppEnv = "local"
	t.Cleanup(func() { config.Config.AppEnv = old })
	z := &models.Zone{ID: 1, RouterIP: "127.0.0.2", RouterPort: 1, ConnectionType: "rest"}
	tr, err := services.NewMikroTikService().GetInterfaceTraffic(z, "ether1")
	if err != nil || tr.Available || tr.RxMbps != 0 || tr.TxMbps != 0 {
		t.Errorf("unreachable router: %+v %v, want unavailable with no figures", tr, err)
	}
}

// smsAvailable is true only with OTP login on AND a provider with credentials.
func TestSettingsPublic_SmsAvailable(t *testing.T) {
	setupC2BTestDB(t)
	oldK, oldU := config.Config.HostpinnacleApiKey, config.Config.HostpinnacleUsername
	config.Config.HostpinnacleApiKey, config.Config.HostpinnacleUsername = "", ""
	t.Cleanup(func() { config.Config.HostpinnacleApiKey, config.Config.HostpinnacleUsername = oldK, oldU })
	app := fiber.New()
	app.Get("/s", SettingsPublic)
	avail := func() interface{} {
		_, out := agentCall(t, app, "GET", "/s", "", "")
		return out["data"].(map[string]interface{})["smsAvailable"]
	}
	putSetting(t, "otp_login_enabled", "yes")
	if v := avail(); v != false {
		t.Errorf("OTP on, no SMS credentials: %v, want false", v)
	}
	putSetting(t, "hostpinnacle_api_key", "fake-key-for-test")
	putSetting(t, "hostpinnacle_username", "fake-user")
	if v := avail(); v != true {
		t.Errorf("OTP on + credentials: %v, want true", v)
	}
	putSetting(t, "otp_login_enabled", "no")
	if v := avail(); v != false {
		t.Errorf("OTP off: %v, want false", v)
	}
}
