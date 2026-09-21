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
	"github.com/zyranet/zyranet-api/services"
)

func setupPayoutTest(t *testing.T) {
	t.Helper()
	setupC2BTestDB(t)
	sms := services.NewSmsService()
	InitPayoutService(services.NewPayoutService(services.NewMpesaService(sms, services.NewVoucherService(sms), services.NewMikroTikService())))
	oldSecret := config.Config.MpesaCallbackSecret
	t.Cleanup(func() { config.Config.MpesaCallbackSecret = oldSecret })
}

func setPlatformSetting(t *testing.T, key, val string) {
	t.Helper()
	v := val
	config.DB.Where("`key` = ?", key).Delete(&models.PlatformSetting{})
	if err := config.DB.Create(&models.PlatformSetting{Key: key, Value: &v}).Error; err != nil {
		t.Fatal(err)
	}
}

// payoutApp mounts the platform payout routes behind a fake staff login.
func payoutApp(staffID uint) *fiber.App {
	app := fiber.New()
	staff := func(c *fiber.Ctx) error {
		c.Locals("claims", &middleware.Claims{PlatformUserID: staffID, Type: "platform"})
		return c.Next()
	}
	p := app.Group("/platform", staff)
	p.Get("/payouts/balances", PlatformPayoutBalances)
	p.Post("/payouts/backfill", PlatformPayoutBackfill)
	p.Get("/payouts", PlatformPayoutIndex)
	p.Post("/payouts", PlatformPayoutStore)
	p.Get("/payouts/:id", PlatformPayoutShow)
	p.Post("/payouts/:id/send", PlatformPayoutSend)
	p.Post("/payouts/:id/mark-paid", PlatformPayoutMarkPaid)
	p.Post("/payouts/:id/cancel", PlatformPayoutCancel)
	p.Post("/payouts/:id/fail", PlatformPayoutFail)
	p.Patch("/organizations/:id", OrganizationUpdate)
	p.Get("/daraja", PlatformDarajaShow)
	app.Post("/payouts/b2b/result", PayoutB2BResult)
	app.Post("/payouts/b2b/timeout", PayoutB2BTimeout)
	adm := app.Group("/admin", func(c *fiber.Ctx) error {
		c.Locals("claims", &middleware.Claims{Role: "super_admin", OrganizationID: lastOrgID, Type: "admin"})
		return c.Next()
	})
	adm.Get("/payouts", OrganizationPayoutsIndex)
	return app
}

var lastOrgID uint

func pcall(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func dataOf(m map[string]interface{}) map[string]interface{} {
	d, _ := m["data"].(map[string]interface{})
	return d
}

// seedSettledISP makes an ISP on the shared shortcode with a bank-paybill
// destination and one platform-collected KES 200 payment.
func seedSettledISP(t *testing.T, slug string) (models.Organization, models.Payment) {
	t.Helper()
	i := seedISP(t, slug, "", 1)
	config.DB.Model(&models.Organization{}).Where("id = ?", i.OrgID).Updates(map[string]interface{}{
		"settlement_type": "paybill", "settlement_paybill_number": "247247", "settlement_account_number": "0123456789",
	})
	var org models.Organization
	config.DB.First(&org, i.OrgID)
	lastOrgID = org.ID
	p := models.Payment{ZoneID: i.ZoneID, Phone: "254711000001", Amount: 200, Method: "mpesa", Status: "completed", CollectedVia: "platform"}
	config.DB.Create(&p)
	return org, p
}

func TestC2B_RecordsWhereTheMoneyLanded(t *testing.T) {
	setupPayoutTest(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "222333", 1) // own shortcode
	seedCustomer(t, a.ZoneID, "A1", "254711000001", "ZYR#A1")
	seedCustomer(t, b.ZoneID, "B1", "254722000002", "ZYR#B1")

	postC2B(t, platformPaybill, "TXP1", "ZYR#A1", "254711000001", 30) // shared paybill
	postC2B(t, "222333", "TXP2", "ZYR#B1", "254722000002", 30)        // ISP's own shortcode

	via := func(receipt string) string {
		var p models.Payment
		config.DB.Where("mpesa_receipt_number = ?", receipt).First(&p)
		return p.CollectedVia
	}
	if got := via("TXP1"); got != "platform" {
		t.Errorf("shared paybill payment collected_via = %q, want platform", got)
	}
	if got := via("TXP2"); got != "own" {
		t.Errorf("own-shortcode payment collected_via = %q, want own (never part of a payout)", got)
	}
}

func TestPayoutsAPI_CreateSendGuardsAndCompletion(t *testing.T) {
	setupPayoutTest(t)
	org, pay := seedSettledISP(t, "acme")
	app := payoutApp(1)

	// Balance shows what is owed and where it goes.
	_, out := pcall(t, app, "GET", "/platform/payouts/balances", "")
	row := dataOf(out)["balances"].([]interface{})[0].(map[string]interface{})
	if row["gross"] != 200.0 || row["payout_amount"] != 200.0 || row["destination"] != "Paybill 247247 / 0123456789" || row["destination_ready"] != true {
		t.Errorf("balance row: %v", row)
	}

	st, out := pcall(t, app, "POST", "/platform/payouts", fmt.Sprintf(`{"organization_id":%d}`, org.ID))
	if st != 201 {
		t.Fatalf("create: %d %v", st, out)
	}
	id := int(dataOf(out)["id"].(float64))

	if st, _ := pcall(t, app, "POST", "/platform/payouts", fmt.Sprintf(`{"organization_id":%d}`, org.ID)); st != 409 && st != 422 {
		t.Errorf("a second payout while one is open: %d, want 409/422", st)
	}
	// Once claimed the money is no longer owed.
	_, out = pcall(t, app, "GET", "/platform/payouts/balances", "")
	row = dataOf(out)["balances"].([]interface{})[0].(map[string]interface{})
	if row["gross"] != 0.0 || row["open_payout_status"] != "pending" {
		t.Errorf("balance after claim: %v", row)
	}

	// Kill switch: sending through M-Pesa is off by default.
	if st, _ := pcall(t, app, "POST", fmt.Sprintf("/platform/payouts/%d/send", id), ""); st != 403 {
		t.Errorf("send with payouts disabled: %d, want 403", st)
	}

	// Two-person rule: the creator can't confirm their own payout.
	setPlatformSetting(t, "payouts_require_second_approver", "yes")
	if st, _ := pcall(t, app, "POST", fmt.Sprintf("/platform/payouts/%d/mark-paid", id), `{"reference":"BANK1"}`); st != 403 {
		t.Errorf("creator confirming own payout: %d, want 403", st)
	}
	other := payoutApp(2)
	if st, _ := pcall(t, other, "POST", fmt.Sprintf("/platform/payouts/%d/mark-paid", id), `{"reference":""}`); st != 422 {
		t.Errorf("mark paid without a reference: %d, want 422", st)
	}
	if st, out := pcall(t, other, "POST", fmt.Sprintf("/platform/payouts/%d/mark-paid", id), `{"reference":"BANK1"}`); st != 200 {
		t.Errorf("second approver marking paid: %d %v", st, out)
	}

	var got models.Payment
	config.DB.First(&got, pay.ID)
	if got.PayoutID == nil {
		t.Error("a paid payout's payments must stay claimed")
	}
	if st, _ := pcall(t, app, "POST", fmt.Sprintf("/platform/payouts/%d/cancel", id), ""); st != 409 {
		t.Errorf("cancelling a completed payout: %d, want 409", st)
	}

	// The ISP sees its own history and balance — read only, no internal ids.
	_, out = pcall(t, app, "GET", "/admin/payouts", "")
	d := dataOf(out)
	hist := d["payouts"].([]interface{})
	if len(hist) != 1 || hist[0].(map[string]interface{})["status"] != "completed" || hist[0].(map[string]interface{})["reference"] != "BANK1" {
		t.Errorf("ISP payout history: %v", hist)
	}
	if _, leaked := hist[0].(map[string]interface{})["created_by_platform_user_id"]; leaked {
		t.Error("the ISP view must not expose platform staff ids")
	}
}

func TestPayoutCallbacks_RequireTheSharedSecret(t *testing.T) {
	setupPayoutTest(t)
	org, _ := seedSettledISP(t, "acme")
	svc := payoutSvcGlobal
	p, err := svc.CreatePayout(org.ID, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	config.DB.Model(p).Updates(map[string]interface{}{"status": "processing", "originator_conversation_id": "orig-x"})
	config.Config.MpesaCallbackSecret = "s3cret"
	app := payoutApp(1)
	body := `{"Result":{"ResultCode":0,"ResultDesc":"ok","OriginatorConversationID":"orig-x","ConversationID":"c","TransactionID":"RKT999"}}`

	if st, _ := pcall(t, app, "POST", "/payouts/b2b/result", body); st != 401 {
		t.Errorf("callback without the secret: %d, want 401", st)
	}
	if st, _ := pcall(t, app, "POST", "/payouts/b2b/result?token=wrong", body); st != 401 {
		t.Errorf("callback with a wrong secret: %d, want 401", st)
	}
	var got models.Payout
	config.DB.First(&got, p.ID)
	if got.Status != "processing" {
		t.Fatalf("a forged callback changed the payout to %q", got.Status)
	}
	if st, _ := pcall(t, app, "POST", "/payouts/b2b/result?token=s3cret", body); st != 200 {
		t.Errorf("genuine callback: %d", st)
	}
	config.DB.First(&got, p.ID)
	if got.Status != "completed" || got.Reference != "RKT999" {
		t.Errorf("after a genuine callback: %+v", got)
	}
}

func TestPayoutBackfill(t *testing.T) {
	setupPayoutTest(t)
	i := seedISP(t, "old", "", 1)
	own := seedISP(t, "own", "999888", 1)
	config.DB.Create(&models.OrganizationMpesaConfig{OrganizationID: own.OrgID, Mode: "own"})
	app := payoutApp(1)

	mock := "MOCKABC"
	real := "RCP1"
	old := time.Now().AddDate(0, -2, 0)
	mk := func(amount float64, method string, receipt *string, at time.Time) models.Payment {
		p := models.Payment{ZoneID: i.ZoneID, Phone: "2547", Amount: amount, Method: method, Status: "completed", MpesaReceiptNumber: receipt}
		config.DB.Create(&p)
		config.DB.Model(&p).UpdateColumn("created_at", at)
		return p
	}
	recent := time.Now().AddDate(0, 0, -3)
	eligible := mk(100, "mpesa", &real, recent)
	mk(500, "mpesa", &mock, recent) // fake sandbox money
	mk(700, "cash", nil, recent)    // recorded by hand, never on the shortcode
	mk(900, "mpesa", &real, old)    // before the cut-off date

	from := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	st, out := pcall(t, app, "POST", "/platform/payouts/backfill", fmt.Sprintf(`{"organization_id":%d,"from":%q}`, i.OrgID, from))
	d := dataOf(out)
	if st != 200 || d["payments"] != 1.0 || d["gross"] != 100.0 {
		t.Fatalf("backfill: %d %v — only the real, recent M-Pesa payment qualifies", st, out)
	}
	var e models.Payment
	config.DB.First(&e, eligible.ID)
	if e.CollectedVia != "platform" {
		t.Errorf("eligible payment collected_via = %q", e.CollectedVia)
	}
	if st, _ := pcall(t, app, "POST", "/platform/payouts/backfill", fmt.Sprintf(`{"organization_id":%d,"from":%q}`, own.OrgID, from)); st != 422 {
		t.Errorf("backfilling an ISP on its own Daraja: %d, want 422", st)
	}
	if st, _ := pcall(t, app, "POST", "/platform/payouts/backfill", fmt.Sprintf(`{"organization_id":%d,"from":"yesterday"}`, i.OrgID)); st != 422 {
		t.Errorf("bad date: %d, want 422", st)
	}
}

func TestPlatformDaraja_B2BPasswordIsWriteOnly(t *testing.T) {
	setupPayoutTest(t)
	app := payoutApp(1)
	v := "hunter2-initiator"
	config.DB.Create(&models.Setting{Key: "mpesa_b2b_password", Value: &v})
	invalidateAllSettings()

	_, out := pcall(t, app, "GET", "/platform/daraja", "")
	d := dataOf(out)
	if _, present := d["mpesa_b2b_password"]; present || d["mpesa_b2b_password_set"] != true {
		t.Errorf("daraja show leaked or mis-reported the B2B password: %v", d)
	}
	if strings.Contains(fmt.Sprint(out), "hunter2") {
		t.Error("the password appears in the response")
	}
}

func TestOrgCommissionPercent_IsValidated(t *testing.T) {
	setupPayoutTest(t)
	org, _ := seedSettledISP(t, "acme")
	app := payoutApp(1)
	patch := func(body string) int {
		st, _ := pcall(t, app, "PATCH", fmt.Sprintf("/platform/organizations/%d", org.ID), body)
		return st
	}
	if st := patch(`{"commission_percent":150}`); st != 422 {
		t.Errorf("150%%: %d, want 422", st)
	}
	if st := patch(`{"commission_percent":-1}`); st != 422 {
		t.Errorf("-1%%: %d, want 422", st)
	}
	if st := patch(`{"commission_percent":7.5}`); st != 200 {
		t.Errorf("7.5%%: %d, want 200", st)
	}
	if st := patch(`{"commission_percent":null}`); st != 200 {
		t.Errorf("null (use the platform default): %d, want 200", st)
	}
}

func TestPayoutCallbacks_RefuseEverythingWhenNoSecretIsConfigured(t *testing.T) {
	setupPayoutTest(t)
	org, _ := seedSettledISP(t, "acme")
	p, _ := payoutSvcGlobal.CreatePayout(org.ID, 1, 0, 0)
	config.DB.Model(p).Updates(map[string]interface{}{"status": "processing", "originator_conversation_id": "orig-y"})
	config.Config.MpesaCallbackSecret = "" // the production server today
	app := payoutApp(1)
	body := `{"Result":{"ResultCode":0,"ResultDesc":"ok","OriginatorConversationID":"orig-y","TransactionID":"FORGED"}}`

	if st, _ := pcall(t, app, "POST", "/payouts/b2b/result", body); st != 401 {
		t.Errorf("callback with no server secret: %d, want 401 (the STK callback's accept-anyone fallback must not apply to payouts)", st)
	}
	var got models.Payout
	config.DB.First(&got, p.ID)
	if got.Status != "processing" || got.Reference == "FORGED" {
		t.Errorf("a forged callback changed the payout: %+v", got)
	}
}
