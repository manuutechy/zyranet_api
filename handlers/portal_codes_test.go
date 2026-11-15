package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
)

// (a) /hotspot/pay: one prompt per phone (or device on the zone) per 90 s.
func TestHotspotPay_SecondTapReusesThePendingPrompt(t *testing.T) {
	setupC2BTestDB(t)
	f := newFakeDarajaAPI(t)
	productionPlatform(t)
	initMoneyServices(t, f.URL)
	putSetting(t, "sms_enable_voucher", "no")
	old := voucherSvcGlobal
	voucherSvcGlobal = services.NewVoucherService(services.NewSmsService())
	t.Cleanup(func() { voucherSvcGlobal = old })
	a := seedISP(t, "a", "", 1)
	var pkg models.Package
	config.DB.Where("zone_id = ?", a.ZoneID).First(&pkg)
	config.DB.Model(&models.Zone{}).Where("id = ?", a.ZoneID).Update("last_seen_at", time.Now())

	body := fmt.Sprintf(`{"phone":"0712345678","plan_id":"%d","zone_id":"%d","mac":"AA:BB:CC:DD:EE:02"}`, pkg.ID, a.ZoneID)
	c1, o1 := postJSON(t, HotspotPay, "/pay", body)
	c2, o2 := postJSON(t, HotspotPay, "/pay", strings.Replace(body, "0712345678", "254712345678", 1))
	if c1 != 200 || c2 != 409 {
		t.Fatalf("status %d %v / %d %v, want 200 then 409", c1, o1, c2, o2)
	}
	if f.stkPushes != 1 || o1["reference"] == nil || o2["code"] != "prompt_pending" || o2["reference"] != nil ||
		strings.Contains(fmt.Sprint(o2), fmt.Sprint(o1["reference"])) {
		t.Errorf("%d prompts; second tap %v must be prompt_pending without the reference %v", f.stkPushes, o2, o1["reference"])
	}
	postJSON(t, HotspotPay, "/pay", strings.Replace(body, "0712345678", "0799000111", 1))
	if f.stkPushes != 1 {
		t.Errorf("same device, other phone: %d prompts", f.stkPushes)
	}
	if n := count(t, &models.Payment{}); n != 1 {
		t.Errorf("%d payments, want 1", n)
	}
}

// (b) Failed payments carry a machine code next to the message.
func TestHotspotStatus_FailureCodes(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "", 1)
	app := fiber.New()
	app.Get("/s/:reference", HotspotStatus)
	cases := map[string]string{
		"Payment cancelled. You closed the M-Pesa prompt before approving it.":                  "cancelled",
		"Payment failed: wrong M-Pesa PIN entered. Please try again with the correct PIN.":      "pin",
		"Payment failed: not enough money in your M-Pesa account. Top up M-Pesa and try again.": "funds",
		services.STKNoResponseReason:                          "no_response",
		services.STKUnconfirmedReason:                         "unconfirmed",
		"We couldn't process this payment. Please try again.": "other",
	}
	i := 0
	for reason, want := range cases {
		i++
		r := reason
		ref := fmt.Sprintf("ws_CO_test_%04d", i)
		config.DB.Create(&models.Payment{ZoneID: a.ZoneID, Phone: "254700000009", Amount: 10, Method: "mpesa", Status: "failed", StatusReason: &r, MpesaTransactionID: &ref})
		st, out := agentCall(t, app, "GET", "/s/"+ref, "", "")
		if st != 200 || out["status"] != "failed" || out["code"] != want || out["message"] != reason {
			t.Errorf("%q: %d %v, want code %s and the message kept", reason, st, out, want)
		}
	}
}

// (c) Voucher refusals carry a code: invalid | used | expired.
func TestVoucherRedeem_RefusalCodes(t *testing.T) {
	_, a, b := setupAgentTest(t)
	pkg := pkgOf(t, a.ZoneID)
	mk := func(status string, exp *time.Time) string {
		v, err := voucherSvcGlobal.Generate(a.ZoneID, pkg.ID, "single_use", 1)
		if err != nil {
			t.Fatal(err)
		}
		config.DB.Model(&models.Voucher{}).Where("id = ?", v.ID).Updates(map[string]interface{}{"status": status, "expires_at": exp})
		return v.Code
	}
	past := time.Now().Add(-time.Hour)
	used, expired, timedOut, fine := mk("depleted", nil), mk("expired", nil), mk("unused", &past), mk("unused", nil)
	for _, tc := range []struct{ body, want string }{
		{`{"code":"NOPE-0000"}`, "invalid"},
		{`{}`, "invalid"},
		{fmt.Sprintf(`{"code":%q}`, used), "used"},
		{fmt.Sprintf(`{"code":%q}`, expired), "expired"},
		{fmt.Sprintf(`{"code":%q}`, timedOut), "expired"},
		{fmt.Sprintf(`{"code":%q,"zone_id":"%d"}`, fine, b.ZoneID), "invalid"}, // another ISP's WiFi
	} {
		st, out := postJSON(t, VoucherRedeem, "/r", tc.body)
		if st < 400 || out["code"] != tc.want || out["error"] == nil {
			t.Errorf("%s: %d %v, want code %s", tc.body, st, out, tc.want)
		}
	}
	if got := services.VoucherFailureCode(fmt.Errorf("voucher was already redeemed")); got != "used" {
		t.Errorf("already redeemed → %s", got)
	}
}

// (d) Magic test phones only ever work with MPESA_ENV=mock off production.
func TestMockSTKFailure_OnlyInMock(t *testing.T) {
	oldM, oldA := config.Config.MpesaEnv, config.Config.AppEnv
	t.Cleanup(func() { config.Config.MpesaEnv, config.Config.AppEnv = oldM, oldA })
	config.Config.MpesaEnv, config.Config.AppEnv = "mock", "local"
	if code, _, ok := services.MockSTKFailure("0700000001"); !ok || code != 1032 {
		t.Errorf("mock cancelled: %v %v", code, ok)
	}
	if _, _, ok := services.MockSTKFailure("0712345678"); ok {
		t.Error("a normal phone must succeed in mock")
	}
	for _, env := range [][2]string{{"production", "local"}, {"sandbox", "local"}, {"mock", "production"}, {"", ""}} {
		config.Config.MpesaEnv, config.Config.AppEnv = env[0], env[1]
		if _, _, ok := services.MockSTKFailure("254700000001"); ok {
			t.Errorf("magic phone worked with MPESA_ENV=%q APP_ENV=%q", env[0], env[1])
		}
	}
}

// A poll that says it comes from another device or WiFi than the one that
// paid gets "paid" but no session token and no router login.
func TestHotspotStatus_CredentialsOnlyForThePayingDevice(t *testing.T) {
	setupC2BTestDB(t)
	initMoneyServices(t, "")
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "Payer", "254700000077", "ZYR#77")
	pkgID := pkgOfZone(t, a.ZoneID)
	exp := time.Now().Add(24 * time.Hour)
	config.DB.Model(&models.Customer{}).Where("id = ?", c.ID).Updates(map[string]interface{}{"status": "active", "expires_at": exp})
	ref, rc := "ws_CO_bind_0001", "RBIND00001"
	config.DB.Create(&models.Payment{ZoneID: a.ZoneID, CustomerID: &c.ID, PackageID: &pkgID, Phone: c.Phone, Amount: 100, Method: "mpesa",
		Status: "completed", MpesaTransactionID: &ref, MpesaReceiptNumber: &rc, MacAddress: "AA:BB:CC:DD:EE:77"})
	app := fiber.New()
	app.Get("/s/:reference", HotspotStatus)
	for _, q := range []string{"?mac=AA:BB:CC:DD:EE:99", fmt.Sprintf("?zone_id=%d", a.ZoneID+100), fmt.Sprintf("?mac=aa-bb-cc-dd-ee-99&zone_id=%d", a.ZoneID)} {
		st, out := agentCall(t, app, "GET", "/s/"+ref+q, "", "")
		if st != 200 || out["status"] != "paid" || out["token"] != nil || out["router_login"] != nil {
			t.Errorf("%s: %d %v — a stranger's poll must get no session", q, st, out)
		}
	}
	st, out := agentCall(t, app, "GET", fmt.Sprintf("/s/%s?mac=aa-bb-cc-dd-ee-77&zone_id=%d", ref, a.ZoneID), "", "")
	if st != 200 || out["token"] == nil {
		t.Errorf("the paying device: %d %v, want a session token", st, out)
	}
}

func pkgOfZone(t *testing.T, zoneID uint) uint {
	var p models.Package
	config.DB.Where("zone_id = ?", zoneID).First(&p)
	return p.ID
}
