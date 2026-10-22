package handlers

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
)

// nopRouter stands in for MikroTik so no test ever dials a router.
type nopRouter struct{}

func (nopRouter) WhitelistMAC(*models.Zone, string, *models.Package) error     { return nil }
func (nopRouter) PushHotspotUsers(*models.Zone, []models.Voucher) (int, error) { return 0, nil }
func (nopRouter) ReactivateCustomer(*models.Zone, *models.Customer) error      { return nil }

// fakeDarajaAPI answers OAuth, STK push and Transaction Status requests.
type fakeDarajaAPI struct {
	*httptest.Server
	stkPushes    int32
	statusChecks int32
	lastStatus   map[string]interface{}
}

func newFakeDarajaAPI(t *testing.T) *fakeDarajaAPI {
	t.Helper()
	f := &fakeDarajaAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v1/generate":
			io.WriteString(w, `{"access_token":"tok","expires_in":"3599"}`)
		case "/mpesa/stkpush/v1/processrequest":
			n := atomic.AddInt32(&f.stkPushes, 1)
			fmt.Fprintf(w, `{"ResponseCode":"0","CheckoutRequestID":"ws_CO_fake_%d","ResponseDescription":"Success. Request accepted for processing"}`, n)
		case "/mpesa/transactionstatus/v1/query":
			atomic.AddInt32(&f.statusChecks, 1)
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &f.lastStatus)
			fmt.Fprintf(w, `{"OriginatorConversationID":"oc-%s","ConversationID":"c-1","ResponseCode":"0","ResponseDescription":"Accept the service request successfully."}`, f.lastStatus["TransactionID"])
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func testInitiatorCert(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func putSetting(t *testing.T, key, val string) {
	t.Helper()
	v := val
	config.DB.Where("`key` = ?", key).Delete(&models.Setting{})
	if err := config.DB.Create(&models.Setting{Key: key, Value: &v}).Error; err != nil {
		t.Fatal(err)
	}
	invalidateAllSettings()
	services.InvalidateMpesaCaches()
}

// initMoneyServices wires a real MpesaService (pointed at baseURL when given)
// with a no-op router into the handler globals.
func initMoneyServices(t *testing.T, baseURL string) *services.MpesaService {
	t.Helper()
	sms := services.NewSmsService()
	svc := services.NewMpesaService(sms, services.NewVoucherService(sms), nil)
	svc.Router = nopRouter{}
	if baseURL != "" {
		svc.UseDarajaBaseURL(baseURL)
	}
	oldM, oldS, oldK := mpesaSvcGlobal, smsSvcGlobal, mikrotikSvcGlobal
	InitMpesaService(svc, sms, nil)
	t.Cleanup(func() { mpesaSvcGlobal, smsSvcGlobal, mikrotikSvcGlobal = oldM, oldS, oldK })
	return svc
}

// productionPlatform configures the shared Daraja app as live, with a B2B
// initiator (needed for Transaction Status checks).
func productionPlatform(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"mpesa_consumer_key": "ck", "mpesa_consumer_secret": "cs", "mpesa_shortcode": platformPaybill, "mpesa_passkey": "pk",
		"mpesa_environment": "production", "mpesa_callback_url": "https://api.example.com/api/v1/payments/callback",
		"mpesa_b2b_initiator": "apiop", "mpesa_b2b_password": "pw", "mpesa_b2b_cert": testInitiatorCert(t),
	} {
		putSetting(t, k, v)
	}
	old := config.Config.AppEnv
	config.Config.AppEnv = "production"
	t.Cleanup(func() { config.Config.AppEnv = old })
}

func postJSON(t *testing.T, h fiber.Handler, url, body string) (int, map[string]interface{}) {
	t.Helper()
	app := fiber.New()
	path := strings.SplitN(url, "?", 2)[0]
	app.Post(path, h)
	req := httptest.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func stkSuccessBody(checkout string, amount float64, receipt string) string {
	return fmt.Sprintf(`{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":%q,"ResultCode":0,"ResultDesc":"ok","CallbackMetadata":{"Item":[{"Name":"Amount","Value":%v},{"Name":"MpesaReceiptNumber","Value":%q},{"Name":"PhoneNumber","Value":254711000001}]}}}}`,
		checkout, amount, receipt)
}

func TestStkCallback_ForgedWithoutTheSecretIsRejected(t *testing.T) {
	setupC2BTestDB(t)
	initMoneyServices(t, "")
	a := seedISP(t, "a", "", 1)
	var pkg models.Package
	config.DB.Where("zone_id = ?", a.ZoneID).First(&pkg)
	checkout := "ws_CO_forge"
	p := models.Payment{ZoneID: a.ZoneID, PackageID: &pkg.ID, Phone: "254711000001", Amount: 100, Method: "mpesa", Status: "pending", MpesaTransactionID: &checkout}
	config.DB.Create(&p)

	for _, url := range []string{"/cb", "/cb?token=", "/cb?token=wrong-guess"} {
		if code, _ := postJSON(t, MpesaCallback, url, stkSuccessBody(checkout, 100, "FAKE001")); code != 401 {
			t.Errorf("%s: status %d, want 401", url, code)
		}
	}
	// No secret configured at all: nothing is accepted (it used to be "anyone").
	config.Config.MpesaCallbackSecret = ""
	if code, _ := postJSON(t, MpesaCallback, "/cb?token=", stkSuccessBody(checkout, 100, "FAKE002")); code != 401 {
		t.Errorf("empty secret: status %d, want 401", code)
	}
	config.Config.MpesaCallbackSecret = testCallbackSecret

	var got models.Payment
	config.DB.First(&got, p.ID)
	if got.Status != "pending" {
		t.Fatalf("forged callback changed the payment: %s", got.Status)
	}
	if code, _ := postJSON(t, MpesaCallback, "/cb?token="+testCallbackSecret, stkSuccessBody(checkout, 100, "UJKREAL1")); code != 200 {
		t.Fatalf("genuine callback: status %d", code)
	}
	config.DB.First(&got, p.ID)
	if got.Status != "completed" {
		t.Errorf("genuine callback not applied: %s", got.Status)
	}
}

func TestCallbackSecretProblem_RefusesProductionWithoutSecret(t *testing.T) {
	for env, secret := range map[string]string{"production": "", "prod": "", "": "", "Production": "short"} {
		if config.CallbackSecretProblem(config.AppConfig{AppEnv: env, MpesaCallbackSecret: secret}) == "" {
			t.Errorf("APP_ENV=%q secret=%q accepted", env, secret)
		}
	}
	if p := config.CallbackSecretProblem(config.AppConfig{AppEnv: "production", MpesaCallbackSecret: testCallbackSecret}); p != "" {
		t.Errorf("good secret refused: %s", p)
	}
	if p := config.CallbackSecretProblem(config.AppConfig{AppEnv: "local"}); p != "" {
		t.Errorf("local dev refused: %s", p)
	}
}

func TestC2B_WithoutTokenAndNoWayToVerify_IsParkedNotCredited(t *testing.T) {
	setupC2BTestDB(t)
	initMoneyServices(t, "") // sandbox platform, no initiator: can't verify
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "A1", "254711000001", "ZYR#A1")

	postC2BTo(t, "/c2b", platformPaybill, "UJKFORGE1", "ZYR#A1", "254711000001", 90)

	if got := balance(t, c.ID); got != 0 {
		t.Fatalf("tokenless confirmation credited KES %v without verification", got)
	}
	var u models.UnmatchedC2BPayment
	if err := config.DB.Where("trans_id = ?", "UJKFORGE1").First(&u).Error; err != nil {
		t.Fatal("not parked for review")
	}
	if u.Verification != "unverified" || u.Note == "" {
		t.Errorf("parked row: verification=%q note=%q", u.Verification, u.Note)
	}
	var l models.C2BTransaction
	config.DB.Where("trans_id = ?", "UJKFORGE1").First(&l)
	if l.Status != "parked" {
		t.Errorf("ledger status %q, want parked", l.Status)
	}

	// The customer can't claim an unverified payment by quoting its code.
	code, out := postJSON(t, PaymentVerifyCode, "/v", `{"code":"UJKFORGE1"}`)
	if code != 200 || out["pending"] != true {
		t.Errorf("verify-code on an unverified payment: %d %v", code, out)
	}
	if got := balance(t, c.ID); got != 0 {
		t.Errorf("self-claim credited an unverified payment: %v", got)
	}
}

func TestC2B_WithoutToken_CreditedOnlyWhenSafaricomConfirms(t *testing.T) {
	setupC2BTestDB(t)
	f := newFakeDarajaAPI(t)
	productionPlatform(t)
	initMoneyServices(t, f.URL)
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "A1", "254711000001", "ZYR#A1")

	// The live paybill posts without a token.
	postC2BTo(t, "/c2b", platformPaybill, "UJKGOOD1", "ZYR#A1", "254711000001", 40)
	if f.statusChecks != 1 || f.lastStatus["TransactionID"] != "UJKGOOD1" || f.lastStatus["PartyA"] != platformPaybill {
		t.Fatalf("status check not sent properly: %d %v", f.statusChecks, f.lastStatus)
	}
	if !strings.Contains(f.lastStatus["ResultURL"].(string), "token="+testCallbackSecret) {
		t.Errorf("ResultURL must carry the callback token: %v", f.lastStatus["ResultURL"])
	}
	if got := balance(t, c.ID); got != 0 {
		t.Fatalf("credited before Safaricom answered: %v", got)
	}

	result := func(transID string, amount float64) string {
		return fmt.Sprintf(`{"Result":{"ResultType":0,"ResultCode":0,"ResultDesc":"ok","OriginatorConversationID":"oc-%s","ConversationID":"c-1","TransactionID":"X","ResultParameters":{"ResultParameter":[
			{"Key":"ReceiptNo","Value":%q},{"Key":"TransactionStatus","Value":"Completed"},{"Key":"Amount","Value":%v},
			{"Key":"CreditPartyName","Value":"%s - ZYRA NET"},{"Key":"DebitPartyName","Value":"2547****0001 - JANE DOE"}]}}}`,
			transID, transID, amount, platformPaybill)
	}
	// A result without the token is ignored.
	if code, _ := postJSON(t, MpesaC2BVerifyResult, "/r", result("UJKGOOD1", 40)); code != 401 {
		t.Errorf("tokenless verify result: status %d", code)
	}
	postJSON(t, MpesaC2BVerifyResult, "/r?token="+testCallbackSecret, result("UJKGOOD1", 40))
	if got := balance(t, c.ID); got != 40 {
		t.Fatalf("balance %v after Safaricom confirmed KES 40", got)
	}
	// A replayed result credits nothing more.
	postJSON(t, MpesaC2BVerifyResult, "/r?token="+testCallbackSecret, result("UJKGOOD1", 40))
	postC2BTo(t, "/c2b", platformPaybill, "UJKGOOD1", "ZYR#A1", "254711000001", 40)
	if got := balance(t, c.ID); got != 40 {
		t.Errorf("replay credited again: %v", got)
	}

	// Inflated confirmation: Safaricom says KES 5, the request claimed 90.
	postC2BTo(t, "/c2b", platformPaybill, "UJKINFL1", "ZYR#A1", "254711000001", 90)
	postJSON(t, MpesaC2BVerifyResult, "/r?token="+testCallbackSecret, result("UJKINFL1", 5))
	if got := balance(t, c.ID); got != 40 {
		t.Errorf("mismatched confirmation credited: balance %v", got)
	}
	var u models.UnmatchedC2BPayment
	if config.DB.Where("trans_id = ?", "UJKINFL1").First(&u).Error != nil || u.Verification != "unverified" || !strings.Contains(u.Note, "did not match") {
		t.Errorf("mismatch not parked for review: %+v", u)
	}
}

func TestC2B_ConcurrentDuplicateDeliveriesCreditOnce(t *testing.T) {
	setupC2BTestDB(t)
	sqlDB, _ := config.DB.DB()
	sqlDB.SetMaxOpenConns(1)
	initMoneyServices(t, "")
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "A1", "254711000001", "ZYR#A1")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postC2B(t, platformPaybill, "UJKRACE1", "ZYR#A1", "254711000001", 30)
		}()
	}
	wg.Wait()
	if got := balance(t, c.ID); got != 30 {
		t.Errorf("balance = %v after 8 simultaneous deliveries of one KES 30 payment, want 30", got)
	}
	if n := count(t, &models.Payment{}); n != 1 {
		t.Errorf("%d payment rows, want 1", n)
	}
}

func TestStkPush_SecondTapReusesThePendingPrompt(t *testing.T) {
	setupC2BTestDB(t)
	f := newFakeDarajaAPI(t)
	productionPlatform(t)
	initMoneyServices(t, f.URL)
	a := seedISP(t, "a", "", 1)
	var pkg models.Package
	config.DB.Where("zone_id = ?", a.ZoneID).First(&pkg)

	body := fmt.Sprintf(`{"phone":"0712345678","package_id":%d,"mac":"AA:BB:CC:DD:EE:01"}`, pkg.ID)
	code1, out1 := postJSON(t, MpesaStkPush, "/stk", body)
	code2, out2 := postJSON(t, MpesaStkPush, "/stk", strings.Replace(body, "0712345678", "254712345678", 1))
	if code1 != 200 || code2 != 200 {
		t.Fatalf("status %d / %d", code1, code2)
	}
	d1, _ := out1["data"].(map[string]interface{})
	d2, _ := out2["data"].(map[string]interface{})
	if f.stkPushes != 1 {
		t.Errorf("%d STK prompts sent for two taps within 90s, want 1", f.stkPushes)
	}
	if d1["transaction_id"] == nil || d1["transaction_id"] != d2["transaction_id"] || d2["reused"] != true {
		t.Errorf("second tap should return the first CheckoutRequestID: %v vs %v", d1, d2)
	}
	if n := count(t, &models.Voucher{}); n != 0 {
		t.Errorf("%d vouchers created before any payment", n)
	}
	// The same device on the same zone with another number is also held back.
	postJSON(t, MpesaStkPush, "/stk", strings.Replace(body, "0712345678", "0799000111", 1))
	if f.stkPushes != 1 {
		t.Errorf("same device, other phone: %d prompts", f.stkPushes)
	}
}
