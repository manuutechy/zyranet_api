package services

import (
	"errors"
	"strings"
	"testing"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func setAppEnv(t *testing.T, env string) {
	t.Helper()
	old := config.Config.AppEnv
	config.Config.AppEnv = env
	t.Cleanup(func() { config.Config.AppEnv = old })
}

func setSetting(t *testing.T, key, val string) {
	t.Helper()
	v := val
	if err := config.DB.Create(&models.Setting{Key: key, Value: &v}).Error; err != nil {
		t.Fatalf("seed setting %s: %v", key, err)
	}
}

// seedOwnOrgZone makes an org in "own" mode with the given config and a zone in it.
func seedOwnOrgZone(t *testing.T, cfg models.OrganizationMpesaConfig) uint {
	t.Helper()
	org := models.Organization{Name: "ISP", Slug: "isp-" + t.Name()}
	if err := config.DB.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	cfg.OrganizationID = org.ID
	cfg.Mode = "own"
	if err := config.DB.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}
	zone := models.Zone{Name: "Z", Location: "L", RouterName: "r", RouterIP: "127.0.0.1", OrganizationID: org.ID}
	if err := config.DB.Create(&zone).Error; err != nil {
		t.Fatal(err)
	}
	return zone.ID
}

func TestResolveMpesaCreds_OwnModeNeverBorrowsPlatformCredentials(t *testing.T) {
	setupTestDB(t)
	setSetting(t, "mpesa_consumer_key", "PLATFORM_KEY")
	setSetting(t, "mpesa_consumer_secret", "PLATFORM_SECRET")
	setSetting(t, "mpesa_shortcode", "5662552")
	setSetting(t, "mpesa_passkey", "PLATFORM_PASSKEY")
	setSetting(t, "mpesa_till_number", "PLATFORM_TILL")
	setSetting(t, "mpesa_environment", "production")
	setSetting(t, "mpesa_callback_url", "https://api.zyranet.co.ke/api/v1/mpesa/callback")

	// ISP filled in a key and shortcode but left passkey/secret/till blank.
	zoneID := seedOwnOrgZone(t, models.OrganizationMpesaConfig{
		ConsumerKey: "ISP_KEY", Shortcode: "111222", BillingType: "till",
	})

	creds := newTestMpesaService().ResolveMpesaCreds(zoneID)

	if !creds.Own {
		t.Fatal("expected own creds")
	}
	if creds.ConsumerKey != "ISP_KEY" || creds.Shortcode != "111222" {
		t.Errorf("own values lost: key=%q shortcode=%q", creds.ConsumerKey, creds.Shortcode)
	}
	if creds.Passkey != "" || creds.ConsumerSecret != "" || creds.TillNumber != "" {
		t.Errorf("platform values leaked into own creds: passkey=%q secret=%q till=%q", creds.Passkey, creds.ConsumerSecret, creds.TillNumber)
	}
	if creds.Env != "sandbox" {
		t.Errorf("blank own env must default to sandbox, not the platform's %q", creds.Env)
	}
	if creds.CallbackURL != "https://api.zyranet.co.ke/api/v1/mpesa/callback" {
		t.Errorf("callback URL should default to Zyra's endpoint, got %q", creds.CallbackURL)
	}
	want := []string{"consumer secret", "passkey", "till number"}
	if got := creds.missingFields(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("missingFields = %v, want %v", got, want)
	}
}

func TestResolveMpesaCreds_PlatformModeUnchanged(t *testing.T) {
	setupTestDB(t)
	setSetting(t, "mpesa_shortcode", "5662552")
	setSetting(t, "mpesa_passkey", "PLATFORM_PASSKEY")
	zoneID, _ := seedZone(t) // org 0, no config row

	creds := newTestMpesaService().ResolveMpesaCreds(zoneID)
	if creds.Own || creds.Shortcode != "5662552" || creds.Passkey != "PLATFORM_PASSKEY" {
		t.Errorf("unexpected platform creds: %+v", creds)
	}
}

func TestNormalizeBillingType_LegacyBankCollectsAsPaybill(t *testing.T) {
	for in, want := range map[string]string{"till": "till", "TILL": "till", "paybill": "paybill", "bank": "paybill", "": "paybill", "junk": "paybill"} {
		if got := normalizeBillingType(in); got != want {
			t.Errorf("normalizeBillingType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoute_PaybillSettlesToTheSigningShortcode(t *testing.T) {
	// PaybillNumber differs from Shortcode: Daraja needs PartyB == the shortcode
	// the request is signed with, so the mismatched number must be ignored.
	c := mpesaCreds{BillingType: "paybill", Shortcode: "5662552", PaybillNumber: "999999"}
	r := c.route()
	if r.TransactionType != "CustomerPayBillOnline" || r.PartyB != "5662552" {
		t.Errorf("paybill route = %+v", r)
	}
}

func TestRoute_TillUsesBuyGoodsWithTillAsPartyB(t *testing.T) {
	c := mpesaCreds{BillingType: "till", Shortcode: "174379", TillNumber: "523456"}
	r := c.route()
	if r.TransactionType != "CustomerBuyGoodsOnline" || r.PartyB != "523456" {
		t.Errorf("till route = %+v", r)
	}
}

func TestC2BShortcode(t *testing.T) {
	if got := (mpesaCreds{BillingType: "till", Shortcode: "174379", TillNumber: "523456"}).c2bShortcode(); got != "523456" {
		t.Errorf("till c2b shortcode = %q", got)
	}
	if got := (mpesaCreds{BillingType: "paybill", Shortcode: "5662552", PaybillNumber: "999999"}).c2bShortcode(); got != "5662552" {
		t.Errorf("paybill c2b shortcode = %q", got)
	}
}

func TestInitiateSTKPush_IncompleteOwnConfigIsRefusedNotMocked(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	zoneID := seedOwnOrgZone(t, models.OrganizationMpesaConfig{ConsumerKey: "ISP_KEY", Shortcode: "111222"})

	resp, err := newTestMpesaService().InitiateSTKPush(zoneID, "254712345678", 30, "Cust1", "WiFi")
	if resp != nil {
		t.Fatalf("must not return a (mock) success response, got %+v", resp)
	}
	if !errors.Is(err, errPaymentsUnavailable) {
		t.Fatalf("expected errPaymentsUnavailable, got %v", err)
	}
}

func TestGetPaybillNumber_OwnModeNeverShowsZyrasPaybill(t *testing.T) {
	setupTestDB(t)
	zoneID := seedOwnOrgZone(t, models.OrganizationMpesaConfig{ConsumerKey: "ISP_KEY"})
	if got := newTestMpesaService().GetPaybillNumber(zoneID); got != "" {
		t.Errorf("own-mode ISP with no shortcode got %q, want empty", got)
	}

	zoneID2, _ := seedZone(t)
	if got := newTestMpesaService().GetPaybillNumber(zoneID2); got != "7289306" {
		t.Errorf("platform default = %q, want 7289306", got)
	}
}

func TestGetPaymentInfo(t *testing.T) {
	setupTestDB(t)
	zoneID := seedOwnOrgZone(t, models.OrganizationMpesaConfig{
		ConsumerKey: "k", ConsumerSecret: "s", Passkey: "p", Shortcode: "174379", BillingType: "till", TillNumber: "523456",
	})
	bt, paybill, till := newTestMpesaService().GetPaymentInfo(zoneID)
	if bt != "till" || till != "523456" || paybill != "174379" {
		t.Errorf("info = %q paybill=%q till=%q", bt, paybill, till)
	}
}

func TestOrgForShortcode(t *testing.T) {
	setupTestDB(t)
	seedOwnOrgZone(t, models.OrganizationMpesaConfig{Shortcode: "111222", TillNumber: "523456"})

	if _, ok := OrgForShortcode("111222"); !ok {
		t.Error("shortcode should map to the ISP")
	}
	if _, ok := OrgForShortcode("523456"); !ok {
		t.Error("till should map to the ISP")
	}
	if _, ok := OrgForShortcode("5662552"); ok {
		t.Error("the shared platform paybill must not map to any ISP")
	}
	if _, ok := OrgForShortcode(""); ok {
		t.Error("blank shortcode must not map to any ISP")
	}
}

func TestRegisterC2BURLs_IncompleteOwnConfigIsRefused(t *testing.T) {
	setupTestDB(t)
	setAppEnv(t, "production")
	zoneID := seedOwnOrgZone(t, models.OrganizationMpesaConfig{ConsumerKey: "ISP_KEY"})
	if _, err := newTestMpesaService().RegisterC2BURLs(zoneID, "https://x.example/c", "https://x.example/v", ""); err == nil {
		t.Fatal("expected an error for incomplete own config")
	}
}

func TestCredsCache_ServesCachedUntilInvalidated(t *testing.T) {
	setupTestDB(t)
	setSetting(t, "mpesa_shortcode", "111111")
	svc := newTestMpesaService()
	zoneID, _ := seedZone(t)

	if got := svc.ResolveMpesaCreds(zoneID).Shortcode; got != "111111" {
		t.Fatalf("shortcode = %q", got)
	}
	// Change the setting behind the cache's back: still the cached value...
	config.DB.Model(&models.Setting{}).Where("`key` = ?", "mpesa_shortcode").Update("value", "222222")
	if got := svc.ResolveMpesaCreds(zoneID).Shortcode; got != "111111" {
		t.Errorf("expected the cached shortcode, got %q", got)
	}
	// ...until a save path invalidates it.
	InvalidateMpesaCaches()
	if got := svc.ResolveMpesaCreds(zoneID).Shortcode; got != "222222" {
		t.Errorf("after invalidation shortcode = %q, want 222222", got)
	}
}
