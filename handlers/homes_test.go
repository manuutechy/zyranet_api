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
)

func setupHomesTest(t *testing.T) isp {
	t.Helper()
	setupC2BTestDB(t)
	if err := config.DB.AutoMigrate(&models.HomeRenewal{}, &models.Session{}); err != nil {
		t.Fatal(err)
	}
	return seedISP(t, "fibre", "", 1)
}

// seedHomeCustomer adds a KES 30/day home plan to the zone and a home on it.
func seedHomeCustomer(t *testing.T, zoneID uint, status string, credit float64) (models.Customer, models.Package) {
	t.Helper()
	pkg := models.Package{ZoneID: zoneID, Name: "Home Daily", Type: "pppoe", Price: 30, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "active"}
	if err := config.DB.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	user, pw := "h", "p"
	c := models.Customer{Name: "Jane Akinyi Otieno", Phone: "254711222333", ZoneID: zoneID, PackageID: pkg.ID, Type: "pppoe",
		PPPoEUsername: &user, PPPoEPassword: &pw, Status: status, CreditBalance: credit}
	if err := config.DB.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	if c.CustomerNo == nil {
		t.Fatal("no customer number assigned on create")
	}
	return c, pkg
}

func homeCall(t *testing.T, app *fiber.App, method, url, body string) (int, map[string]interface{}, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, url, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func publicHomeApp() *fiber.App {
	app := fiber.New()
	app.Get("/public/home/:no", HomePublicLookup)
	app.Post("/public/home/:no/pay", HomePublicPay)
	app.Get("/public/home/:no/pay/:id", HomePublicPayStatus)
	return app
}

func TestC2B_HomeByCustomerNumber_BuysManyDaysKeepsRemainder(t *testing.T) {
	a := setupHomesTest(t)
	c, pkg := seedHomeCustomer(t, a.ZoneID, "expired", 0)

	// Typed the way people do on the M-Pesa menu.
	postC2B(t, platformPaybill, "THOME1", fmt.Sprintf("zyr #%d", *c.CustomerNo), "254799000111", 100)

	var got models.Customer
	config.DB.First(&got, c.ID)
	if got.CreditBalance != 10 || got.Status != "active" {
		t.Fatalf("credit %v status %q, want 10 left and active", got.CreditBalance, got.Status)
	}
	if d := time.Until(*got.ExpiresAt); d < 71*time.Hour || d > 73*time.Hour {
		t.Errorf("paid for %v, want 3 days", d)
	}
	if got.PackageID != pkg.ID {
		t.Error("home plan changed")
	}
	// Same TransID again: credited once.
	postC2B(t, platformPaybill, "THOME1", fmt.Sprintf("%d", *c.CustomerNo), "254799000111", 100)
	config.DB.First(&got, c.ID)
	if got.CreditBalance != 10 {
		t.Errorf("duplicate delivery changed credit to %v", got.CreditBalance)
	}
}

func TestC2B_HomeKeepsOwnPlanEvenIfAHotspotPlanHasThatPrice(t *testing.T) {
	a := setupHomesTest(t)
	c, pkg := seedHomeCustomer(t, a.ZoneID, "expired", 0)
	// seedISP's hotspot "Daily" costs 100: a home paying 100 must not be moved onto it.
	postC2B(t, platformPaybill, "THOME2", fmt.Sprintf("%d", *c.CustomerNo), "254799000111", 100)
	var got models.Customer
	config.DB.First(&got, c.ID)
	if got.PackageID != pkg.ID || got.Type != "pppoe" {
		t.Errorf("home moved to package %d", got.PackageID)
	}
}

func TestC2B_WrongCheckDigitIsNotSomeoneElses(t *testing.T) {
	a := setupHomesTest(t)
	c, _ := seedHomeCustomer(t, a.ZoneID, "expired", 0)
	wrong := *c.CustomerNo + 1
	postC2B(t, platformPaybill, "THOME3", fmt.Sprintf("%d", wrong), "hashedmsisdn", 30)
	var got models.Customer
	config.DB.First(&got, c.ID)
	if got.CreditBalance != 0 {
		t.Error("a mistyped number was credited to a customer")
	}
	if count(t, &models.UnmatchedC2BPayment{}) != 1 {
		t.Error("mistyped number should be queued as unmatched")
	}
}

func TestHomePublicLookup_MasksAndHidesPersonalData(t *testing.T) {
	a := setupHomesTest(t)
	c, _ := seedHomeCustomer(t, a.ZoneID, "active", 55)
	app := publicHomeApp()

	st, out, raw := homeCall(t, app, "GET", fmt.Sprintf("/public/home/%d", *c.CustomerNo), "")
	if st != 200 {
		t.Fatalf("lookup %d %s", st, raw)
	}
	d := out["data"].(map[string]interface{})
	if d["name"] != "Ja*** A*** O***" {
		t.Errorf("name = %v", d["name"])
	}
	for _, leak := range []string{"254711222333", "Akinyi", "credit", "55", `"p"`} {
		if strings.Contains(raw, leak) {
			t.Errorf("public lookup leaks %q: %s", leak, raw)
		}
	}
	if d["status"] != "off" {
		t.Errorf("status %v, want off (no paid time)", d["status"])
	}
	opts := d["options"].([]interface{})
	if len(opts) != 3 || opts[1].(map[string]interface{})["amount"].(float64) != 210 {
		t.Errorf("options = %v, want 1/7/30 days with 7 days = 210", opts)
	}
	pay := d["pay"].(map[string]interface{})
	if pay["account"] != fmt.Sprint(*c.CustomerNo) {
		t.Errorf("pay account = %v", pay["account"])
	}

	if st, _, _ := homeCall(t, app, "GET", "/public/home/99999", ""); st != 404 {
		t.Errorf("unknown number: %d, want 404", st)
	}
	// A hotspot customer's number is not a home.
	h := seedCustomer(t, a.ZoneID, "Hot", "254700000999", "ZYR#H")
	if st, _, _ := homeCall(t, app, "GET", fmt.Sprintf("/public/home/%d", *h.CustomerNo), ""); st != 404 {
		t.Errorf("hotspot customer via home lookup: %d, want 404", st)
	}
}

func TestHomePublicPay_Validation(t *testing.T) {
	a := setupHomesTest(t)
	initMoneyServices(t, "")
	c, _ := seedHomeCustomer(t, a.ZoneID, "active", 0)
	s, _ := seedHomeCustomer(t, a.ZoneID, "suspended", 0)
	app := publicHomeApp()
	url := fmt.Sprintf("/public/home/%d/pay", *c.CustomerNo)

	if st, _, _ := homeCall(t, app, "POST", url, `{"phone":"12345","periods":1}`); st != 422 {
		t.Errorf("bad phone: %d", st)
	}
	if st, _, _ := homeCall(t, app, "POST", url, `{"phone":"0712345678","periods":0}`); st != 422 {
		t.Errorf("0 periods: %d", st)
	}
	if st, _, _ := homeCall(t, app, "POST", url, `{"phone":"0712345678","periods":367}`); st != 422 {
		t.Errorf("367 periods: %d", st)
	}
	if st, _, _ := homeCall(t, app, "POST", fmt.Sprintf("/public/home/%d/pay", *s.CustomerNo), `{"phone":"0712345678","periods":1}`); st != 409 {
		t.Errorf("suspended home: %d, want 409", st)
	}
}

func TestHomePublicPayStatus_OnlyThisHomesRenewals(t *testing.T) {
	a := setupHomesTest(t)
	c1, pkg := seedHomeCustomer(t, a.ZoneID, "active", 0)
	c2, _ := seedHomeCustomer(t, a.ZoneID, "active", 0)
	pid := pkg.ID
	p := models.Payment{CustomerID: &c1.ID, ZoneID: a.ZoneID, PackageID: &pid, Phone: "254712345678", Amount: 30, Method: "mpesa", Status: "failed"}
	config.DB.Create(&p)
	config.DB.Create(&models.HomeRenewal{PaymentID: p.ID, CustomerID: c1.ID, Periods: 1})
	app := publicHomeApp()

	if st, out, _ := homeCall(t, app, "GET", fmt.Sprintf("/public/home/%d/pay/%d", *c1.CustomerNo, p.ID), ""); st != 200 || out["data"].(map[string]interface{})["status"] != "failed" {
		t.Errorf("own payment: %d %v", st, out)
	}
	if st, _, _ := homeCall(t, app, "GET", fmt.Sprintf("/public/home/%d/pay/%d", *c2.CustomerNo, p.ID), ""); st != 404 {
		t.Errorf("another home's payment: %d, want 404", st)
	}
}

func adminHomeApp(orgID uint, role string, zoneID *uint) *fiber.App {
	app := fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals("claims", &middleware.Claims{Role: role, OrganizationID: orgID, ZoneID: zoneID, Type: "admin"})
		return c.Next()
	}
	app.Post("/homes", auth, HomeStore)
	app.Get("/homes/:id", auth, HomeShow)
	app.Post("/homes/:id/switch-off", auth, HomeSwitchOff)
	app.Post("/homes/:id/switch-on", auth, HomeSwitchOn)
	return app
}

func TestHomeStore_CreatesPPPoEWithCustomerNumberAsUsername(t *testing.T) {
	a := setupHomesTest(t)
	_, pkg := seedHomeCustomer(t, a.ZoneID, "active", 0)
	app := adminHomeApp(a.OrgID, "super_admin", nil)

	body := fmt.Sprintf(`{"name":"Peter Ouma","phone":"0722 000 111","zone_id":%d,"package_id":%d,"location":"Behind Maseno Market","ont_serial":"HWTC1234ABCD"}`, a.ZoneID, pkg.ID)
	st, out, raw := homeCall(t, app, "POST", "/homes", body)
	if st != 201 {
		t.Fatalf("create: %d %s", st, raw)
	}
	d := out["data"].(map[string]interface{})
	var c models.Customer
	config.DB.First(&c, uint(d["customer"].(map[string]interface{})["id"].(float64)))
	if c.Type != "pppoe" || c.CustomerNo == nil || c.PPPoEUsername == nil || *c.PPPoEUsername != fmt.Sprint(*c.CustomerNo) {
		t.Fatalf("home = %+v", c)
	}
	if c.PPPoEPassword == nil || len(*c.PPPoEPassword) < 8 || c.Phone != "254722000111" || c.Status != "expired" {
		t.Errorf("password/phone/status wrong: %+v", c)
	}
	if c.Location == nil || *c.Location != "Behind Maseno Market" || c.OntSerial == nil || *c.OntSerial != "HWTC1234ABCD" {
		t.Error("location / ONT serial not saved")
	}
	if d["state"] != "off" {
		t.Errorf("new home state %v, want off until paid", d["state"])
	}

	// A hotspot plan is not a home plan.
	var hot models.Package
	config.DB.Where("zone_id = ? AND type = ?", a.ZoneID, "hotspot").First(&hot)
	if st, _, _ := homeCall(t, app, "POST", "/homes", fmt.Sprintf(`{"name":"X","phone":"0722000112","zone_id":%d,"package_id":%d}`, a.ZoneID, hot.ID)); st != 422 {
		t.Errorf("hotspot plan: %d, want 422", st)
	}
	// Another ISP's zone.
	b := seedISP(t, "other", "", 1)
	if st, _, _ := homeCall(t, app, "POST", "/homes", fmt.Sprintf(`{"name":"X","phone":"0722000113","zone_id":%d,"package_id":%d}`, b.ZoneID, pkg.ID)); st != 422 {
		t.Errorf("other ISP's zone: %d, want 422", st)
	}
	// A zone manager of another zone.
	other := b.ZoneID
	if st, _, _ := homeCall(t, adminHomeApp(a.OrgID, "zone_manager", &other), "POST", "/homes", body); st != 403 {
		t.Errorf("zone manager of another zone: %d, want 403", st)
	}
	// Field roles can't add homes.
	if st, _, _ := homeCall(t, adminHomeApp(a.OrgID, "support", nil), "POST", "/homes", body); st != 403 {
		t.Errorf("support role: %d, want 403", st)
	}
}

func TestHomeSwitchOffOn(t *testing.T) {
	a := setupHomesTest(t)
	initMoneyServices(t, "")
	c, _ := seedHomeCustomer(t, a.ZoneID, "expired", 20)
	app := adminHomeApp(a.OrgID, "super_admin", nil)

	// Not paid, credit 20 < 30: stays off.
	if st, _, _ := homeCall(t, app, "POST", fmt.Sprintf("/homes/%d/switch-on", c.ID), ""); st != 422 {
		t.Errorf("switch on without money: %d, want 422", st)
	}
	config.DB.Model(&models.Customer{}).Where("id = ?", c.ID).Update("credit_balance", 50)
	st, out, raw := homeCall(t, app, "POST", fmt.Sprintf("/homes/%d/switch-on", c.ID), "")
	if st != 200 || out["data"].(map[string]interface{})["state"] != "on" {
		t.Fatalf("switch on with credit: %d %s", st, raw)
	}
	var got models.Customer
	config.DB.First(&got, c.ID)
	if got.CreditBalance != 20 {
		t.Errorf("credit %v, want 20 (one day bought)", got.CreditBalance)
	}

	st, out, _ = homeCall(t, app, "POST", fmt.Sprintf("/homes/%d/switch-off", c.ID), "")
	if st != 200 || out["data"].(map[string]interface{})["state"] != "suspended" {
		t.Fatalf("switch off: %d %v", st, out)
	}
	// Switching back on with paid time left costs nothing.
	homeCall(t, app, "POST", fmt.Sprintf("/homes/%d/switch-on", c.ID), "")
	config.DB.First(&got, c.ID)
	if got.Status != "active" || got.CreditBalance != 20 {
		t.Errorf("after switch on: %q credit %v", got.Status, got.CreditBalance)
	}

	// Another ISP can't touch it.
	b := seedISP(t, "other", "", 1)
	if st, _, _ := homeCall(t, adminHomeApp(b.OrgID, "super_admin", nil), "POST", fmt.Sprintf("/homes/%d/switch-off", c.ID), ""); st != 404 {
		t.Errorf("other ISP switch off: %d, want 404", st)
	}
}
