package services

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func newPayoutSvc() *PayoutService { return NewPayoutService(newTestMpesaService()) }

// seedPlatformOrg makes an ISP with a paybill settlement destination and a zone.
func seedPlatformOrg(t *testing.T, slug string) (models.Organization, uint) {
	t.Helper()
	org := models.Organization{
		Name: slug, Slug: slug, SettlementType: "paybill",
		SettlementPaybillNumber: "247247", SettlementAccountNumber: "0123456789",
	}
	if err := config.DB.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	zone := models.Zone{Name: slug + "-z", Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: org.ID}
	if err := config.DB.Create(&zone).Error; err != nil {
		t.Fatal(err)
	}
	return org, zone.ID
}

func addPayment(t *testing.T, zoneID uint, amount float64, via, status string) models.Payment {
	t.Helper()
	p := models.Payment{ZoneID: zoneID, Phone: "254711000001", Amount: amount, Method: "mpesa", Status: status, CollectedVia: via}
	if err := config.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func claimedBy(t *testing.T, paymentID uint) *uint {
	t.Helper()
	var p models.Payment
	config.DB.First(&p, paymentID)
	return p.PayoutID
}

func TestPrice(t *testing.T) {
	for _, tc := range []struct {
		gross, pct      float64
		commission, net float64
		payout          int64
	}{
		{1000, 0, 0, 1000, 1000},
		{1000, 5, 50, 950, 950},
		{150, 5, 7.5, 142.5, 142},  // floor to whole shillings
		{99.99, 10, 10, 89.99, 89}, // rounds commission to cents first
		{1000, 150, 1000, 0, 0},    // clamped to 100%
		{1000, -5, 0, 1000, 1000},  // clamped to 0%
	} {
		c, n, p := Price(tc.gross, tc.pct)
		if c != tc.commission || n != tc.net || p != tc.payout {
			t.Errorf("Price(%v,%v) = %v,%v,%v want %v,%v,%v", tc.gross, tc.pct, c, n, p, tc.commission, tc.net, tc.payout)
		}
	}
}

func TestCreatePayout_ClaimsExactlyTheOwedPayments(t *testing.T) {
	setupTestDB(t)
	a, zoneA := seedPlatformOrg(t, "a")
	_, zoneB := seedPlatformOrg(t, "b")
	other := models.Payout{OrganizationID: a.ID, Status: "completed"}
	config.DB.Create(&other)

	owed1 := addPayment(t, zoneA, 100, "platform", "completed")
	owed2 := addPayment(t, zoneA, 50, "platform", "completed")
	own := addPayment(t, zoneA, 999, "own", "completed")        // ISP's own shortcode: already theirs
	manual := addPayment(t, zoneA, 999, "", "completed")        // manual / mock / sandbox
	pending := addPayment(t, zoneA, 999, "platform", "pending") // not paid yet
	failed := addPayment(t, zoneA, 999, "platform", "failed")
	otherOrg := addPayment(t, zoneB, 999, "platform", "completed") // another ISP's money
	settled := addPayment(t, zoneA, 999, "platform", "completed")  // already paid out
	config.DB.Model(&settled).Update("payout_id", other.ID)

	p, err := newPayoutSvc().CreatePayout(a.ID, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.GrossAmount != 150 || p.PaymentCount != 2 || p.PayoutAmount != 150 || p.Status != "pending" {
		t.Errorf("payout = gross %v count %d pay %d status %s", p.GrossAmount, p.PaymentCount, p.PayoutAmount, p.Status)
	}
	for _, id := range []uint{owed1.ID, owed2.ID} {
		if got := claimedBy(t, id); got == nil || *got != p.ID {
			t.Errorf("payment %d should be claimed by payout %d, got %v", id, p.ID, got)
		}
	}
	for name, pay := range map[string]models.Payment{"own": own, "manual": manual, "pending": pending, "failed": failed, "other org": otherOrg} {
		if got := claimedBy(t, pay.ID); got != nil {
			t.Errorf("%s payment must not be claimed, but is claimed by %d", name, *got)
		}
	}
	if got := claimedBy(t, settled.ID); got == nil || *got != other.ID {
		t.Error("an already-paid payment must stay with its original payout")
	}
	if p.DestType != "paybill" || p.DestPaybill != "247247" || p.DestAccount != "0123456789" {
		t.Errorf("destination not snapshotted: %+v", p)
	}
}

func TestCreatePayout_Commission(t *testing.T) {
	setupTestDB(t)
	org, zone := seedPlatformOrg(t, "a")
	addPayment(t, zone, 150, "platform", "completed")

	p, err := newPayoutSvc().CreatePayout(org.ID, 1, 10, 0) // platform default 10%
	if err != nil {
		t.Fatal(err)
	}
	if p.CommissionPercent != 10 || p.CommissionAmount != 15 || p.NetAmount != 135 || p.PayoutAmount != 135 {
		t.Errorf("default commission: %+v", p)
	}
	if err := newPayoutSvc().Cancel(p.ID); err != nil {
		t.Fatal(err)
	}

	five := 5.0
	config.DB.Model(&org).Update("commission_percent", five) // ISP override beats the default
	p, err = newPayoutSvc().CreatePayout(org.ID, 1, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.CommissionPercent != 5 || p.CommissionAmount != 7.5 || p.NetAmount != 142.5 || p.PayoutAmount != 142 {
		t.Errorf("override commission: %+v", p)
	}
}

func TestCreatePayout_Guards(t *testing.T) {
	setupTestDB(t)
	svc := newPayoutSvc()
	org, zone := seedPlatformOrg(t, "a")

	if _, err := svc.CreatePayout(org.ID, 1, 0, 0); !errors.Is(err, ErrPayoutNothingOwed) {
		t.Errorf("nothing owed: %v", err)
	}
	var n int64
	config.DB.Model(&models.Payout{}).Count(&n)
	if n != 0 {
		t.Errorf("a failed create left %d payout row(s) behind", n)
	}

	pay := addPayment(t, zone, 40, "platform", "completed")
	if _, err := svc.CreatePayout(org.ID, 1, 0, 100); !errors.Is(err, ErrPayoutBelowMinimum) {
		t.Errorf("below minimum: %v", err)
	}
	if claimedBy(t, pay.ID) != nil {
		t.Error("a payout rejected for being too small must leave its payments unclaimed")
	}

	p, err := svc.CreatePayout(org.ID, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreatePayout(org.ID, 1, 0, 0); !errors.Is(err, ErrPayoutOpen) {
		t.Errorf("second open payout: %v", err)
	}
	// Cancelling releases the money back to the ledger for the next payout.
	if err := svc.Cancel(p.ID); err != nil {
		t.Fatal(err)
	}
	if claimedBy(t, pay.ID) != nil {
		t.Error("cancelled payout should release its payments")
	}
	if _, err := svc.CreatePayout(org.ID, 1, 0, 0); err != nil {
		t.Errorf("after cancel: %v", err)
	}

	config.DB.Model(&org).Updates(map[string]interface{}{"settlement_paybill_number": ""})
	if _, err := svc.CreatePayout(org.ID, 1, 0, 0); !errors.Is(err, ErrPayoutNoDestination) {
		t.Errorf("no destination: %v", err)
	}
}

func TestPayoutTransitions_AreGuarded(t *testing.T) {
	setupTestDB(t)
	svc := newPayoutSvc()
	org, zone := seedPlatformOrg(t, "a")
	pay := addPayment(t, zone, 100, "platform", "completed")
	p, _ := svc.CreatePayout(org.ID, 7, 0, 0)

	if err := svc.Fail(p.ID, "x"); !errors.Is(err, ErrPayoutNotInState) {
		t.Errorf("failing a pending payout: %v", err)
	}
	if err := svc.MarkPaid(p.ID, 7, "  "); err == nil {
		t.Error("marking paid without a reference must be refused")
	}
	if err := svc.MarkPaid(p.ID, 7, "QWE123"); err != nil {
		t.Fatal(err)
	}
	var got models.Payout
	config.DB.First(&got, p.ID)
	if got.Status != "completed" || got.Reference != "QWE123" || got.Method != "manual" || got.CompletedAt == nil {
		t.Errorf("after MarkPaid: %+v", got)
	}
	// Completed is final: no double completion, no cancel, and the money stays claimed.
	if err := svc.MarkPaid(p.ID, 7, "OTHER"); !errors.Is(err, ErrPayoutNotInState) {
		t.Errorf("second MarkPaid: %v", err)
	}
	if err := svc.Cancel(p.ID); !errors.Is(err, ErrPayoutNotInState) {
		t.Errorf("cancelling a completed payout: %v", err)
	}
	if claimedBy(t, pay.ID) == nil {
		t.Error("a completed payout's payments must stay claimed")
	}
}

// ---- Daraja pieces ----

func testCert(t *testing.T) (certPEM string, key *rsa.PrivateKey) {
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
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), key
}

func TestSecurityCredential_RoundTrip(t *testing.T) {
	certPEM, key := testCert(t)
	enc, err := SecurityCredential("s3cret-Init!", certPEM)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, key, raw)
	if err != nil || string(plain) != "s3cret-Init!" {
		t.Errorf("decrypted %q, %v", plain, err)
	}
	if _, err := SecurityCredential("x", "not a cert"); err == nil {
		t.Error("garbage certificate must be rejected")
	}
}

func TestB2BPayload(t *testing.T) {
	bank := &models.Payout{ID: 9, PayoutAmount: 1234, DestType: "paybill", DestPaybill: "247247", DestAccount: "0123456789"}
	p := b2bPayload(bank, "init", "cred", "5662552", "https://r", "https://t")
	if p["CommandID"] != "BusinessPayBill" || p["PartyB"] != "247247" || p["AccountReference"] != "0123456789" ||
		p["Amount"] != "1234" || p["PartyA"] != "5662552" || p["SenderIdentifierType"] != "4" || p["RecieverIdentifierType"] != "4" {
		t.Errorf("paybill payload: %v", p)
	}
	till := &models.Payout{ID: 10, PayoutAmount: 50, DestType: "till", DestTill: "523456"}
	p = b2bPayload(till, "init", "cred", "5662552", "https://r", "https://t")
	if p["CommandID"] != "BusinessBuyGoods" || p["PartyB"] != "523456" {
		t.Errorf("till payload: %v", p)
	}
	if _, has := p["AccountReference"]; has {
		t.Error("a till payout has no account reference")
	}
}

func TestClassifyB2BResponse(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   b2bOutcome
	}{
		"accepted":            {200, `{"OriginatorConversationID":"o1","ConversationID":"c1","ResponseCode":"0","ResponseDescription":"Accept the service request successfully."}`, b2bAccepted},
		"accepted, no id":     {200, `{"ResponseCode":"0"}`, b2bUnknown},
		"200 with error code": {200, `{"ResponseCode":"1","ResponseDescription":"insufficient"}`, b2bRejected},
		"400 bad credentials": {400, `{"errorCode":"400.002.02","errorMessage":"Bad Request - Invalid Initiator"}`, b2bRejected},
		"401 auth":            {401, `{"errorMessage":"Invalid Access Token"}`, b2bRejected},
		"500 gateway":         {500, `{"errorMessage":"boom"}`, b2bUnknown},
		"502 html":            {502, `<html>bad gateway</html>`, b2bUnknown},
		"200 unparsable":      {200, `<html>`, b2bUnknown},
	} {
		if got, _, _, _ := classifyB2BResponse(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: outcome %d, want %d", name, got, tc.want)
		}
	}
}

func resultPayload(orig string, code float64, desc, txid string) map[string]interface{} {
	return map[string]interface{}{"Result": map[string]interface{}{
		"ResultType": float64(0), "ResultCode": code, "ResultDesc": desc,
		"OriginatorConversationID": orig, "ConversationID": "conv-1", "TransactionID": txid,
	}}
}

func processingPayout(t *testing.T, svc *PayoutService, orgID uint, orig string) *models.Payout {
	t.Helper()
	p, err := svc.CreatePayout(orgID, 1, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	config.DB.Model(p).Updates(map[string]interface{}{"status": "processing", "method": "b2b", "originator_conversation_id": orig})
	return p
}

func TestHandleB2BResult_SuccessIsIdempotent(t *testing.T) {
	setupTestDB(t)
	svc := newPayoutSvc()
	org, zone := seedPlatformOrg(t, "a")
	pay := addPayment(t, zone, 100, "platform", "completed")
	p := processingPayout(t, svc, org.ID, "orig-1")

	for i := 0; i < 2; i++ { // Safaricom may deliver a callback twice
		if err := svc.HandleB2BResult(resultPayload("orig-1", 0, "The service request is processed successfully.", "RKT1234")); err != nil {
			t.Fatal(err)
		}
	}
	var got models.Payout
	config.DB.First(&got, p.ID)
	if got.Status != "completed" || got.Reference != "RKT1234" || got.CompletedAt == nil {
		t.Errorf("after result: %+v", got)
	}
	if claimedBy(t, pay.ID) == nil {
		t.Error("a paid payout must keep its payments claimed")
	}
	// A late *failure* for an already-completed payout must not undo it.
	svc.HandleB2BResult(resultPayload("orig-1", 2001, "late failure", ""))
	config.DB.First(&got, p.ID)
	if got.Status != "completed" || claimedBy(t, pay.ID) == nil {
		t.Error("a completed payout must not be reversed by a later callback")
	}
}

func TestHandleB2BResult_FailureReleasesThePayments(t *testing.T) {
	setupTestDB(t)
	svc := newPayoutSvc()
	org, zone := seedPlatformOrg(t, "a")
	pay := addPayment(t, zone, 100, "platform", "completed")
	p := processingPayout(t, svc, org.ID, "orig-2")

	if err := svc.HandleB2BResult(resultPayload("orig-2", 2001, "The initiator information is invalid.", "")); err != nil {
		t.Fatal(err)
	}
	var got models.Payout
	config.DB.First(&got, p.ID)
	if got.Status != "failed" || got.FailureReason == "" {
		t.Errorf("after failure: %+v", got)
	}
	if claimedBy(t, pay.ID) != nil {
		t.Error("a failed payout must release its payments so they're paid next time")
	}
	if _, err := svc.CreatePayout(org.ID, 1, 0, 0); err != nil {
		t.Errorf("a new payout should be possible after a failed one: %v", err)
	}
	if err := svc.HandleB2BResult(resultPayload("no-such", 0, "", "X")); err == nil {
		t.Error("a result for an unknown conversation must be an error")
	}
}

// ---- end to end against a fake Daraja ----

type fakeDaraja struct {
	*httptest.Server
	hits    atomic.Int32
	status  int
	reply   string
	lastReq map[string]interface{}
}

func newFakeDaraja(t *testing.T, status int, reply string) *fakeDaraja {
	f := &fakeDaraja{status: status, reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v1/generate":
			io.WriteString(w, `{"access_token":"tok","expires_in":"3599"}`)
		case "/mpesa/b2b/v1/paymentrequest":
			f.hits.Add(1)
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &f.lastReq)
			w.WriteHeader(f.status)
			io.WriteString(w, f.reply)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

const acceptedReply = `{"OriginatorConversationID":"orig-e2e","ConversationID":"conv-e2e","ResponseCode":"0","ResponseDescription":"Accept the service request successfully."}`

func setupB2B(t *testing.T, f *fakeDaraja) (*PayoutService, models.Organization, models.Payment) {
	t.Helper()
	setupTestDB(t)
	certPEM, _ := testCert(t)
	for k, v := range map[string]string{
		"mpesa_consumer_key": "ck", "mpesa_consumer_secret": "cs", "mpesa_shortcode": "5662552",
		"mpesa_environment": "production", "mpesa_callback_url": "https://api.example.com/api/v1/mpesa/callback",
		"mpesa_b2b_initiator": "apiop", "mpesa_b2b_password": "pw", "mpesa_b2b_cert": certPEM,
	} {
		setSetting(t, k, v)
	}
	oldSecret := config.Config.MpesaCallbackSecret
	config.Config.MpesaCallbackSecret = "test-callback-secret"
	t.Cleanup(func() { config.Config.MpesaCallbackSecret = oldSecret })
	svc := newPayoutSvc()
	svc.Mpesa.baseURLOverride = f.URL
	org, zone := seedPlatformOrg(t, "a")
	pay := addPayment(t, zone, 100, "platform", "completed")
	return svc, org, pay
}

func TestSendB2B_AcceptedThenNeverSentTwice(t *testing.T) {
	f := newFakeDaraja(t, 200, acceptedReply)
	svc, org, _ := setupB2B(t, f)
	p, _ := svc.CreatePayout(org.ID, 1, 0, 0)

	got, err := svc.SendB2B(p.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "processing" || got.Method != "b2b" || got.OriginatorConversationID != "orig-e2e" || got.SentByPlatformUserID == nil || *got.SentByPlatformUserID != 42 {
		t.Errorf("after send: %+v", got)
	}
	if f.lastReq["CommandID"] != "BusinessPayBill" || f.lastReq["PartyB"] != "247247" || f.lastReq["Amount"] != "100" || f.lastReq["Initiator"] != "apiop" {
		t.Errorf("request sent to Daraja: %v", f.lastReq)
	}
	if cred, _ := f.lastReq["SecurityCredential"].(string); cred == "" || cred == "pw" {
		t.Error("the initiator password must be sent encrypted")
	}

	// A double-click / retry must not send money again.
	if _, err := svc.SendB2B(p.ID, 42); !errors.Is(err, ErrPayoutNotInState) {
		t.Errorf("second send: %v, want ErrPayoutNotInState", err)
	}
	if n := f.hits.Load(); n != 1 {
		t.Errorf("Daraja received %d payout requests, want exactly 1", n)
	}
}

func TestSendB2B_RejectedGoesBackToPending(t *testing.T) {
	f := newFakeDaraja(t, 400, `{"errorCode":"400.002.02","errorMessage":"Invalid Initiator"}`)
	svc, org, pay := setupB2B(t, f)
	p, _ := svc.CreatePayout(org.ID, 1, 0, 0)

	got, err := svc.SendB2B(p.ID, 1)
	if err == nil {
		t.Fatal("expected an error for a rejected payout")
	}
	if got.Status != "pending" || got.FailureReason == "" || got.Method != "" {
		t.Errorf("rejected payout should be pending again with a reason: %+v", got)
	}
	if claimedBy(t, pay.ID) == nil {
		t.Error("the payments stay claimed by the pending payout so it can be retried")
	}
}

func TestSendB2B_UnknownOutcomeIsNotRetried(t *testing.T) {
	f := newFakeDaraja(t, 500, `{"errorMessage":"upstream exploded"}`)
	svc, org, _ := setupB2B(t, f)
	p, _ := svc.CreatePayout(org.ID, 1, 0, 0)

	got, err := svc.SendB2B(p.ID, 1)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got.Status != "processing" || got.FailureReason == "" {
		t.Errorf("an unclear reply must leave the payout processing for a human to check: %+v", got)
	}
	if _, err := svc.SendB2B(p.ID, 1); !errors.Is(err, ErrPayoutNotInState) {
		t.Errorf("must not be re-sendable: %v", err)
	}
	if n := f.hits.Load(); n != 1 {
		t.Errorf("Daraja received %d requests, want 1", n)
	}
	// Staff confirm from the statement that it did not go through: money returns to the ledger.
	if err := svc.Fail(p.ID, "not on statement"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreatePayout(org.ID, 1, 0, 0); err != nil {
		t.Errorf("after Fail the ISP should be payable again: %v", err)
	}
}

func TestSendB2B_NotConfiguredSendsNothing(t *testing.T) {
	f := newFakeDaraja(t, 200, acceptedReply)
	svc, org, _ := setupB2B(t, f)
	config.DB.Where("`key` = ?", "mpesa_b2b_password").Delete(&models.Setting{})
	p, _ := svc.CreatePayout(org.ID, 1, 0, 0)

	got, err := svc.SendB2B(p.ID, 1)
	if !errors.Is(err, ErrPayoutB2BNotConfigured) || got.Status != "pending" {
		t.Errorf("err=%v status=%s, want not-configured and still pending", err, got.Status)
	}
	if f.hits.Load() != 0 {
		t.Error("nothing may be sent when B2B isn't configured")
	}
}

func TestCollectionChannel_OnlyRealSharedShortcodeMoneyIsPayable(t *testing.T) {
	setupTestDB(t)
	svc := newTestMpesaService()
	zone, _ := seedZone(t)
	ownZone := seedOwnOrgZone(t, models.OrganizationMpesaConfig{ConsumerKey: "k", ConsumerSecret: "s", Passkey: "p", Shortcode: "111222", Env: "production"})

	// Shared shortcode on sandbox: not real money.
	if got := svc.CollectionChannel(zone, "RCP123"); got != "" {
		t.Errorf("sandbox platform payment = %q, want \"\"", got)
	}
	if got := svc.CollectionChannel(ownZone, "RCP123"); got != "own" {
		t.Errorf("own-Daraja payment = %q, want own", got)
	}

	setSetting(t, "mpesa_environment", "production")
	InvalidateMpesaCaches()
	if got := svc.CollectionChannel(zone, "RCP123"); got != "platform" {
		t.Errorf("production shared-shortcode payment = %q, want platform", got)
	}
	if got := svc.CollectionChannel(zone, "MOCKAB12"); got != "" {
		t.Errorf("mock receipt = %q — fake payments must never become payable", got)
	}
	if got := svc.CollectionChannel(zone, "QRY_ws_CO_123"); got != "platform" {
		t.Errorf("receipt recovered via the status query = %q, want platform", got)
	}
}

func TestSendB2B_RefusesWithoutACallbackSecret(t *testing.T) {
	f := newFakeDaraja(t, 200, acceptedReply)
	svc, org, _ := setupB2B(t, f)
	config.Config.MpesaCallbackSecret = "" // as on a server that never set one
	p, _ := svc.CreatePayout(org.ID, 1, 0, 0)

	got, err := svc.SendB2B(p.ID, 1)
	if !errors.Is(err, ErrPayoutCallbackSecretMissing) || got.Status != "pending" {
		t.Errorf("err=%v status=%s, want the secret-missing error and still pending", err, got.Status)
	}
	if f.hits.Load() != 0 {
		t.Error("nothing may be sent when the result callback can't be authenticated")
	}
}
