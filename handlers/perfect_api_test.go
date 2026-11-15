package handlers

import (
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
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// perfectApp mounts the endpoints changed on this branch behind the real
// AdminAuth (role, zone and ISP come from the database).
func perfectApp() *fiber.App {
	app := fiber.New()
	app.Post("/otp", RequestOtp)
	app.Post("/verify", VerifyOtp)
	app.Post("/tickets", TicketStorePublic)
	app.Get("/tickets/status", TicketStatusPublic)
	app.Get("/receipts/:token", PaymentReceiptPublic)
	app.Post("/verify-code", PaymentVerifyCode)
	adm := app.Group("/a", middleware.AdminAuth())
	adm.Get("/users", UserIndex)
	adm.Get("/vouchers", VoucherIndex)
	adm.Post("/payments/manual", PaymentRecordManual)
	adm.Post("/customers", CustomerStore)
	adm.Post("/customers/:id/suspend", CustomerSuspend)
	adm.Get("/reports/revenue", middleware.RequireRoles("super_admin", "finance", "zone_manager"), ReportRevenue)
	adm.Get("/reports/vouchers", middleware.RequireRoles("super_admin", "finance", "zone_manager"), ReportVouchers)
	adm.Put("/zones/:id", ZoneUpdate)
	return app
}

func zoneStaff(t *testing.T, orgID uint, role string, zoneID *uint) (models.User, string) {
	t.Helper()
	u, tok := staffUser(t, orgID, role)
	if zoneID != nil {
		config.DB.Model(&models.User{}).Where("id = ?", u.ID).Update("zone_id", *zoneID)
		u.ZoneID = zoneID
	}
	return u, tok
}

func zonesOf(t *testing.T, orgID uint) []uint {
	var ids []uint
	config.DB.Model(&models.Zone{}).Where("organization_id = ?", orgID).Order("id").Pluck("id", &ids)
	return ids
}

// ---- 1. OTP / portal phone login is per ISP ----

func TestOtp_PhoneLookupIsPerISP(t *testing.T) {
	_, a, b := setupAgentTest(t)
	config.DB.AutoMigrate(&models.OtpCode{})
	t.Setenv("ALLOW_SANDBOX_OTP", "true") // code 1234, never in production
	app := perfectApp()
	putSetting(t, "sms_enable_otp", "no")
	ca := seedCustomer(t, a.ZoneID, "Alice A", "254711222333", "ZYR#A")
	cb := seedCustomer(t, b.ZoneID, "Alice B", "254711222333", "ZYR#B")
	otpRequestStore.Delete("254711222333")

	// Two ISPs have this phone and no zone was sent: refused, not guessed.
	if st, out := agentCall(t, app, "POST", "/otp", "", `{"phone":"0711222333"}`); st != 409 {
		t.Fatalf("ambiguous phone without zone: %d %v, want 409", st, out)
	}
	// With the zone, each ISP's sign-in lands on its own account.
	for _, tc := range []struct {
		zone uint
		want uint
	}{{a.ZoneID, ca.ID}, {b.ZoneID, cb.ID}} {
		otpRequestStore.Delete("254711222333")
		if st, out := agentCall(t, app, "POST", "/otp", "", fmt.Sprintf(`{"phone":"0711222333","zone_id":"%d"}`, tc.zone)); st != 200 {
			t.Fatalf("otp zone %d: %d %v", tc.zone, st, out)
		}
		st, out := agentCall(t, app, "POST", "/verify", "", fmt.Sprintf(`{"phone":"0711222333","otp":"1234","zone_id":"%d"}`, tc.zone))
		d, _ := out["data"].(map[string]interface{})
		cust, _ := d["customer"].(map[string]interface{})
		if st != 200 || cust == nil || uint(cust["id"].(float64)) != tc.want {
			t.Fatalf("verify zone %d: %d %v, want customer %d", tc.zone, st, out, tc.want)
		}
	}
	if st, _ := agentCall(t, app, "POST", "/verify", "", `{"phone":"0711222333","otp":"1234"}`); st != 409 {
		t.Errorf("verify without zone for a two-ISP phone: %d, want 409", st)
	}
	if n := count(t, &models.Customer{}); n != 2 {
		t.Errorf("%d customers, want still 2 (no duplicate sign-ups)", n)
	}

	// A phone only one ISP knows still works without a zone (old portal pages).
	only := seedCustomer(t, a.ZoneID, "Solo", "254799000111", "ZYR#S")
	st, out := agentCall(t, app, "POST", "/verify", "", `{"phone":"0799000111","otp":"1234"}`)
	d, _ := out["data"].(map[string]interface{})
	cust, _ := d["customer"].(map[string]interface{})
	if st != 200 || cust == nil || uint(cust["id"].(float64)) != only.ID {
		t.Errorf("single-ISP phone without zone: %d %v", st, out)
	}
}

func TestPublicTickets_LinkInsideISPAndStatusLeaksNothing(t *testing.T) {
	_, a, b := setupAgentTest(t)
	config.DB.AutoMigrate(&models.Ticket{})
	app := perfectApp()
	seedCustomer(t, a.ZoneID, "A", "254711222333", "ZYR#A")
	cb := seedCustomer(t, b.ZoneID, "B", "254711222333", "ZYR#B")
	st, out := agentCall(t, app, "POST", "/tickets", "", fmt.Sprintf(`{"zone_id":%d,"name":"x","phone":"0711222333","subject":"slow","message":"secret details","customer_id":1}`, b.ZoneID))
	if st != 201 {
		t.Fatalf("ticket: %d %v", st, out)
	}
	var tk models.Ticket
	config.DB.Last(&tk)
	if tk.CustomerID == nil || *tk.CustomerID != cb.ID || tk.OrganizationID != b.OrgID {
		t.Fatalf("ticket linked to %v org %d, want customer %d org %d", tk.CustomerID, tk.OrganizationID, cb.ID, b.OrgID)
	}
	config.DB.Model(&tk).Update("internal_notes", "staff only")

	if st, _ := agentCall(t, app, "GET", fmt.Sprintf("/tickets/status?ticket_id=%d", tk.ID), "", ""); st != 400 {
		t.Errorf("status by id alone: %d, want 400 (phone required)", st)
	}
	req := httptest.NewRequest("GET", "/tickets/status?phone=0711222333", nil)
	resp, _ := app.Test(req, -1)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "slow") ||
		strings.Contains(string(raw), "secret details") || strings.Contains(string(raw), "staff only") {
		t.Errorf("public status leaks: %d %s", resp.StatusCode, raw)
	}
}

// ---- 5. Users dropdown for zone managers ----

func TestUsers_ZoneManagerGetsScopedReadOnlyList(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	app := perfectApp()
	zs := zonesOf(t, a.OrgID)
	zm, zmTok := zoneStaff(t, a.OrgID, "zone_manager", &zs[0])
	mate, _ := zoneStaff(t, a.OrgID, "field_agent", &zs[0])
	other, _ := zoneStaff(t, a.OrgID, "field_agent", &zs[1])
	_, faTok := zoneStaff(t, a.OrgID, "field_agent", &zs[0])

	st, out := agentCall(t, app, "GET", "/a/users?all=1", zmTok, "")
	if st != 200 {
		t.Fatalf("zone manager /users: %d %v", st, out)
	}
	got := map[uint]bool{}
	for _, r := range out["data"].([]interface{}) {
		m := r.(map[string]interface{})
		got[uint(m["id"].(float64))] = true
		if _, leaked := m["email"]; leaked {
			t.Errorf("slim list carries email: %v", m)
		}
	}
	if !got[zm.ID] || !got[mate.ID] || got[other.ID] {
		t.Errorf("zone manager sees %v; want self %d + zone mate %d, not %d", got, zm.ID, mate.ID, other.ID)
	}
	if st, _ := agentCall(t, app, "GET", "/a/users", faTok, ""); st != 403 {
		t.Errorf("field agent /users: %d, want 403", st)
	}
	// Without a zone a zone manager sees only themselves.
	lone, loneTok := zoneStaff(t, a.OrgID, "zone_manager", nil)
	_, out = agentCall(t, app, "GET", "/a/users?all=1", loneTok, "")
	if rows := out["data"].([]interface{}); len(rows) != 1 || uint(rows[0].(map[string]interface{})["id"].(float64)) != lone.ID {
		t.Errorf("unassigned zone manager sees %v", rows)
	}
}

// ---- 8. Reports scoping ----

func TestReports_RolesAndZoneScope(t *testing.T) {
	_, a, b := setupAgentTest(t)
	app := perfectApp()
	zs := zonesOf(t, a.OrgID)
	now := time.Now()
	for _, p := range []models.Payment{
		{ZoneID: zs[0], Phone: "254700000001", Amount: 100, Method: "mpesa", Status: "completed", CreatedAt: now},
		{ZoneID: zs[1], Phone: "254700000002", Amount: 30, Method: "mpesa", Status: "completed", CreatedAt: now},
		{ZoneID: b.ZoneID, Phone: "254700000003", Amount: 999, Method: "mpesa", Status: "completed", CreatedAt: now},
	} {
		p := p
		config.DB.Create(&p)
	}
	total := func(tok string) (int, string) {
		st, out := agentCall(t, app, "GET", "/a/reports/revenue", tok, "")
		d, _ := out["data"].(map[string]interface{})
		s, _ := d["summary"].(map[string]interface{})
		if s == nil {
			return st, ""
		}
		return st, fmt.Sprint(s["total_revenue"])
	}
	_, owner := staffUser(t, a.OrgID, "super_admin")
	_, fin := staffUser(t, a.OrgID, "finance")
	_, zm := zoneStaff(t, a.OrgID, "zone_manager", &zs[1])
	_, lone := zoneStaff(t, a.OrgID, "zone_manager", nil)
	_, fa := staffUser(t, a.OrgID, "field_agent")
	for _, tc := range []struct {
		name, tok string
		code      int
		want      string
	}{{"owner", owner, 200, "130"}, {"finance", fin, 200, "130"}, {"zone manager", zm, 200, "30"}, {"zone manager without zone", lone, 200, "0"}} {
		if st, got := total(tc.tok); st != tc.code || got != tc.want {
			t.Errorf("%s: %d total %s, want %d %s", tc.name, st, got, tc.code, tc.want)
		}
	}
	for _, p := range []string{"/a/reports/revenue", "/a/reports/vouchers"} {
		if st, _ := agentCall(t, app, "GET", p, fa, ""); st != 403 {
			t.Errorf("field agent %s: %d, want 403", p, st)
		}
	}
}

// ---- Vouchers by role ----

func TestVouchers_ReadByRole(t *testing.T) {
	app0, a, _ := setupAgentTest(t)
	app := perfectApp()
	_, owner := staffUser(t, a.OrgID, "super_admin")
	faUser, faTok := staffUser(t, a.OrgID, "field_agent")
	_, finTok := staffUser(t, a.OrgID, "finance")
	pkg := pkgOf(t, a.ZoneID)
	// Agent linked to the field agent login, issued 2 vouchers; 3 loose ones.
	agentID := newAgent(t, app0, owner, a.ZoneID, fmt.Sprintf(`,"user_id":%d,"credit_limit_cents":100000`, faUser.ID))
	if st, out := agentCall(t, app0, "POST", fmt.Sprintf("/a/agents/%d/issue", agentID), owner,
		fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":2,"funding":"credit","idempotency_key":"k-issue-0001"}`, a.ZoneID, pkg.ID)); st != 201 && st != 200 {
		t.Fatalf("issue: %d %v", st, out)
	}
	for i := 0; i < 3; i++ {
		if _, err := voucherSvcGlobal.Generate(a.ZoneID, pkg.ID, "single_use", 1); err != nil {
			t.Fatal(err)
		}
	}
	n := func(tok string) (int, int) {
		st, out := agentCall(t, app, "GET", "/a/vouchers", tok, "")
		rows, _ := out["data"].([]interface{})
		return st, len(rows)
	}
	var all int64
	config.DB.Model(&models.Voucher{}).Count(&all)
	if st, got := n(owner); st != 200 || got != 5 || all != 5 {
		t.Errorf("owner: %d sees %d (db %d), want 5", st, got, all)
	}
	if st, got := n(faTok); st != 200 || got != 2 {
		t.Errorf("field agent: %d sees %d, want only their 2", st, got)
	}
	if st, _ := n(finTok); st != 403 {
		t.Errorf("finance: %d, want 403", st)
	}
}

// ---- 10. Public receipt ----

func TestPublicReceipt_TokenReadOnlyNoPII(t *testing.T) {
	setupTenantTest(t)
	a := seedISP(t, "acme", "", 1)
	c := seedCustomer(t, a.ZoneID, "<script>alert(1)</script>Jane", "254711999888", "ZYR#J")
	rc := "UKC6G1KTH9"
	p := models.Payment{ZoneID: a.ZoneID, CustomerID: &c.ID, Phone: "254711999888", Amount: 50, Method: "mpesa", Provider: "cyzora", Status: "completed", MpesaReceiptNumber: &rc}
	config.DB.Create(&p)
	pending := models.Payment{ZoneID: a.ZoneID, Phone: "254711999888", Amount: 50, Method: "mpesa", Status: "pending"}
	config.DB.Create(&pending)
	app := perfectApp()

	get := func(path string) (int, string) {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	st, body := get("/receipts/" + ReceiptToken(p.ID))
	if st != 200 || !strings.Contains(body, "UKC6G1KTH9") || !strings.Contains(body, "KES 50") || !strings.Contains(body, "acme") {
		t.Fatalf("receipt: %d %s", st, body)
	}
	for _, leak := range []string{"Jane", "script", "254711999888", "ZYR#J"} {
		if strings.Contains(body, leak) {
			t.Errorf("receipt shows %q", leak)
		}
	}
	if !strings.Contains(body, "2547*****888") {
		t.Errorf("phone should be masked: %s", body)
	}
	for _, bad := range []string{fmt.Sprint(p.ID), fmt.Sprintf("%d-%s", p.ID, strings.Repeat("A", 24)), "/receipts/" + ReceiptToken(pending.ID)} {
		path := bad
		if !strings.HasPrefix(path, "/") {
			path = "/receipts/" + bad
		}
		if st, _ := get(path); st != 404 {
			t.Errorf("%s: %d, want 404", path, st)
		}
	}
	// The token of one payment never opens another.
	tok := ReceiptToken(p.ID)
	if _, ok := paymentFromReceiptToken(fmt.Sprint(pending.ID) + tok[strings.Index(tok, "-"):]); ok {
		t.Error("a token was accepted for another payment id")
	}
}

// ---- Manual payments + C2B double credit ----

func TestManualPayment_ZoneScopeAndLedgerDuplicate(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	app := perfectApp()
	zs := zonesOf(t, a.OrgID)
	other := seedCustomer(t, zs[1], "Other zone", "254700111222", "ZYR#O")
	_, zm := zoneStaff(t, a.OrgID, "zone_manager", &zs[0])
	st, _ := agentCall(t, app, "POST", "/a/payments/manual", zm, fmt.Sprintf(`{"customer_id":%d,"zone_id":%d,"amount":500,"action":"credit"}`, other.ID, zs[0]))
	if st != 404 {
		t.Errorf("zone manager crediting another zone's customer: %d, want 404", st)
	}
	if bal := services.CreditBalanceOf(config.DB, other.ID); bal != 0 {
		t.Fatalf("credit leaked: %v", bal)
	}

	// A paybill confirmation of this code is still being verified: recording it
	// by hand too would credit it twice.
	config.DB.Create(&models.C2BTransaction{TransID: "QAB12CD34E", Amount: 50, Status: "verifying"})
	_, owner := staffUser(t, a.OrgID, "super_admin")
	st, out := agentCall(t, app, "POST", "/a/payments/manual", owner, fmt.Sprintf(`{"customer_id":%d,"zone_id":%d,"amount":50,"action":"credit","mpesa_receipt_number":"qab12cd34e"}`, other.ID, zs[1]))
	if st != 409 {
		t.Errorf("code in the C2B ledger: %d %v, want 409", st, out)
	}
	// A normal credit: one payment, one credit, booked to the customer's zone.
	st, out = agentCall(t, app, "POST", "/a/payments/manual", owner, fmt.Sprintf(`{"customer_id":%d,"zone_id":%d,"amount":70,"action":"credit"}`, other.ID, zs[0]))
	if st != 201 {
		t.Fatalf("owner credit: %d %v", st, out)
	}
	var p models.Payment
	config.DB.Last(&p)
	if bal := services.CreditBalanceOf(config.DB, other.ID); bal != 70 || p.ZoneID != zs[1] || p.Amount != 70 {
		t.Errorf("balance %v payment zone %d amount %v; want 70 on zone %d", bal, p.ZoneID, p.Amount, zs[1])
	}
}

func TestC2B_StaffRecordedFirstIsNotCreditedAgain(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "600111", 1)
	c := seedCustomer(t, a.ZoneID, "Jane", "254711000009", "ZYR#J9")
	ref := "QZZ99YY88X"
	config.DB.Create(&models.Payment{CustomerID: &c.ID, ZoneID: a.ZoneID, Phone: c.Phone, Amount: 50, Method: "manual", Status: "completed", MpesaReceiptNumber: &ref})
	l := models.C2BTransaction{TransID: ref, Amount: 50, BusinessShortCode: "600111", BillRefNumber: "ZYR#J9", MSISDN: "254711000009", Status: "verifying", Verification: "status_query"}
	config.DB.Create(&l)
	creditC2B(&l)
	if bal := services.CreditBalanceOf(config.DB, c.ID); bal != 0 {
		t.Errorf("credited again after staff recorded it: balance %v", bal)
	}
	config.DB.First(&l, l.ID)
	if l.Status != "credited" {
		t.Errorf("ledger row %s, want credited (closed)", l.Status)
	}
}

func TestVerifyCode_OnlyThePayersAccountGetsIt(t *testing.T) {
	setupC2BTestDB(t)
	initMoneyServices(t, "")
	a := seedISP(t, "a", "", 1)
	payer := seedCustomer(t, a.ZoneID, "Payer", "254711000001", "ZYR#P")
	thief := seedCustomer(t, a.ZoneID, "Other", "254722000002", "ZYR#T")
	config.DB.Create(&models.UnmatchedC2BPayment{TransID: "QVV11WW22Z", Phone: "254711000001", Amount: 50, Status: "pending", Verification: "verified"})
	app := perfectApp()
	tok, _ := middleware.GenerateCustomerToken(thief.ID)
	st, _ := agentCall(t, app, "POST", "/verify-code", tok, `{"code":"QVV11WW22Z"}`)
	if st != 200 {
		t.Fatalf("verify-code: %d", st)
	}
	if bal := services.CreditBalanceOf(config.DB, thief.ID); bal != 0 {
		t.Fatalf("another signed-in customer got the payment: %v", bal)
	}
	if bal := services.CreditBalanceOf(config.DB, payer.ID); bal != 50 {
		t.Errorf("payer balance %v, want 50", bal)
	}
	if utils.FormatPhone(payer.Phone) == "" || phoneTail9("2547***001") != "" {
		t.Error("masked numbers must never match")
	}
}

// ---- Other authz fixes ----

func TestCustomerStore_CannotMintCreditAndSuspendNeedsManager(t *testing.T) {
	_, a, b := setupAgentTest(t)
	app := perfectApp()
	_, owner := staffUser(t, a.OrgID, "super_admin")
	st, out := agentCall(t, app, "POST", "/a/customers", owner, fmt.Sprintf(`{"name":"N","phone":"254733000111","zone_id":%d,"package_id":%d,"type":"hotspot","status":"expired","credit_balance":5000}`, a.ZoneID, pkgOf(t, a.ZoneID).ID))
	if st != 201 {
		t.Fatalf("create: %d %v", st, out)
	}
	var c models.Customer
	config.DB.Where("phone = ?", "254733000111").First(&c)
	if c.CreditBalance != 0 {
		t.Errorf("credit minted on create: %v", c.CreditBalance)
	}
	if st, _ := agentCall(t, app, "POST", "/a/customers", owner, fmt.Sprintf(`{"name":"N2","phone":"254733000112","zone_id":%d,"package_id":%d,"type":"hotspot"}`, a.ZoneID, pkgOf(t, b.ZoneID).ID)); st != 422 {
		t.Errorf("another ISP's package: %d, want 422", st)
	}
	for _, role := range []string{"field_agent", "finance"} {
		_, tok := staffUser(t, a.OrgID, role)
		if st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/customers/%d/suspend", c.ID), tok, ""); st != 403 {
			t.Errorf("%s suspend: %d, want 403", role, st)
		}
	}
	zid := a.ZoneID
	_, faZone := zoneStaff(t, a.OrgID, "field_agent", &zid)
	if st, _ := agentCall(t, app, "PUT", fmt.Sprintf("/a/zones/%d", a.ZoneID), faZone, `{"name":"pwned"}`); st != 403 {
		t.Errorf("field agent with a zone editing the router: %d, want 403", st)
	}
}

// ---- 3. Test STK follows the ISP's provider ----

func TestStkTest_FollowsPaymentProvider(t *testing.T) {
	setupC2BTestDB(t)
	initMoneyServices(t, "")
	oldEnv, oldM := config.Config.AppEnv, config.Config.MpesaEnv
	config.Config.AppEnv, config.Config.MpesaEnv = "test", "mock"
	t.Cleanup(func() { config.Config.AppEnv, config.Config.MpesaEnv = oldEnv, oldM })
	a := seedISP(t, "a", "", 1)
	config.DB.Model(&models.Organization{}).Where("id = ?", a.OrgID).Update("payment_provider", services.ProviderCyzora)
	out, err := runStkTest(a.ZoneID, stkTestBody{Phone: "0712345678", Amount: 1})
	if err != nil || out["provider"] != "cyzora" || out["is_mock"] != true {
		t.Fatalf("cyzora ISP: %v %v", out, err)
	}
	if n := count(t, &models.Payment{}); n != 0 {
		t.Errorf("a test prompt made %d payment rows", n)
	}
}

// ---- 2/6. Kenyan days, not the UTC server's ----

func TestDates_AreKenyanDays(t *testing.T) {
	want := time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC) // 00:00 EAT 15 Nov
	if got, err := parseCustomerDateTime("2026-11-15", false); err != nil || !got.Equal(want) {
		t.Errorf("customer filter from: %v %v, want %v", got, err, want)
	}
	if got, _ := parseCustomerDateTime("2026-11-15", true); !got.Equal(want.Add(24*time.Hour - time.Nanosecond)) {
		t.Errorf("customer filter to: %v", got)
	}
	if got, ok := parseDay("2026-11-15", false); !ok || !got.Equal(want) {
		t.Errorf("logs from: %v", got)
	}
	start, end := PreviousCalendarMonth()
	if start.In(utils.EAT).Day() != 1 || start.In(utils.EAT).Hour() != 0 || end.In(utils.EAT).Hour() != 0 {
		t.Errorf("invoice month %v – %v is not EAT midnight", start, end)
	}
}

// The platform overview's "today" starts at 00:00 EAT (21:00 UTC), not at
// the UTC server's midnight (03:00 EAT).
func TestPlatformOverview_TodayIsKenyan(t *testing.T) {
	setupC2BTestDB(t)
	config.DB.AutoMigrate(&models.PlatformInvoice{})
	a := seedISP(t, "a", "", 1)
	k := utils.Kenya(time.Now())
	midnight := time.Date(k.Year(), k.Month(), k.Day(), 0, 0, 0, 0, utils.EAT)
	early := midnight.Add(30 * time.Minute)      // 00:30 EAT today
	yesterday := midnight.Add(-30 * time.Minute) // 23:30 EAT yesterday
	if early.After(time.Now()) {
		t.Skip("too close to midnight EAT")
	}
	config.DB.Create(&models.Payment{ZoneID: a.ZoneID, Phone: "1", Amount: 7, Method: "mpesa", Status: "completed", CreatedAt: early.In(time.Local)})
	config.DB.Create(&models.Payment{ZoneID: a.ZoneID, Phone: "2", Amount: 5, Method: "mpesa", Status: "completed", CreatedAt: yesterday.In(time.Local)})
	app := fiber.New()
	app.Get("/o", PlatformOverview)
	_, out := agentCall(t, app, "GET", "/o", "", "")
	d, _ := out["data"].(map[string]interface{})
	if d == nil || fmt.Sprint(d["today_revenue"]) != "7" {
		t.Errorf("today_revenue = %v, want 7 (00:30 EAT counts, 23:30 EAT yesterday doesn't)", d["today_revenue"])
	}
}
