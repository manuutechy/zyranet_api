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
)

func moneyHolesApp() *fiber.App {
	app := fiber.New()
	adm := app.Group("/a", middleware.AdminAuth())
	adm.Post("/vouchers/generate", VoucherGenerate)
	adm.Get("/payments", PaymentIndex)
	adm.Get("/reports/revenue", ReportRevenue)
	adm.Get("/reports/zones", ReportZones)
	app.Post("/customer/auth/guest", CustomerAuthGuest)
	return app
}

// Free vouchers: only the owner and zone managers, and every batch is logged.
// A field agent's vouchers must come through the agent ledger.
func TestVoucherGenerate_AgentsCannotMintFreeVouchers(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	app := moneyHolesApp()
	body := fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":3}`, a.ZoneID, pkgOf(t, a.ZoneID).ID)
	for _, role := range []string{"field_agent", "finance", "support", "installer"} {
		_, tok := staffUser(t, a.OrgID, role)
		if st, out := agentCall(t, app, "POST", "/a/vouchers/generate", tok, body); st != 403 {
			t.Errorf("%s: want 403, got %d %v", role, st, out)
		}
	}
	var n int64
	config.DB.Model(&models.Voucher{}).Count(&n)
	if n != 0 {
		t.Fatalf("refused roles still made %d vouchers", n)
	}
	_, owner := staffUser(t, a.OrgID, "super_admin")
	if st, out := agentCall(t, app, "POST", "/a/vouchers/generate", owner, body); st != 201 {
		t.Fatalf("owner: %d %v", st, out)
	}
	var logs []models.AuditLog
	config.DB.Where("action = ?", "vouchers_generated_free").Find(&logs)
	if len(logs) != 1 || logs[0].NewValues == nil || !strings.Contains(*logs[0].NewValues, `"face_value_total":300`) {
		t.Errorf("free batch must be logged with its value: %+v", logs)
	}
}

// Buying with account credit is not new money: it was counted when the
// top-up came in. Reports must not count it again.
func TestReports_CreditPurchasesAreNotCountedAsTakings(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	app := moneyHolesApp()
	_, owner := staffUser(t, a.OrgID, "super_admin")
	now := time.Now()
	for _, p := range []models.Payment{
		{ZoneID: a.ZoneID, Phone: "254700000001", Amount: 100, Method: "mpesa", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ZoneID: a.ZoneID, Phone: "254700000001", Amount: 40, Method: "credit", Status: "completed", CreatedAt: now, UpdatedAt: now},
	} {
		p := p
		if err := config.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	st, out := agentCall(t, app, "GET", "/a/reports/revenue", owner, "")
	data, _ := out["data"].(map[string]interface{})
	rs, _ := data["summary"].(map[string]interface{})
	if st != 200 || rs == nil || fmt.Sprint(rs["total_revenue"]) != "100" || fmt.Sprint(rs["total_payments"]) != "1" {
		t.Errorf("revenue report: %d summary=%v (want total 100 from 1 payment)", st, rs)
	}
	st, out = agentCall(t, app, "GET", "/a/payments", owner, "")
	sum, _ := out["summary"].(map[string]interface{})
	if sum == nil {
		if d, ok := out["data"].(map[string]interface{}); ok {
			sum, _ = d["summary"].(map[string]interface{})
		}
	}
	if st != 200 || sum == nil || fmt.Sprint(sum["completed_amount"]) != "100" {
		t.Errorf("payments totals: %d %v (want completed_amount 100)", st, sum)
	}
	// The purchase itself is still in the list (it is the customer's history).
	var rows int64
	config.DB.Model(&models.Payment{}).Where("method = ?", "credit").Count(&rows)
	if rows != 1 {
		t.Error("credit purchase record must be kept")
	}
}

// A new portal account is filed under the WiFi it signed up on, never
// "the first zone in the database".
func TestSignup_FiledUnderTheRightISP(t *testing.T) {
	_, a, b := setupAgentTest(t) // two ISPs
	app := moneyHolesApp()
	post := func(body string) (int, map[string]interface{}) {
		return agentCall(t, app, "POST", "/customer/auth/guest", "", body)
	}
	// No zone, two ISPs: nothing is guessed.
	if st, out := post(`{"mac":"AA:BB:CC:00:00:01"}`); st != 400 || !strings.Contains(fmt.Sprint(out["error"]), "WiFi login screen") {
		t.Fatalf("no zone with two ISPs: %d %v", st, out)
	}
	var n int64
	config.DB.Model(&models.Customer{}).Count(&n)
	if n != 0 {
		t.Fatalf("refused sign-up left %d customers", n)
	}
	// On ISP B's WiFi → ISP B's zone and ISP B's plan.
	if st, out := post(fmt.Sprintf(`{"mac":"AA:BB:CC:00:00:02","zone_id":"%d"}`, b.ZoneID)); st >= 300 {
		t.Fatalf("guest on B: %d %v", st, out)
	}
	var c models.Customer
	if err := config.DB.Order("id DESC").First(&c).Error; err != nil {
		t.Fatal(err)
	}
	var pkg models.Package
	config.DB.First(&pkg, c.PackageID)
	if c.ZoneID != b.ZoneID || pkg.ZoneID != b.ZoneID {
		t.Errorf("filed under zone %d with a plan of zone %d; want ISP B's zone %d (ISP A is %d)", c.ZoneID, pkg.ZoneID, b.ZoneID, a.ZoneID)
	}
}
