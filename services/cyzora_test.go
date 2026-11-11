package services

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// A fake Cyzora secret key — never a real one.
const testCyzoraKey = "sk_live_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeCyzora answers POST /charges, GET /charges/{id} and GET /charges. No
// real Cyzora call is ever made from tests.
type fakeCyzora struct {
	*httptest.Server
	mu        sync.Mutex
	creates   int32
	gets      int32
	lists     int32
	lastBody  map[string]interface{}
	lastIdem  string
	lastAuth  string
	createRes func(n int32) (int, string) // status + body for POST /charges
	charges   map[string]string           // id → JSON for GET /charges/{id}
}

func newFakeCyzora(t *testing.T) *fakeCyzora {
	t.Helper()
	f := &fakeCyzora{charges: map[string]string{}}
	f.createRes = func(n int32) (int, string) {
		return 200, fmt.Sprintf(`{"id":"chg-%d","reference":"CYZ%d","status":"pending","amount":3000,"currency":"KES","livemode":true}`, n, n)
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/charges":
			n := atomic.AddInt32(&f.creates, 1)
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.lastBody = map[string]interface{}{}
			json.Unmarshal(b, &f.lastBody)
			f.lastIdem = r.Header.Get("Idempotency-Key")
			f.mu.Unlock()
			code, body := f.createRes(n)
			w.WriteHeader(code)
			io.WriteString(w, body)
		case r.Method == http.MethodGet && r.URL.Path == "/charges":
			atomic.AddInt32(&f.lists, 1)
			if r.Header.Get("Authorization") != "Bearer "+testCyzoraKey {
				w.WriteHeader(401)
				io.WriteString(w, `{"error":"invalid api key"}`)
				return
			}
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/charges/"):
			atomic.AddInt32(&f.gets, 1)
			f.mu.Lock()
			body, ok := f.charges[strings.TrimPrefix(r.URL.Path, "/charges/")]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(404)
				io.WriteString(w, `{"error":"charge not found"}`)
				return
			}
			io.WriteString(w, body)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCyzora) setCharge(id, status string, cents int64, receipt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charges[id] = fmt.Sprintf(`{"id":%q,"status":%q,"amount":%d,"mpesa_receipt":%q,"livemode":true}`, id, status, cents, receipt)
}

// cyzoraISP makes an ISP with the given provider routing, one zone and a
// KES 30 hotspot package, and saves a (fake) platform Cyzora key.
func cyzoraISP(t *testing.T, provider string, fallback bool) (zoneID, pkgID uint) {
	t.Helper()
	org := models.Organization{Name: "ISP", Slug: "isp-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), PaymentProvider: provider, CyzoraFallback: fallback}
	if err := config.DB.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	if !fallback {
		config.DB.Model(&org).Update("cyzora_fallback", false)
	}
	zone := models.Zone{Name: "Z", Location: "Nairobi", RouterName: "r", RouterIP: "127.0.0.1", OrganizationID: org.ID}
	if err := config.DB.Create(&zone).Error; err != nil {
		t.Fatal(err)
	}
	pkg := models.Package{ZoneID: zone.ID, Name: "Day", Type: "hotspot", Price: 30, BillingCycle: "daily", Status: "active",
		SpeedUploadKbps: 2048, SpeedDownloadKbps: 5120}
	if err := config.DB.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	setSetting(t, CyzoraSecretKeySetting, testCyzoraKey)
	setSetting(t, "sms_enable_active", "no")
	setSetting(t, "sms_enable_voucher", "no")
	setSetting(t, "sms_enable_credit", "no")
	InvalidateMpesaCaches()
	return zone.ID, pkg.ID
}

func newPending(t *testing.T, zoneID, pkgID uint, age time.Duration) *models.Payment {
	t.Helper()
	p := models.Payment{ZoneID: zoneID, PackageID: &pkgID, Phone: "0712345678", Amount: 30, Currency: "KES",
		Method: "mpesa", Status: "pending", MacAddress: "AA:BB:CC:DD:EE:01", CreatedAt: time.Now().Add(-age)}
	if err := config.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return &p
}

func cyzoraSvcFor(t *testing.T, f *fakeCyzora) *MpesaService {
	svc := newTestMpesaService()
	svc.Cyzora.UseBaseURL(f.URL)
	return svc
}

func TestVerifyCyzoraSignature(t *testing.T) {
	body := []byte(`{"event":"charge.succeeded","charge_id":"c1"}`)
	sig := SignCyzoraBody(body, "whsec-abcdefghijklmnop")
	if !VerifyCyzoraSignature(body, sig, "whsec-abcdefghijklmnop") {
		t.Error("a correct signature was refused")
	}
	if !VerifyCyzoraSignature(body, strings.ToUpper(sig), "whsec-abcdefghijklmnop") {
		t.Error("an upper-case hex signature was refused")
	}
	for name, c := range map[string][3]string{
		"wrong secret":  {string(body), sig, "another-secret-123456"},
		"tampered body": {strings.Replace(string(body), "c1", "c2", 1), sig, "whsec-abcdefghijklmnop"},
		"no signature":  {string(body), "", "whsec-abcdefghijklmnop"},
		"no secret":     {string(body), SignCyzoraBody(body, ""), ""},
	} {
		if VerifyCyzoraSignature([]byte(c[0]), c[1], c[2]) {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestCyzoraKeyMode(t *testing.T) {
	for key, want := range map[string]string{
		testCyzoraKey:                         "live",
		"sk_test_" + strings.Repeat("ab", 32): "test",
		strings.Repeat("cd", 32):              "live", // older bare-hex keys
		"":                                    "",
		"pk_live_abc":                         "",
	} {
		if got := CyzoraKeyMode(key); got != want {
			t.Errorf("CyzoraKeyMode(%.12q) = %q, want %q", key, got, want)
		}
	}
	if ValidCyzoraSecretKey("sk_live_short") || !ValidCyzoraSecretKey(testCyzoraKey) {
		t.Error("ValidCyzoraSecretKey wrong")
	}
}

func TestStartSTK_CyzoraProviderSendsChargeAndPersistsProvider(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	p := newPending(t, zoneID, pkgID, 0)

	start, err := svc.StartSTK(p, "Vouch1", "Day")
	if err != nil {
		t.Fatal(err)
	}
	if start.Provider != ProviderCyzora || start.Reference != "chg-1" || start.IsMock {
		t.Fatalf("start = %+v", start)
	}
	got := reload(t, p.ID)
	if got.Provider != ProviderCyzora || got.ProviderRef == nil || *got.ProviderRef != "chg-1" ||
		got.MpesaTransactionID == nil || *got.MpesaTransactionID != "chg-1" || got.Status != "pending" {
		t.Errorf("payment = provider %q ref %v txn %v status %s", got.Provider, got.ProviderRef, got.MpesaTransactionID, got.Status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastAuth != "Bearer "+testCyzoraKey {
		t.Errorf("auth header %q", f.lastAuth)
	}
	if f.lastIdem != fmt.Sprintf("zyranet-payment-%d", p.ID) {
		t.Errorf("Idempotency-Key %q", f.lastIdem)
	}
	if f.lastBody["amount"] != float64(3000) || f.lastBody["phone"] != "254712345678" {
		t.Errorf("body amount/phone = %v / %v, want 3000 cents / 254712345678", f.lastBody["amount"], f.lastBody["phone"])
	}
	if ref, _ := f.lastBody["account_ref"].(string); ref == "" || len(ref) > 12 {
		t.Errorf("account_ref %q", ref)
	}
	meta, _ := f.lastBody["metadata"].(map[string]interface{})
	if meta["zyra_payment_id"] != fmt.Sprint(p.ID) || meta["org_id"] == "" {
		t.Errorf("metadata %v", meta)
	}
}

func TestStartSTK_Cyzora202IsPendingNotFailed(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	f.createRes = func(int32) (int, string) {
		return 202, `{"charge":{"id":"chg-202","status":"pending","amount":3000},"warning":"prompt may be on the phone"}`
	}
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	p := newPending(t, zoneID, pkgID, 0)

	start, err := svc.StartSTK(p, "", "Day")
	if err != nil {
		t.Fatalf("a 202 must not be an error: %v", err)
	}
	got := reload(t, p.ID)
	if start.Reference != "chg-202" || got.Status != "pending" || got.ProviderRef == nil || *got.ProviderRef != "chg-202" {
		t.Errorf("start=%+v status=%s ref=%v", start, got.Status, got.ProviderRef)
	}

	// A 202 that carries no charge: still pending, under an unguessable
	// reference, and the webhook can still match it by metadata.
	f.createRes = func(int32) (int, string) { return 202, `{"warning":"prompt may be on the phone"}` }
	p2 := newPending(t, zoneID, pkgID, 0)
	start2, err := svc.StartSTK(p2, "", "Day")
	if err != nil || len(start2.Reference) < 20 || !strings.HasPrefix(start2.Reference, "cyz_") {
		t.Fatalf("start2=%+v err=%v", start2, err)
	}
	if _, err := svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-late", Status: "succeeded",
		Amount: 3000, MpesaReceipt: "RCYZLATE1", Metadata: map[string]string{"zyra_payment_id": fmt.Sprint(p2.ID)}}); err != nil {
		t.Fatal(err)
	}
	if got := reload(t, p2.ID); got.Status != "completed" || got.ProviderRef == nil || *got.ProviderRef != "chg-late" {
		t.Errorf("metadata-matched payment: status=%s ref=%v", got.Status, got.ProviderRef)
	}
}

func TestStartSTK_CyzoraRejectionIsAnError(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	f.createRes = func(int32) (int, string) { return 403, `{"error":"merchant account is not verified"}` }
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	if _, err := svc.StartSTK(newPending(t, zoneID, pkgID, 0), "", "Day"); err == nil {
		t.Fatal("a 403 from Cyzora must fail the start")
	}
}

func TestStartSTK_CyzoraTestKeyRefusedOnDeployedServer(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	config.DB.Where("`key` = ?", CyzoraSecretKeySetting).Delete(&models.Setting{})
	setSetting(t, CyzoraSecretKeySetting, "sk_test_"+strings.Repeat("ab", 32))
	InvalidateMpesaCaches()
	svc := cyzoraSvcFor(t, f)
	if _, err := svc.StartSTK(newPending(t, zoneID, pkgID, 0), "", "Day"); err == nil {
		t.Fatal("a test key must not sell WiFi on a deployed server")
	}
	if f.creates != 0 {
		t.Errorf("%d charges created", f.creates)
	}
}

// fakeDarajaPush answers OAuth and STK push with the given behaviour.
func fakeDarajaPush(t *testing.T, oauthStatus int, stkStatus int, stkBody string) (*httptest.Server, *int32) {
	t.Helper()
	var pushes int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v1/generate":
			if oauthStatus != 200 {
				w.WriteHeader(oauthStatus)
				io.WriteString(w, `{"errorMessage":"Invalid Authentication passed"}`)
				return
			}
			io.WriteString(w, `{"access_token":"tok","expires_in":"3599"}`)
		case "/mpesa/stkpush/v1/processrequest":
			atomic.AddInt32(&pushes, 1)
			w.WriteHeader(stkStatus)
			io.WriteString(w, stkBody)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &pushes
}

func TestStartSTK_FallbackToCyzoraOnlyWhenDarajaPromptWasNotSent(t *testing.T) {
	cases := []struct {
		name       string
		fallback   bool
		oauth      int
		stkStatus  int
		stkBody    string
		wantCyzora bool
		wantErr    bool
	}{
		{"daraja ok", true, 200, 200, `{"ResponseCode":"0","CheckoutRequestID":"ws_CO_1","ResponseDescription":"ok"}`, false, false},
		{"daraja auth dead → cyzora", true, 400, 0, ``, true, false},
		{"daraja refused request → cyzora", true, 200, 400, `{"errorCode":"400.002.02","errorMessage":"Bad Request - Invalid BusinessShortCode"}`, true, false},
		{"prompt already on phone → no fallback", true, 200, 500, `{"errorCode":"500.001.1001","errorMessage":"Unable to lock subscriber, a transaction is already in process for the current subscriber"}`, false, true},
		{"other 5xx: unsure → no fallback", true, 200, 500, `{"errorCode":"500.003.02","errorMessage":"System is busy"}`, false, true},
		{"fallback off → no fallback", false, 400, 0, ``, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			setAppEnv(t, "production")
			productionPlatformSettings(t, "paybill", "")
			f := newFakeCyzora(t)
			zoneID, pkgID := cyzoraISP(t, ProviderDaraja, tc.fallback)
			srv, _ := fakeDarajaPush(t, tc.oauth, tc.stkStatus, tc.stkBody)
			svc := cyzoraSvcFor(t, f)
			svc.baseURLOverride = srv.URL
			p := newPending(t, zoneID, pkgID, 0)

			start, err := svc.StartSTK(p, "", "Day")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := atomic.LoadInt32(&f.creates) == 1; got != tc.wantCyzora {
				t.Errorf("cyzora charges = %d, want cyzora=%v", f.creates, tc.wantCyzora)
			}
			got := reload(t, p.ID)
			switch {
			case tc.wantCyzora:
				if start.Provider != ProviderCyzora || got.Provider != ProviderCyzora {
					t.Errorf("provider %q / row %q, want cyzora", start.Provider, got.Provider)
				}
			case !tc.wantErr:
				if start.Provider != ProviderDaraja || got.Provider != ProviderDaraja || *got.MpesaTransactionID != "ws_CO_1" {
					t.Errorf("provider %q / row %q, want daraja", start.Provider, got.Provider)
				}
			}
		})
	}
}

func TestCyzoraWebhook_CompletesThroughTheDarajaPathAndIsIdempotent(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	router := &fakeRouter{}
	svc.Router = router
	p := newPending(t, zoneID, pkgID, 0)
	if _, err := svc.StartSTK(p, "", "Day"); err != nil {
		t.Fatal(err)
	}

	evt := CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-1", Status: "succeeded", Amount: 3000,
		Currency: "KES", MpesaReceipt: "RCYZ00001", Metadata: map[string]string{"zyra_payment_id": fmt.Sprint(p.ID)}}
	for i := 0; i < 3; i++ { // Cyzora may deliver the same event more than once
		if _, err := svc.HandleCyzoraWebhook(evt); err != nil {
			t.Fatal(err)
		}
	}
	got := reload(t, p.ID)
	if got.Status != "completed" || got.MpesaReceiptNumber == nil || *got.MpesaReceiptNumber != "RCYZ00001" {
		t.Fatalf("status=%s receipt=%v", got.Status, got.MpesaReceiptNumber)
	}
	// Cyzora money lands in Zyra Net's Cyzora wallet: platform money the ISP
	// is owed through the payout ledger, like the shared paybill.
	if got.CollectedVia != "platform" {
		t.Errorf("collected_via = %q, want platform", got.CollectedVia)
	}
	if got.CustomerID == nil {
		t.Fatal("no customer activated")
	}
	var cust models.Customer
	config.DB.First(&cust, *got.CustomerID)
	if cust.Status != "active" || cust.ExpiresAt == nil || cust.PackageID != pkgID {
		t.Errorf("customer not activated: %+v", cust)
	}
	var customers, actions int64
	config.DB.Model(&models.Customer{}).Count(&customers)
	config.DB.Model(&models.PendingRouterAction{}).Where("payment_id = ?", p.ID).Count(&actions)
	if customers != 1 || actions != 1 {
		t.Errorf("after 3 deliveries: %d customers, %d router actions — want 1 and 1", customers, actions)
	}
	// The payout ledger sees it as owed to the ISP.
	var owed int64
	config.DB.Model(&models.Payment{}).Where("status = ? AND collected_via = ? AND payout_id IS NULL", "completed", "platform").Count(&owed)
	if owed != 1 {
		t.Errorf("%d payments owed to the ISP, want 1", owed)
	}
	// A failure arriving after the success changes nothing.
	svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.failed", ChargeID: "chg-1", Status: "failed"})
	if got := reload(t, p.ID); got.Status != "completed" {
		t.Errorf("late failure undid a completed payment: %s", got.Status)
	}
}

func TestCyzoraWebhook_TopUpCreditedOnceUnderConcurrentDeliveries(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, _ := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	sqlDB, _ := config.DB.DB()
	sqlDB.SetMaxOpenConns(1)
	cust := models.Customer{Name: "Jane", Phone: "254712345678", ZoneID: zoneID, Type: "hotspot", Status: "active"}
	config.DB.Create(&cust)
	p := models.Payment{CustomerID: &cust.ID, ZoneID: zoneID, Phone: "254712345678", Amount: 50, Method: "mpesa", Status: "pending"}
	config.DB.Create(&p)
	if _, err := svc.StartSTK(&p, "", "TopUp"); err != nil {
		t.Fatal(err)
	}
	evt := CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-1", Status: "succeeded", Amount: 5000, MpesaReceipt: "RCYZTOPUP"}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.HandleCyzoraWebhook(evt) }()
	}
	wg.Wait()
	if bal := CreditBalanceOf(config.DB, cust.ID); bal != 50 {
		t.Errorf("balance %.2f after 6 deliveries of one KES 50 top-up, want 50", bal)
	}
}

func TestCyzoraWebhook_UnderpaymentIsParkedNotGranted(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	cust := models.Customer{Name: "Jane", Phone: "254712345678", ZoneID: zoneID, PackageID: pkgID, Type: "hotspot", Status: "expired"}
	config.DB.Create(&cust)
	p := newPending(t, zoneID, pkgID, 0)
	config.DB.Model(p).Update("customer_id", cust.ID)
	if _, err := svc.StartSTK(p, "", "Day"); err != nil {
		t.Fatal(err)
	}

	svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-1", Status: "succeeded", Amount: 1000, MpesaReceipt: "RCYZUNDER"})

	got := reload(t, p.ID)
	if got.PackageID != nil || got.StatusReason == nil || !strings.Contains(*got.StatusReason, "Underpaid") {
		t.Errorf("underpaid payment: package=%v reason=%v — must not grant the package", got.PackageID, got.StatusReason)
	}
	var c models.Customer
	config.DB.First(&c, cust.ID)
	if c.Status != "expired" || c.CreditBalance != 10 {
		t.Errorf("customer status=%s credit=%.2f, want expired with KES 10 credit", c.Status, c.CreditBalance)
	}
}

func TestCyzoraWebhook_DuplicateReceiptRefused(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	a := newPending(t, zoneID, pkgID, 0)
	b := newPending(t, zoneID, pkgID, 0)
	b.Phone = "0722000111"
	config.DB.Model(b).Update("phone", "0722000111")
	svc.StartSTK(a, "", "Day")
	svc.StartSTK(b, "", "Day")

	svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-1", Status: "succeeded", Amount: 3000, MpesaReceipt: "RSAME0001"})
	svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-2", Status: "succeeded", Amount: 3000, MpesaReceipt: "RSAME0001"})

	if got := reload(t, a.ID); got.Status != "completed" {
		t.Errorf("first: %s", got.Status)
	}
	got := reload(t, b.ID)
	if got.Status == "completed" || got.StatusReason == nil || !strings.Contains(*got.StatusReason, "already used") {
		t.Errorf("second payment with the same receipt: status=%s reason=%v", got.Status, got.StatusReason)
	}
}

func TestCyzoraWebhook_FailedAndUnknown(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)
	p := newPending(t, zoneID, pkgID, 0)
	svc.StartSTK(p, "", "Day")

	svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.failed", ChargeID: "chg-1", Status: "failed", ResultCode: "1032", Error: "Request cancelled by user"})
	got := reload(t, p.ID)
	if got.Status != "failed" || got.StatusReason == nil || !strings.Contains(*got.StatusReason, "cancelled") {
		t.Errorf("status=%s reason=%v", got.Status, got.StatusReason)
	}
	note, err := svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-nobody", Status: "succeeded", Amount: 3000})
	if err != nil || note == "" {
		t.Errorf("unknown charge: note=%q err=%v", note, err)
	}
	// A Daraja payment is never touched by a Cyzora event naming its id.
	d := pendingPayment(t, zoneID, pkgID, "ws_CO_daraja", 30, 0)
	note, _ = svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "ws_CO_daraja", Status: "succeeded", Amount: 3000,
		Metadata: map[string]string{"zyra_payment_id": fmt.Sprint(d.ID)}})
	if got := reload(t, d.ID); got.Status != "pending" || note == "" {
		t.Errorf("daraja payment completed by a cyzora event: %s", got.Status)
	}
}

func TestCyzoraStatusPoll_CompletesThrottledAndWaits(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	svc := cyzoraSvcFor(t, f)

	young := newPending(t, zoneID, pkgID, 0)
	svc.StartSTK(young, "", "Day") // chg-1
	f.setCharge("chg-1", "succeeded", 3000, "RPOLL0001")
	if st, _ := svc.QueryAndUpdateSTKStatus(young); st != "pending" || f.gets != 0 {
		t.Fatalf("a 0s-old payment was polled (status %s, %d gets)", st, f.gets)
	}

	old := newPending(t, zoneID, pkgID, 30*time.Second)
	old.Phone = "0722000111"
	config.DB.Model(old).Update("phone", "0722000111")
	svc.StartSTK(old, "", "Day") // chg-2
	f.setCharge("chg-2", "pending", 3000, "")
	*old = reload(t, old.ID)
	if st, err := svc.QueryAndUpdateSTKStatus(old); st != "pending" || err != nil {
		t.Fatalf("pending charge: %s %v", st, err)
	}
	f.setCharge("chg-2", "succeeded", 3000, "RPOLL0002")
	if st, _ := svc.QueryAndUpdateSTKStatus(old); st != "pending" || f.gets != 1 {
		t.Errorf("second poll within 10s hit Cyzora (%d gets)", f.gets)
	}
	svc.cyzoraPolls.Delete(old.ID) // 10 seconds later…
	if st, err := svc.QueryAndUpdateSTKStatus(old); st != "completed" || err != nil {
		t.Fatalf("poll: %s %v", st, err)
	}
	if got := reload(t, old.ID); got.Status != "completed" || *got.MpesaReceiptNumber != "RPOLL0002" || got.CollectedVia != "platform" {
		t.Errorf("status=%s receipt=%v via=%s", got.Status, got.MpesaReceiptNumber, got.CollectedVia)
	}
}

func TestReconcilePendingCyzora_AndDarajaSweepSkipsCyzora(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	productionPlatformSettings(t, "paybill", "")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	daraja, darajaHits := fakeSTKQuery(t, map[string]string{})
	svc := cyzoraSvcFor(t, f)
	svc.baseURLOverride = daraja.URL

	mk := func(ref string, age time.Duration) *models.Payment {
		r := ref
		p := models.Payment{ZoneID: zoneID, PackageID: &pkgID, Phone: "0712345678", Amount: 30, Method: "mpesa", Status: "pending",
			Provider: ProviderCyzora, ProviderRef: &r, MpesaTransactionID: &r, CreatedAt: time.Now().Add(-age)}
		config.DB.Create(&p)
		return &p
	}
	paid := mk("chg-paid", 5*time.Minute)
	gone := mk("chg-gone", 3*time.Minute)
	limbo := mk("chg-limbo", 61*time.Minute)
	fresh := mk("chg-fresh", 20*time.Second)
	f.setCharge("chg-paid", "succeeded", 3000, "RSWEEP001")
	f.setCharge("chg-gone", "expired", 3000, "")
	f.setCharge("chg-limbo", "pending", 3000, "")
	f.setCharge("chg-fresh", "succeeded", 3000, "RSWEEP002")

	jobs := NewMoneyJobs(svc)
	jobs.ReconcilePendingSTK()
	if *darajaHits != 0 {
		t.Errorf("the Daraja sweep queried Safaricom %d times about Cyzora payments", *darajaHits)
	}
	jobs.ReconcilePendingCyzora()

	if got := reload(t, paid.ID); got.Status != "completed" {
		t.Errorf("paid: %s", got.Status)
	}
	if got := reload(t, gone.ID); got.Status != "failed" || *got.StatusReason != STKNoResponseReason {
		t.Errorf("expired: %s %v", got.Status, got.StatusReason)
	}
	if got := reload(t, limbo.ID); got.Status != "failed" || *got.StatusReason != STKUnconfirmedReason {
		t.Errorf("limbo: %s", got.Status)
	}
	if got := reload(t, fresh.ID); got.Status != "pending" {
		t.Errorf("a 20s-old payment was swept: %s", got.Status)
	}
}

func TestCyzoraTestKey_NotPaidOut(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "test")
	f := newFakeCyzora(t)
	zoneID, pkgID := cyzoraISP(t, ProviderCyzora, false)
	config.DB.Where("`key` = ?", CyzoraSecretKeySetting).Delete(&models.Setting{})
	setSetting(t, CyzoraSecretKeySetting, "sk_test_"+strings.Repeat("ab", 32))
	InvalidateMpesaCaches()
	svc := cyzoraSvcFor(t, f)
	p := newPending(t, zoneID, pkgID, 0)
	if _, err := svc.StartSTK(p, "", "Day"); err != nil {
		t.Fatal(err)
	}
	svc.HandleCyzoraWebhook(CyzoraWebhookEvent{Event: "charge.succeeded", ChargeID: "chg-1", Status: "succeeded", Amount: 3000, MpesaReceipt: "RTEST0001"})
	if got := reload(t, p.ID); got.Status != "completed" || got.CollectedVia != "" {
		t.Errorf("test-key payment: status=%s collected_via=%q — must never enter the payout ledger", got.Status, got.CollectedVia)
	}
}

func TestCyzoraTestKeyCheck(t *testing.T) {
	setupTestDB(t)
	f := newFakeCyzora(t)
	c := NewCyzoraService()
	c.UseBaseURL(f.URL)
	if ok, _ := c.TestKey(); ok {
		t.Error("no key saved, but the check passed")
	}
	setSetting(t, CyzoraSecretKeySetting, testCyzoraKey)
	InvalidateMpesaCaches()
	if ok, msg := c.TestKey(); !ok {
		t.Errorf("good key failed: %s", msg)
	}
	if f.creates != 0 {
		t.Error("the key check created a charge")
	}
}

func TestInsights_CyzoraIsItsOwnMethod(t *testing.T) {
	if k, l := methodProviderLabel("mpesa", ProviderCyzora); k != "cyzora" || l != "Cyzora" {
		t.Errorf("cyzora → %s/%s", k, l)
	}
	if k, _ := methodProviderLabel("mpesa", ProviderDaraja); k != "mpesa" {
		t.Errorf("daraja → %s", k)
	}
	if k, _ := methodProviderLabel("mpesa", ""); k != "mpesa" {
		t.Errorf("old rows → %s", k)
	}
}
