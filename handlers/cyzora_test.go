package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
)

// Fake credentials only — no real Cyzora key or secret is used in tests.
const (
	fakeCyzoraKey     = "sk_live_" + "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	fakeWebhookSecret = "whsec_test_0123456789abcdef"
)

// fakeCyzoraAPI answers POST /charges with chg-<n> (pending).
func fakeCyzoraAPI(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/charges":
			i := atomic.AddInt32(&n, 1)
			fmt.Fprintf(w, `{"id":"chg-%d","status":"pending","amount":10000,"livemode":true}`, i)
		case r.Method == "GET" && r.URL.Path == "/charges":
			if r.Header.Get("Authorization") != "Bearer "+fakeCyzoraKey {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":"invalid api key"}`)
				return
			}
			io.WriteString(w, `[]`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func cyzoraHandlerSetup(t *testing.T) (isp, *int32) {
	t.Helper()
	setupC2BTestDB(t)
	old := config.Config.AppEnv
	config.Config.AppEnv = "production"
	t.Cleanup(func() { config.Config.AppEnv = old })
	srv, n := fakeCyzoraAPI(t)
	svc := initMoneyServices(t, "")
	svc.Cyzora.UseBaseURL(srv.URL)
	putSetting(t, services.CyzoraSecretKeySetting, fakeCyzoraKey)
	putSetting(t, services.CyzoraWebhookSecretSetting, fakeWebhookSecret)
	for _, k := range []string{"sms_enable_active", "sms_enable_voucher", "sms_enable_credit"} {
		putSetting(t, k, "no")
	}
	a := seedISP(t, "cyz", "", 1)
	config.DB.Model(&models.Organization{}).Where("id = ?", a.OrgID).Update("payment_provider", services.ProviderCyzora)
	config.DB.Model(&models.Zone{}).Where("id = ?", a.ZoneID).Update("last_seen_at", time.Now())
	return a, n
}

func postSigned(t *testing.T, body, sig string) (int, map[string]interface{}) {
	t.Helper()
	app := fiber.New()
	app.Post("/wh", CyzoraWebhook)
	req := httptest.NewRequest("POST", "/wh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Cyzora-Signature", sig)
	}
	req.Header.Set("X-Cyzora-Event", "charge.succeeded")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func TestCyzora_StkPushThenSignedWebhookActivates(t *testing.T) {
	a, creates := cyzoraHandlerSetup(t)
	var pkg models.Package
	config.DB.Where("zone_id = ?", a.ZoneID).First(&pkg)

	code, out := postJSON(t, MpesaStkPush, "/stk", fmt.Sprintf(`{"phone":"0712345678","package_id":%d,"mac":"AA:BB:CC:DD:EE:01"}`, pkg.ID))
	d, _ := out["data"].(map[string]interface{})
	if code != 200 || d["transaction_id"] != "chg-1" || d["provider"] != "cyzora" || *creates != 1 {
		t.Fatalf("stk push: %d %v (charges %d)", code, out, *creates)
	}
	var p models.Payment
	config.DB.First(&p, uint(d["payment_id"].(float64)))
	if p.Provider != "cyzora" || p.ProviderRef == nil || *p.ProviderRef != "chg-1" {
		t.Fatalf("payment provider %q ref %v", p.Provider, p.ProviderRef)
	}

	body := fmt.Sprintf(`{"event":"charge.succeeded","charge_id":"chg-1","reference":"R1","amount":10000,"currency":"KES","status":"succeeded","mpesa_receipt":"RCYZHOOK1","livemode":true,"metadata":{"zyra_payment_id":"%d"}}`, p.ID)

	// Bad / missing signature → 401 and nothing happens.
	if code, _ := postSigned(t, body, services.SignCyzoraBody([]byte(body), "wrong-secret-xxxxxxxx")); code != 401 {
		t.Errorf("bad signature: %d, want 401", code)
	}
	if code, _ := postSigned(t, body, ""); code != 401 {
		t.Errorf("no signature: %d, want 401", code)
	}
	config.DB.First(&p, p.ID)
	if p.Status != "pending" {
		t.Fatalf("an unsigned webhook changed the payment: %s", p.Status)
	}

	sig := services.SignCyzoraBody([]byte(body), fakeWebhookSecret)
	for i := 0; i < 2; i++ { // the second delivery is a no-op
		if code, out := postSigned(t, body, sig); code != 200 {
			t.Fatalf("delivery %d: %d %v", i+1, code, out)
		}
	}
	config.DB.First(&p, p.ID)
	if p.Status != "completed" || p.CollectedVia != "platform" || p.CustomerID == nil {
		t.Fatalf("after webhook: status=%s via=%q customer=%v", p.Status, p.CollectedVia, p.CustomerID)
	}
	if n := count(t, &models.Customer{}); n != 1 {
		t.Errorf("%d customers after two deliveries, want 1", n)
	}

	// Unknown charge → 200 with a note.
	unk := `{"event":"charge.succeeded","charge_id":"chg-nope","amount":100,"status":"succeeded"}`
	if code, out := postSigned(t, unk, services.SignCyzoraBody([]byte(unk), fakeWebhookSecret)); code != 200 || out["note"] == nil {
		t.Errorf("unknown charge: %d %v", code, out)
	}
}

func TestCyzoraWebhook_RejectedWithoutWebhookSecret(t *testing.T) {
	cyzoraHandlerSetup(t)
	config.DB.Where("`key` = ?", services.CyzoraWebhookSecretSetting).Delete(&models.Setting{})
	invalidateAllSettings()
	services.InvalidateMpesaCaches()
	old := config.Config.CyzoraWebhookSecret
	config.Config.CyzoraWebhookSecret = ""
	t.Cleanup(func() { config.Config.CyzoraWebhookSecret = old })
	body := `{"event":"charge.succeeded","charge_id":"chg-1"}`
	// Even "signed" with an empty secret, nothing is accepted.
	if code, _ := postSigned(t, body, services.SignCyzoraBody([]byte(body), "")); code != 401 {
		t.Errorf("no secret configured: %d, want 401", code)
	}
}

func cyzoraPlatformApp(role string) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error { c.Locals("platformRole", role); return c.Next() })
	app.Get("/cyzora", PlatformCyzoraShow)
	app.Post("/cyzora", middleware.RequirePlatformOwner(), PlatformCyzoraUpdate)
	app.Post("/cyzora/test", PlatformCyzoraTest)
	app.Get("/settings", SettingsIndex)
	app.Put("/orgs/:id", OrganizationUpdate)
	app.Get("/orgs/:id", OrganizationShow)
	return app
}

func cyzCall(t *testing.T, app *fiber.App, method, url, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPlatformCyzora_SecretsNeverReturned(t *testing.T) {
	setupC2BTestDB(t)
	owner := cyzoraPlatformApp(middleware.PlatformOwner)

	code, body := cyzCall(t, owner, "GET", "/cyzora", "")
	if code != 200 || !strings.Contains(body, `"secret_key_set":false`) || !strings.Contains(body, `"webhook_url":"https://api.zyranet.co.ke/api/v1/payments/cyzora/webhook"`) {
		t.Fatalf("empty GET: %d %s", code, body)
	}
	code, body = cyzCall(t, owner, "POST", "/cyzora", fmt.Sprintf(`{"secret_key":%q,"webhook_secret":%q}`, fakeCyzoraKey, fakeWebhookSecret))
	if code != 200 || strings.Contains(body, fakeCyzoraKey) || strings.Contains(body, fakeWebhookSecret) {
		t.Fatalf("POST leaked or failed: %d %s", code, body)
	}
	if !strings.Contains(body, `"secret_key_set":true`) || !strings.Contains(body, `"webhook_secret_set":true`) || !strings.Contains(body, `"mode":"live"`) {
		t.Errorf("POST answer: %s", body)
	}
	// Blank fields keep what is stored.
	cyzCall(t, owner, "POST", "/cyzora", `{"secret_key":"","webhook_secret":"  "}`)
	if services.CyzoraSecretKey() != fakeCyzoraKey || services.CyzoraWebhookSecret() != fakeWebhookSecret {
		t.Error("blank update erased a stored secret")
	}
	// A malformed key is refused.
	if code, _ := cyzCall(t, owner, "POST", "/cyzora", `{"secret_key":"not-a-key"}`); code != 422 {
		t.Errorf("bad key: %d", code)
	}
	// Non-owners can't change it.
	staff := cyzoraPlatformApp("support")
	if code, _ := cyzCall(t, staff, "POST", "/cyzora", fmt.Sprintf(`{"secret_key":%q}`, fakeCyzoraKey)); code != 403 {
		t.Errorf("staff update: %d, want 403", code)
	}
	for _, app := range []*fiber.App{owner, staff} {
		_, body := cyzCall(t, app, "GET", "/cyzora", "")
		_, settings := cyzCall(t, app, "GET", "/settings", "")
		for _, b := range []string{body, settings} {
			if strings.Contains(b, fakeCyzoraKey) || strings.Contains(b, fakeWebhookSecret) || strings.Contains(b, "cyzora_secret_key") {
				t.Errorf("secret visible: %s", b)
			}
		}
	}
}

func TestPlatformCyzora_TestKeyMovesNoMoney(t *testing.T) {
	setupC2BTestDB(t)
	srv, creates := fakeCyzoraAPI(t)
	old := cyzoraSvc
	cyzoraSvc = services.NewCyzoraService()
	cyzoraSvc.UseBaseURL(srv.URL)
	t.Cleanup(func() { cyzoraSvc = old })
	app := cyzoraPlatformApp("support")

	_, body := cyzCall(t, app, "POST", "/cyzora/test", "")
	if !strings.Contains(body, `"ok":false`) {
		t.Errorf("no key: %s", body)
	}
	putSetting(t, services.CyzoraSecretKeySetting, fakeCyzoraKey)
	_, body = cyzCall(t, app, "POST", "/cyzora/test", "")
	if !strings.Contains(body, `"ok":true`) || *creates != 0 {
		t.Errorf("good key: %s (charges %d)", body, *creates)
	}
}

func TestOrganizationUpdate_PaymentProviderRules(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "prov", "", 1)
	url := fmt.Sprintf("/orgs/%d", a.OrgID)
	owner := cyzoraPlatformApp(middleware.PlatformOwner)
	staff := cyzoraPlatformApp("support")

	if code, body := cyzCall(t, owner, "PUT", url, `{"payment_provider":"cyzora"}`); code != 422 {
		t.Errorf("cyzora without a key: %d %s", code, body)
	}
	putSetting(t, services.CyzoraSecretKeySetting, fakeCyzoraKey)
	if code, _ := cyzCall(t, staff, "PUT", url, `{"payment_provider":"cyzora"}`); code != 403 {
		t.Errorf("staff switching provider: %d, want 403", code)
	}
	if code, _ := cyzCall(t, owner, "PUT", url, `{"payment_provider":"paypal"}`); code != 422 {
		t.Errorf("bad provider: %d", code)
	}
	if code, _ := cyzCall(t, owner, "PUT", url, `{"cyzora_fallback":"yes"}`); code != 422 {
		t.Errorf("non-bool fallback: %d", code)
	}
	code, body := cyzCall(t, owner, "PUT", url, `{"payment_provider":"cyzora","cyzora_fallback":true}`)
	if code != 200 || !strings.Contains(body, `"payment_provider":"cyzora"`) || !strings.Contains(body, `"cyzora_fallback":true`) {
		t.Fatalf("owner switch: %d %s", code, body)
	}
	// Staff saving the full form with unchanged values is fine.
	if code, body := cyzCall(t, staff, "PUT", url, `{"contact_email":"x@y.co","payment_provider":"cyzora","cyzora_fallback":true}`); code != 200 {
		t.Errorf("staff unchanged save: %d %s", code, body)
	}
	if _, body := cyzCall(t, staff, "GET", url, ""); !strings.Contains(body, `"payment_provider":"cyzora"`) {
		t.Errorf("show: %s", body)
	}
	if p, _ := services.PaymentRouting(a.ZoneID); p != services.ProviderCyzora {
		t.Errorf("routing = %s", p)
	}
}
