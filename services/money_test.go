package services

import (
	"encoding/json"
	"errors"
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

// fakeRouter stands in for MikroTik. failFirst makes the first N calls fail.
type fakeRouter struct {
	mu        sync.Mutex
	failFirst int
	calls     []string
}

func (f *fakeRouter) record(what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what)
	if len(f.calls) <= f.failFirst {
		return errors.New("router unreachable")
	}
	return nil
}
func (f *fakeRouter) WhitelistMAC(_ *models.Zone, mac string, _ *models.Package) error {
	return f.record("whitelist " + mac)
}
func (f *fakeRouter) PushHotspotUsers(_ *models.Zone, v []models.Voucher) (int, error) {
	return len(v), f.record("voucher " + v[0].Code)
}
func (f *fakeRouter) ReactivateCustomer(_ *models.Zone, c *models.Customer) error {
	return f.record("reactivate")
}
func (f *fakeRouter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitFor polls cond for up to 2s (router actions run in a goroutine).
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func pendingPayment(t *testing.T, zoneID, pkgID uint, checkout string, amount float64, age time.Duration) models.Payment {
	t.Helper()
	id := checkout
	p := models.Payment{ZoneID: zoneID, PackageID: &pkgID, Phone: "254712345678", Amount: amount, Currency: "KES",
		Method: "mpesa", Status: "pending", MpesaTransactionID: &id, CreatedAt: time.Now().Add(-age)}
	if err := config.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func reload(t *testing.T, id uint) models.Payment {
	t.Helper()
	var p models.Payment
	if err := config.DB.First(&p, id).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHandleCallback_UnderpaymentDoesNotGrantPackage(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t) // KES 30 package
	svc := newTestMpesaService()
	cust := models.Customer{Name: "Jane", Phone: "254712345678", ZoneID: zoneID, PackageID: pkgID, Type: "hotspot", Status: "expired"}
	config.DB.Create(&cust)
	p := pendingPayment(t, zoneID, pkgID, "ws_CO_under", 30, 0)
	config.DB.Model(&p).Update("customer_id", cust.ID)

	if err := svc.HandleCallback(successCallbackPayload("ws_CO_under", 10, "RCTUNDER1", "254712345678")); err != nil {
		t.Fatal(err)
	}

	got := reload(t, p.ID)
	if got.Status != "completed" || got.Amount != 10 || got.PackageID != nil {
		t.Errorf("payment: status=%s amount=%v package=%v — want completed, KES 10 actually paid, no package", got.Status, got.Amount, got.PackageID)
	}
	if got.StatusReason == nil || !strings.Contains(*got.StatusReason, "Underpaid") {
		t.Errorf("status_reason = %v, want an Underpaid flag", got.StatusReason)
	}
	var c models.Customer
	config.DB.First(&c, cust.ID)
	if c.CreditBalance != 10 {
		t.Errorf("credit balance = %v, want the KES 10 paid", c.CreditBalance)
	}
	if c.Status != "expired" || c.ExpiresAt != nil {
		t.Errorf("customer was activated on an underpayment: status=%s expires=%v", c.Status, c.ExpiresAt)
	}
	// A duplicate delivery must not credit again.
	svc.HandleCallback(successCallbackPayload("ws_CO_under", 10, "RCTUNDER1", "254712345678"))
	config.DB.First(&c, cust.ID)
	if c.CreditBalance != 10 {
		t.Errorf("duplicate underpaid callback credited again: balance %v", c.CreditBalance)
	}
}

func TestHandleCallback_FullPaymentStillGrantsPackage(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()
	p := pendingPayment(t, zoneID, pkgID, "ws_CO_full", 30, 0)
	if err := svc.HandleCallback(successCallbackPayload("ws_CO_full", 30, "RCTFULL1", "254712345678")); err != nil {
		t.Fatal(err)
	}
	got := reload(t, p.ID)
	if got.Status != "completed" || got.PackageID == nil || got.CustomerID == nil {
		t.Fatalf("payment = %+v", got)
	}
	var c models.Customer
	config.DB.First(&c, *got.CustomerID)
	if c.Status != "active" || c.ExpiresAt == nil {
		t.Errorf("customer not activated: %+v", c)
	}
}

func TestHandleCallback_LateRealCallbackReplacesQueryPlaceholder(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()
	p := pendingPayment(t, zoneID, pkgID, "ws_CO_late", 30, 3*time.Minute)

	// The status query completed it first, with no receipt in its reply.
	if err := svc.ProcessPaymentSuccess(&p, "QRY_ws_CO_late", p.Phone); err != nil {
		t.Fatal(err)
	}
	// Then Safaricom's real callback arrives.
	if err := svc.HandleCallback(successCallbackPayload("ws_CO_late", 30, "UJK7REAL01", "254712345678")); err != nil {
		t.Fatal(err)
	}
	got := reload(t, p.ID)
	if got.MpesaReceiptNumber == nil || *got.MpesaReceiptNumber != "UJK7REAL01" {
		t.Errorf("receipt = %v, want the real receipt to replace the QRY_ placeholder", got.MpesaReceiptNumber)
	}
	var custs int64
	config.DB.Model(&models.Customer{}).Count(&custs)
	if custs != 1 {
		t.Errorf("late callback re-ran the success side effects: %d customers", custs)
	}
	// A real receipt is never overwritten by another value.
	svc.HandleCallback(successCallbackPayload("ws_CO_late", 30, "UJK7OTHER2", "254712345678"))
	if got := reload(t, p.ID); *got.MpesaReceiptNumber != "UJK7REAL01" {
		t.Errorf("real receipt overwritten: %s", *got.MpesaReceiptNumber)
	}
}

func TestHandleCallback_SuccessWithoutAmountWaitsForStatusQuery(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()
	p := pendingPayment(t, zoneID, pkgID, "ws_CO_noamt", 30, 0)
	payload := failureCallbackPayload("ws_CO_noamt", 0, "processed")
	if err := svc.HandleCallback(payload); err != nil {
		t.Fatal(err)
	}
	if got := reload(t, p.ID); got.Status != "pending" {
		t.Errorf("status = %s; a success with no amount must wait for the status query", got.Status)
	}
}

func TestAnonymousPurchase_VoucherCreatedOnlyAfterPayment(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()
	router := svc.Router.(*fakeRouter)
	p := pendingPayment(t, zoneID, pkgID, "ws_CO_anon", 30, 0)

	var n int64
	config.DB.Model(&models.Voucher{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d vouchers exist before payment", n)
	}
	if err := svc.HandleCallback(successCallbackPayload("ws_CO_anon", 30, "RCTANON1", "254712345678")); err != nil {
		t.Fatal(err)
	}
	got := reload(t, p.ID)
	if got.VoucherID == nil {
		t.Fatal("no voucher linked after payment")
	}
	var v models.Voucher
	config.DB.First(&v, *got.VoucherID)
	if v.Status != "unused" || v.PackageID != pkgID {
		t.Errorf("voucher = %+v", v)
	}
	if !waitFor(t, func() bool { return router.count() == 1 }) {
		t.Errorf("voucher was never pushed to the router")
	}
}

func TestRouterActions_PersistedAndRetriedWithBackoff(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()
	router := &fakeRouter{failFirst: 2}
	svc.Router = router

	svc.EnqueueRouterAction(models.PendingRouterAction{ZoneID: zoneID, Action: RouterActionWhitelistMAC, MacAddress: "aa-bb-cc-dd-ee-ff", PackageID: &pkgID})
	var a models.PendingRouterAction
	if !waitFor(t, func() bool {
		config.DB.First(&a)
		return a.Attempts == 1 && a.Status == "pending"
	}) {
		t.Fatalf("first attempt not recorded: %+v", a)
	}
	if a.NextAttemptAt.Before(time.Now().Add(20 * time.Second)) {
		t.Errorf("retry scheduled too soon: %v", a.NextAttemptAt)
	}
	// Not due yet: the job leaves it alone.
	svc.RunDueRouterActions()
	if router.count() != 1 {
		t.Fatalf("ran before its retry time: %d calls", router.count())
	}
	// Time passes (or the API restarts — it's all in the table).
	for i := 0; i < 2; i++ {
		config.DB.Model(&models.PendingRouterAction{}).Where("id = ?", a.ID).Update("next_attempt_at", time.Now().Add(-time.Second))
		svc.RunDueRouterActions()
	}
	config.DB.First(&a, a.ID)
	if a.Status != "done" || a.Attempts != 3 || router.count() != 3 {
		t.Errorf("after retries: status=%s attempts=%d calls=%d, want done/3/3", a.Status, a.Attempts, router.count())
	}
	if router.calls[2] != "whitelist AA:BB:CC:DD:EE:FF" {
		t.Errorf("MAC not normalised: %q", router.calls[2])
	}
}

func TestAdjustCredit_AtomicUnderConcurrencyAndNeverNegative(t *testing.T) {
	db := setupTestDB(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1) // SQLite: one writer at a time, like row locks would
	zoneID, pkgID := seedZone(t)
	c := models.Customer{Name: "C", Phone: "254711000009", ZoneID: zoneID, PackageID: pkgID, Type: "hotspot"}
	config.DB.Create(&c)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			AdjustCredit(config.DB, c.ID, 10)
		}()
	}
	wg.Wait()
	if got := CreditBalanceOf(config.DB, c.ID); got != 500 {
		t.Fatalf("balance = %v after 50 concurrent KES 10 credits, want 500 (lost updates)", got)
	}

	var ok int32
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if done, _ := AdjustCredit(config.DB, c.ID, -10); done {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 50 || CreditBalanceOf(config.DB, c.ID) != 0 {
		t.Errorf("%d debits succeeded, balance %v — want exactly 50 and 0 (never negative)", ok, CreditBalanceOf(config.DB, c.ID))
	}
}

func TestRecentPendingSTK_SamePhoneOrDeviceWithin90s(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	p := pendingPayment(t, zoneID, pkgID, "ws_CO_dup", 30, 30*time.Second)
	config.DB.Model(&p).Updates(map[string]interface{}{"phone": "0712345678", "mac_address": "AA:BB:CC:00:00:01"})

	if got, ok := RecentPendingSTK("254712345678", "", zoneID, 90*time.Second); !ok || got.ID != p.ID {
		t.Errorf("same phone (other format) not found")
	}
	if got, ok := RecentPendingSTK("254700000000", "AA:BB:CC:00:00:01", zoneID, 90*time.Second); !ok || got.ID != p.ID {
		t.Errorf("same device in the same zone not found")
	}
	if _, ok := RecentPendingSTK("254700000000", "AA:BB:CC:00:00:01", zoneID+1, 90*time.Second); ok {
		t.Errorf("same device in another zone matched")
	}
	if _, ok := RecentPendingSTK("254712345678", "", zoneID, 10*time.Second); ok {
		t.Errorf("older than the window matched")
	}
	config.DB.Model(&p).Update("status", "failed")
	if _, ok := RecentPendingSTK("254712345678", "", zoneID, 90*time.Second); ok {
		t.Errorf("a finished push still blocks new ones")
	}
}

// fakeSTKQuery answers Daraja STK status queries from a CheckoutRequestID → reply map.
func fakeSTKQuery(t *testing.T, replies map[string]string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v1/generate":
			io.WriteString(w, `{"access_token":"tok","expires_in":"3599"}`)
		case "/mpesa/stkpushquery/v1/query":
			atomic.AddInt32(&hits, 1)
			var b map[string]interface{}
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &b)
			reply, ok := replies[b["CheckoutRequestID"].(string)]
			if !ok {
				w.WriteHeader(500)
				io.WriteString(w, `{"errorCode":"500.001.1001","errorMessage":"The transaction is being processed"}`)
				return
			}
			io.WriteString(w, reply)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestReconcilePendingSTK_ResolvesStuckPayments(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	productionPlatformSettings(t, "paybill", "")
	zoneID, pkgID := seedZone(t)
	srv, hits := fakeSTKQuery(t, map[string]string{
		"ws_CO_paid":    `{"ResponseCode":"0","ResultCode":"0","ResultDesc":"The service request is processed successfully."}`,
		"ws_CO_ignored": `{"ResponseCode":"0","ResultCode":"1037","ResultDesc":"DS timeout user cannot be reached"}`,
		"ws_CO_nopin":   `{"ResponseCode":"0","ResultCode":"1038","ResultDesc":"Customer did not respond"}`,
		"ws_CO_fresh":   `{"ResponseCode":"0","ResultCode":"0","ResultDesc":"ok"}`,
	})
	svc := newTestMpesaService()
	svc.baseURLOverride = srv.URL

	paid := pendingPayment(t, zoneID, pkgID, "ws_CO_paid", 30, 3*time.Minute)
	ignored := pendingPayment(t, zoneID, pkgID, "ws_CO_ignored", 30, 5*time.Minute)
	nopin := pendingPayment(t, zoneID, pkgID, "ws_CO_nopin", 30, 4*time.Minute)
	fresh := pendingPayment(t, zoneID, pkgID, "ws_CO_fresh", 30, 30*time.Second)  // too new: callback may still come
	limbo := pendingPayment(t, zoneID, pkgID, "ws_CO_limbo", 30, 31*time.Minute)  // Safaricom never answers
	ancient := pendingPayment(t, zoneID, pkgID, "ws_CO_old", 30, 30*24*time.Hour) // outside the window: untouched

	NewMoneyJobs(svc).ReconcilePendingSTK()

	if got := reload(t, paid.ID); got.Status != "completed" || got.MpesaReceiptNumber == nil || *got.MpesaReceiptNumber != "QRY_ws_CO_paid" {
		t.Errorf("paid: status=%s receipt=%v", got.Status, got.MpesaReceiptNumber)
	}
	for _, p := range []models.Payment{ignored, nopin} {
		got := reload(t, p.ID)
		if got.Status != "failed" || got.StatusReason == nil || *got.StatusReason != STKNoResponseReason {
			t.Errorf("%s: status=%s reason=%v, want failed with the didn't-respond message", *p.MpesaTransactionID, got.Status, got.StatusReason)
		}
	}
	if got := reload(t, fresh.ID); got.Status != "pending" {
		t.Errorf("a 30s-old push was reconciled: %s", got.Status)
	}
	if got := reload(t, limbo.ID); got.Status != "failed" || *got.StatusReason != STKUnconfirmedReason {
		t.Errorf("limbo: status=%s, want failed after 30 minutes without an answer", got.Status)
	}
	if got := reload(t, ancient.ID); got.Status != "pending" {
		t.Errorf("an old pending payment outside the window was touched")
	}
	if *hits != 4 {
		t.Errorf("%d STK queries, want 4 (paid, ignored, nopin, limbo)", *hits)
	}
}

func TestFriendlySTKFailureReason_KnownCodes(t *testing.T) {
	for code, want := range map[float64]string{
		1037: STKNoResponseReason, 1038: STKNoResponseReason,
		4999: STKUnconfirmedReason, 2029: STKUnconfirmedReason,
	} {
		if got := friendlySTKFailureReason(code, "x"); got != want {
			t.Errorf("%v → %q", code, got)
		}
	}
	for _, code := range []float64{1, 1032, 2001} {
		got := friendlySTKFailureReason(code, "x")
		if got == "" || strings.Contains(got, "could not be completed") {
			t.Errorf("%v has no specific message: %q", code, got)
		}
	}
}

func TestTransactionStatusResult_Matches(t *testing.T) {
	l := &models.C2BTransaction{TransID: "UJK1ABC", Amount: 50, BusinessShortCode: "7289306", MSISDN: "254712345678"}
	result := func(status string, amount float64, credit, debit string) C2BStatusResult {
		r, _ := ParseTransactionStatusResult(map[string]interface{}{"Result": map[string]interface{}{
			"ResultCode": float64(0), "OriginatorConversationID": "oc1",
			"ResultParameters": map[string]interface{}{"ResultParameter": []interface{}{
				map[string]interface{}{"Key": "ReceiptNo", "Value": "UJK1ABC"},
				map[string]interface{}{"Key": "TransactionStatus", "Value": status},
				map[string]interface{}{"Key": "Amount", "Value": amount},
				map[string]interface{}{"Key": "CreditPartyName", "Value": credit},
				map[string]interface{}{"Key": "DebitPartyName", "Value": debit},
			}},
		}})
		return r
	}
	if ok, why := result("Completed", 50, "7289306 - ZYRA NET", "2547****5678 - JANE DOE").Matches(l); !ok {
		t.Errorf("genuine payment rejected: %s", why)
	}
	for name, r := range map[string]C2BStatusResult{
		"wrong amount":    result("Completed", 5, "7289306 - ZYRA NET", "254712345678 - JANE"),
		"other shortcode": result("Completed", 50, "123456 - OTHER", "254712345678 - JANE"),
		"not completed":   result("Failed", 50, "7289306 - ZYRA NET", "254712345678 - JANE"),
		"other payer":     result("Completed", 50, "7289306 - ZYRA NET", "2547****9999 - JOHN"),
	} {
		if ok, _ := r.Matches(l); ok {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSms_FailsOverToHostPinnacleAndNeverLeavesTheMachine(t *testing.T) {
	setupTestDB(t)
	var hpHits int32
	hp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hpHits, 1)
		io.WriteString(w, `{"status":"success"}`)
	}))
	t.Cleanup(hp.Close)
	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500) // primary provider down
	}))
	t.Cleanup(ms.Close)

	org := models.Organization{Name: "ISP", Slug: "isp-sms"}
	config.DB.AutoMigrate(&models.OrganizationSmsConfig{})
	config.DB.Create(&org)
	config.DB.Create(&models.OrganizationSmsConfig{OrganizationID: org.ID, Mode: "own", Provider: "mobilesasa", MobilesasaBaseURL: ms.URL, MobilesasaAPIToken: "isp-token"})
	setSetting(t, "hostpinnacle_base_url", hp.URL)
	setSetting(t, "hostpinnacle_api_key", "k")
	setSetting(t, "hostpinnacle_username", "u")

	logEntry, err := NewSmsService().Send(org.ID, "0712345678", "hello")
	if err != nil || logEntry.Status != "sent" || hpHits != 1 {
		t.Errorf("failover: err=%v status=%v hostpinnacle hits=%d", err, logEntry.Status, hpHits)
	}

	// A real provider URL is refused outright under `go test`.
	config.DB.Model(&models.Setting{}).Where("`key` = ?", "hostpinnacle_base_url").Update("value", "https://smsportal.hostpinnacle.co.ke/SMSApi/send")
	logEntry, err = NewSmsService().Send(0, "0712345678", "hello")
	if err == nil || !strings.Contains(*logEntry.ProviderResponse, "blocked") {
		t.Errorf("real provider reachable from tests: err=%v resp=%v", err, *logEntry.ProviderResponse)
	}
}
