package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func setupRouterAccessHandlers(t *testing.T) {
	t.Helper()
	setupC2BTestDB(t)
	if err := config.DB.AutoMigrate(&models.Voucher{}, &models.ZoneAlert{}, &models.ZoneStat{}); err != nil {
		t.Fatal(err)
	}
	if err := services.MigrateRouterAccess(config.DB); err != nil {
		t.Fatal(err)
	}
	config.Config.JWTSecret = "router-access-test-secret-router-access"
	config.Config.JWTExpiry = time.Hour
}

func routerAccessApp() *fiber.App {
	app := fiber.New()
	app.Post("/hotspot/credentials", HotspotCredentials)
	app.Post("/hotspot/logout", HotspotLogout)
	app.Get("/hotspot/status/:reference", HotspotStatus)
	app.Post("/customer/reconnect", CustomerReconnect)
	app.Post("/customer/auth/recover", CustomerRecoverByReceipt)
	app.Post("/customer/free-tier/claim", CustomerClaimFreeTier)
	app.Get("/public/zones/sync/:id", PublicZoneSync)
	return app
}

func doJSON(t *testing.T, app *fiber.App, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, decode(t, resp)
}

func decode(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	out["_raw"] = string(raw)
	return out
}

func paidCustomer(t *testing.T, zoneID uint, phone, mac string) models.Customer {
	t.Helper()
	c := seedCustomer(t, zoneID, "Payer", phone, "ZYR#"+phone)
	exp := time.Now().Add(6 * time.Hour)
	config.DB.Model(&c).Updates(map[string]interface{}{"expires_at": exp, "status": "active", "mac_address": mac})
	config.DB.First(&c, c.ID)
	return c
}

// The router login (username/password) only comes out against proof, once
// per ticket, and is never a shared pkg-<id> / free user.
func TestHotspotCredentials_TicketOnceThenDeviceToken(t *testing.T) {
	setupRouterAccessHandlers(t)
	a := seedISP(t, "isp", "", 1)
	c := paidCustomer(t, a.ZoneID, "254711000001", "aa:bb:cc:dd:ee:01")
	cred, err := services.EnsureCredential(c.ID, "aa:bb:cc:dd:ee:01", "paid")
	if err != nil {
		t.Fatal(err)
	}
	rl, _ := services.IssueRouterLogin(cred, true)
	app := routerAccessApp()
	zid := fmt.Sprint(a.ZoneID)

	st, _ := doJSON(t, app, "POST", "/hotspot/credentials", map[string]string{"mac": "AA:BB:CC:DD:EE:01", "zone_id": zid})
	if st != 401 {
		t.Errorf("no proof: %d, want 401", st)
	}
	st, out := doJSON(t, app, "POST", "/hotspot/credentials", map[string]string{"ticket": rl.LoginTicket, "mac": "AA:BB:CC:DD:EE:01", "zone_id": zid})
	if st != 200 {
		t.Fatalf("ticket: %d %v", st, out["_raw"])
	}
	data := out["data"].(map[string]interface{})
	if data["username"] != cred.Username || data["password"] != cred.Password {
		t.Errorf("got %v/%v, want the device's own login", data["username"], data["password"])
	}
	if strings.HasPrefix(fmt.Sprint(data["username"]), "pkg-") || data["username"] == "free" {
		t.Error("shared login handed out")
	}
	if st, _ := doJSON(t, app, "POST", "/hotspot/credentials", map[string]string{"ticket": rl.LoginTicket, "mac": "AA:BB:CC:DD:EE:01"}); st != 401 {
		t.Errorf("ticket reuse: %d, want 401", st)
	}
	if st, _ := doJSON(t, app, "POST", "/hotspot/credentials", map[string]string{"device_token": rl.DeviceToken, "mac": "AA:BB:CC:DD:EE:01", "zone_id": zid}); st != 200 {
		t.Errorf("device token: %d, want 200", st)
	}
	// A ticket used from another device is refused.
	rl2, _ := services.IssueRouterLogin(cred, false)
	if st, _ := doJSON(t, app, "POST", "/hotspot/credentials", map[string]string{"ticket": rl2.LoginTicket, "mac": "AA:BB:CC:DD:EE:99"}); st != 401 {
		t.Errorf("ticket from another MAC: %d, want 401", st)
	}
}

// Moving a plan to a new MAC needs proof; the MAC alone gets nothing.
func TestMACMove_RequiresProof(t *testing.T) {
	setupRouterAccessHandlers(t)
	a := seedISP(t, "isp", "", 1)
	c := paidCustomer(t, a.ZoneID, "254711000001", "aa:bb:cc:dd:ee:01")
	oldCred, _ := services.EnsureCredential(c.ID, "aa:bb:cc:dd:ee:01", "paid")
	rl, _ := services.IssueRouterLogin(oldCred, true)
	app := routerAccessApp()
	zid := fmt.Sprint(a.ZoneID)

	// An attacker who knows (or spoofs) the paying MAC.
	if st, _ := doJSON(t, app, "POST", "/customer/reconnect", map[string]string{"mac": "aa:bb:cc:dd:ee:01", "zone_id": zid}); st != 401 {
		t.Errorf("reconnect by MAC alone: %d, want 401", st)
	}
	if st, _ := doJSON(t, app, "POST", "/customer/reconnect", map[string]string{"mac": "aa:bb:cc:dd:ee:77", "zone_id": zid}); st != 401 {
		t.Errorf("reconnect a new MAC with no proof: %d, want 401", st)
	}
	var n int64
	config.DB.Model(&models.HotspotCredential{}).Where("mac_address = ?", "AA:BB:CC:DD:EE:77").Count(&n)
	if n != 0 {
		t.Fatal("no login may be created for an unproven MAC")
	}

	// The real phone, now with a randomised MAC, proves itself with its token.
	st, out := doJSON(t, app, "POST", "/customer/reconnect", map[string]string{"mac": "aa:bb:cc:dd:ee:02", "zone_id": zid, "device_token": rl.DeviceToken})
	if st != 200 {
		t.Fatalf("reconnect with device token: %d %v", st, out["_raw"])
	}
	var old models.HotspotCredential
	config.DB.First(&old, oldCred.ID)
	if old.Status != "revoked" {
		t.Errorf("old MAC's login should be retired, got %s", old.Status)
	}
	var moved models.HotspotCredential
	if err := config.DB.Where("mac_address = ? AND status = ?", "AA:BB:CC:DD:EE:02", "active").First(&moved).Error; err != nil || moved.CustomerID != c.ID {
		t.Error("the plan should now be on the new MAC")
	}
	if !moved.ExpiresAt.Equal(*c.ExpiresAt) {
		t.Error("remaining time must move unchanged")
	}
}

func TestRecoverByReceipt_NeedsMatchingPhoneAndISP(t *testing.T) {
	setupRouterAccessHandlers(t)
	a := seedISP(t, "isp", "", 1)
	b := seedISP(t, "other", "", 1)
	c := paidCustomer(t, a.ZoneID, "254711000001", "aa:bb:cc:dd:ee:01")
	receipt := "TJK1ABCDEF"
	pkgID := c.PackageID
	config.DB.Create(&models.Payment{CustomerID: &c.ID, ZoneID: a.ZoneID, PackageID: &pkgID, Phone: "0711000001", Amount: 100,
		Method: "mpesa", Status: "completed", MpesaReceiptNumber: &receipt, MacAddress: "aa:bb:cc:dd:ee:01"})
	app := routerAccessApp()

	try := func(phone, code, zone string) (int, map[string]interface{}) {
		return doJSON(t, app, "POST", "/customer/auth/recover", map[string]string{"phone": phone, "code": code, "mac": "aa:bb:cc:dd:ee:05", "zone_id": zone})
	}
	if st, out := try("0722999999", receipt, fmt.Sprint(a.ZoneID)); st != 404 || strings.Contains(fmt.Sprint(out["_raw"]), "Payer") {
		t.Errorf("someone else's code with the wrong phone: %d %v (must reveal nothing)", st, out["_raw"])
	}
	if st, _ := try("0711000001", receipt, fmt.Sprint(b.ZoneID)); st != 404 {
		t.Errorf("code used on another ISP's WiFi: %d, want 404", st)
	}
	st, out := try("+254 711 000001", strings.ToLower(receipt), fmt.Sprint(a.ZoneID))
	if st != 200 {
		t.Fatalf("matching code + phone: %d %v", st, out["_raw"])
	}
	data := out["data"].(map[string]interface{})
	if rl, ok := data["router_login"].(map[string]interface{}); !ok || rl["login_ticket"] == "" || rl["device_token"] == "" {
		t.Errorf("recovery should hand out a router login: %v", data["router_login"])
	}
	var moved int64
	config.DB.Model(&models.HotspotCredential{}).Where("customer_id = ? AND mac_address = ? AND status = ?", c.ID, "AA:BB:CC:DD:EE:05", "active").Count(&moved)
	if moved != 1 {
		t.Error("plan should be on the recovering device")
	}
}

func TestFreeTrial_OnePerDeviceAndPhoneNeverOverPaidPlan(t *testing.T) {
	setupRouterAccessHandlers(t)
	a := seedISP(t, "isp", "", 1)
	limit := 10
	free := models.Package{ZoneID: a.ZoneID, Name: "FREE TIER", Type: "hotspot", Price: 0, IsFreeTier: true, TimeLimitMinutes: &limit,
		FreeTierCooldownHours: 24, FreeTierDays: "all", SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "hourly", Status: "active"}
	config.DB.Create(&free)
	app := routerAccessApp()
	claim := func(query, mac, phone string) (int, map[string]interface{}) {
		return doJSON(t, app, "POST", "/customer/free-tier/claim"+query, map[string]interface{}{"zone_id": a.ZoneID, "mac": mac, "phone": phone})
	}

	st, out := claim("", "aa:bb:cc:dd:ee:10", "0711222333")
	if st != 200 {
		t.Fatalf("first trial: %d %v", st, out["_raw"])
	}
	if strings.Contains(fmt.Sprint(out["_raw"]), `"username":"free"`) || strings.Contains(fmt.Sprint(out["_raw"]), `"password":"free"`) {
		t.Error("trial must not hand out the shared free login")
	}
	// Same device, a new phone, a forced/reset request: no second trial.
	if st, _ := claim("?force=true", "aa:bb:cc:dd:ee:11", "0711222333"); st != 429 {
		t.Errorf("same phone, new MAC, force=true: %d, want 429", st)
	}
	if st, _ := claim("?reset=true", "aa:bb:cc:dd:ee:12", "0711222333"); st != 429 {
		t.Errorf("same phone, reset=true: %d, want 429", st)
	}
	var claims int64
	config.DB.Model(&models.FreeTrialClaim{}).Count(&claims)
	if claims != 1 {
		t.Errorf("trials granted = %d, want 1", claims)
	}

	// A paying customer's plan is never overwritten by a trial.
	payer := paidCustomer(t, a.ZoneID, "254711000009", "aa:bb:cc:dd:ee:20")
	if _, err := services.EnsureCredential(payer.ID, "aa:bb:cc:dd:ee:20", "paid"); err != nil {
		t.Fatal(err)
	}
	if st, _ := claim("?force=true", "aa:bb:cc:dd:ee:20", ""); st != 409 {
		t.Errorf("trial on a paid device: %d, want 409", st)
	}
	var after models.Customer
	config.DB.First(&after, payer.ID)
	if after.PackageID != payer.PackageID || !after.ExpiresAt.Equal(*payer.ExpiresAt) {
		t.Error("the paid plan was changed by a trial claim")
	}
}

func TestHotspotLogout_RequiresTheDevicesOwnToken(t *testing.T) {
	setupRouterAccessHandlers(t)
	a := seedISP(t, "isp", "", 1)
	c := paidCustomer(t, a.ZoneID, "254711000001", "aa:bb:cc:dd:ee:01")
	cred, _ := services.EnsureCredential(c.ID, "aa:bb:cc:dd:ee:01", "")
	rl, _ := services.IssueRouterLogin(cred, true)
	other := paidCustomer(t, a.ZoneID, "254711000002", "aa:bb:cc:dd:ee:02")
	otherCred, _ := services.EnsureCredential(other.ID, "aa:bb:cc:dd:ee:02", "")
	otherRL, _ := services.IssueRouterLogin(otherCred, true)
	app := routerAccessApp()
	zid := fmt.Sprint(a.ZoneID)

	if st, _ := doJSON(t, app, "POST", "/hotspot/logout", map[string]string{"mac": "aa:bb:cc:dd:ee:01", "zone_id": zid}); st != 401 {
		t.Errorf("kick by MAC alone: %d, want 401", st)
	}
	if st, _ := doJSON(t, app, "POST", "/hotspot/logout", map[string]string{"mac": "aa:bb:cc:dd:ee:01", "zone_id": zid, "device_token": otherRL.DeviceToken}); st != 401 {
		t.Errorf("kick someone else with my token: %d, want 401", st)
	}
	st, out := doJSON(t, app, "POST", "/hotspot/logout", map[string]string{"mac": "aa:bb:cc:dd:ee:01", "zone_id": zid, "device_token": rl.DeviceToken})
	if st == 401 {
		t.Errorf("own token should be allowed: %d %v", st, out["_raw"])
	}
}

func TestPublicZoneSync_RejectsMissingTokenAndServesPerDeviceLogins(t *testing.T) {
	setupRouterAccessHandlers(t)
	scriptSvc = services.NewMikroTikScriptService()
	old := config.Config.AllowLegacyRouterRequests
	config.Config.AllowLegacyRouterRequests = true // the removed escape hatch must not matter
	t.Cleanup(func() { config.Config.AllowLegacyRouterRequests = old })
	a := seedISP(t, "isp", "", 1)
	var zone models.Zone
	config.DB.First(&zone, a.ZoneID)
	c := paidCustomer(t, a.ZoneID, "254711000001", "aa:bb:cc:dd:ee:01")
	cred, _ := services.EnsureCredential(c.ID, "aa:bb:cc:dd:ee:01", "")
	app := routerAccessApp()

	get := func(path string) string {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for _, path := range []string{
		fmt.Sprintf("/public/zones/sync/%d", zone.ID),
		fmt.Sprintf("/public/zones/sync/%d?token=", zone.ID),
		fmt.Sprintf("/public/zones/sync/%d?token=nope", zone.ID),
	} {
		if body := get(path); strings.Contains(body, cred.Password) || strings.Contains(body, "/ip hotspot") {
			t.Errorf("%s served a script without the zone's token", path)
		}
	}
	body := get(fmt.Sprintf("/public/zones/sync/%d?token=%s", zone.ID, zone.ProvisionToken))
	if !strings.Contains(body, cred.Username) || strings.Contains(body, `add name="pkg-`) {
		t.Errorf("sync with token should carry per-device logins only:\n%s", body)
	}
	config.DB.First(&zone, zone.ID)
	if zone.LastSyncAt == nil {
		t.Error("serving a sync should note when the router last got its logins")
	}
}

func TestHotspotStatus_PaidGivesTicketNotSharedLogin(t *testing.T) {
	setupRouterAccessHandlers(t)
	a := seedISP(t, "isp", "", 1)
	c := paidCustomer(t, a.ZoneID, "254711000001", "aa:bb:cc:dd:ee:01")
	ref := "ws_CO_123456789"
	pkgID := c.PackageID
	config.DB.Create(&models.Payment{CustomerID: &c.ID, ZoneID: a.ZoneID, PackageID: &pkgID, Phone: "0711000001", Amount: 100,
		Method: "mpesa", Status: "completed", MpesaTransactionID: &ref, MacAddress: "aa:bb:cc:dd:ee:01"})
	app := routerAccessApp()
	st, out := doJSON(t, app, "GET", "/hotspot/status/"+ref, nil)
	if st != 200 || out["status"] != "paid" {
		t.Fatalf("status: %d %v", st, out["_raw"])
	}
	if _, ok := out["username"]; ok {
		t.Error("status must not return router credentials directly")
	}
	rl, ok := out["router_login"].(map[string]interface{})
	if !ok || rl["login_ticket"] == "" {
		t.Fatalf("paid status should carry a one-time login ticket: %v", out["_raw"])
	}
}

// Security review: a session recognised from a MAC (guest / device session)
// is never proof — it can't reconnect or get a router login, and guest
// login never hands out an existing account by MAC.
func TestWeakOrGuestSession_CannotGetRouterLogin(t *testing.T) {
	setupRouterAccessHandlers(t)
	a := seedISP(t, "isp", "", 1)
	victimMAC := "aa:bb:cc:dd:ee:01"
	victim := paidCustomer(t, a.ZoneID, "254711000001", victimMAC)
	config.DB.Model(&victim).Update("account_number", "ZYR#GUEST#10001") // a paid guest account
	if _, err := services.EnsureCredential(victim.ID, victimMAC, "paid"); err != nil {
		t.Fatal(err)
	}
	app := routerAccessApp()
	app.Post("/customer/auth/guest", CustomerAuthGuest)
	zid := fmt.Sprint(a.ZoneID)

	// Guest login with the victim's MAC must not land on the victim's account.
	st, out := doJSON(t, app, "POST", "/customer/auth/guest", map[string]string{"mac": victimMAC})
	if st != 200 {
		t.Fatalf("guest: %d %v", st, out["_raw"])
	}
	data, _ := out["data"].(map[string]interface{})
	cust, _ := data["customer"].(map[string]interface{})
	if cust == nil || uint(cust["id"].(float64)) == victim.ID {
		t.Fatal("guest login handed out an existing account recognised by MAC")
	}

	// A weak (device) session for the victim, from the victim's MAC or a new one.
	weak, _ := middleware.GenerateDeviceCustomerToken(victim.ID)
	for _, mac := range []string{victimMAC, "aa:bb:cc:dd:ee:66"} {
		b, _ := json.Marshal(map[string]string{"mac": mac, "zone_id": zid})
		req := httptest.NewRequest("POST", "/customer/reconnect", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+weak)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 401 {
			t.Errorf("weak session reconnect on %s: %d, want 401", mac, resp.StatusCode)
		}
	}
	var tokens int64
	config.DB.Model(&models.HotspotAccessToken{}).Count(&tokens)
	if tokens != 0 {
		t.Errorf("%d router-login tokens minted without proof", tokens)
	}
}
