package handlers

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"gorm.io/gorm"
)

const platformPaybill = "5662552"

func setupC2BTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&models.Organization{}, &models.OrganizationMpesaConfig{}, &models.Zone{}, &models.Package{},
		&models.Customer{}, &models.CustomerDevice{}, &models.Payment{}, &models.CreditLog{},
		&models.UnmatchedC2BPayment{}, &models.Setting{}, &models.User{}, &models.Payout{}, &models.PlatformSetting{}, &models.RadiusAccount{}, &models.SmsLog{}, &models.AuditLog{},
	); err != nil {
		t.Fatal(err)
	}
	config.DB = db
	invalidateAllSettings()
	services.InvalidateMpesaCaches()
	old := config.Config.MpesaCallbackSecret
	config.Config.MpesaCallbackSecret = ""
	t.Cleanup(func() { config.Config.MpesaCallbackSecret = old })
}

type isp struct {
	OrgID  uint
	ZoneID uint
}

// seedISP creates an org with n zones (each with one KES 100 package). own
// gives the org its own Daraja shortcode.
func seedISP(t *testing.T, slug, ownShortcode string, zones int) isp {
	t.Helper()
	org := models.Organization{Name: slug, Slug: slug}
	if err := config.DB.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	if ownShortcode != "" {
		if err := config.DB.Create(&models.OrganizationMpesaConfig{OrganizationID: org.ID, Mode: "own", Shortcode: ownShortcode}).Error; err != nil {
			t.Fatal(err)
		}
	}
	var first uint
	for i := 0; i < zones; i++ {
		z := models.Zone{Name: fmt.Sprintf("%s-z%d", slug, i), Location: "L", RouterName: "r", RouterIP: "127.0.0.1", OrganizationID: org.ID}
		if err := config.DB.Create(&z).Error; err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = z.ID
		}
		p := models.Package{ZoneID: z.ID, Name: "Daily", Type: "hotspot", Price: 100, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "active"}
		if err := config.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	return isp{OrgID: org.ID, ZoneID: first}
}

func seedCustomer(t *testing.T, zoneID uint, name, phone, account string) models.Customer {
	t.Helper()
	var pkg models.Package
	config.DB.Where("zone_id = ?", zoneID).First(&pkg)
	c := models.Customer{Name: name, Phone: phone, ZoneID: zoneID, PackageID: pkg.ID, Type: "hotspot", Status: "active", AccountNumber: account}
	if err := config.DB.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	return c
}

// postC2B delivers a C2B confirmation the way Safaricom does. Amounts stay
// below the KES 100 package price so the credit is left on the account
// balance and easy to assert on.
func postC2B(t *testing.T, shortcode, transID, billRef, msisdn string, amount float64) {
	t.Helper()
	app := fiber.New()
	app.Post("/c2b", MpesaC2BConfirmation)
	body := fmt.Sprintf(`{"TransactionType":"Pay Bill","TransID":%q,"TransTime":"20260921120000","TransAmount":"%.2f","BusinessShortCode":%q,"BillRefNumber":%q,"MSISDN":%q,"FirstName":"JANE","LastName":"DOE"}`,
		transID, amount, shortcode, billRef, msisdn)
	req := httptest.NewRequest("POST", "/c2b", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func balance(t *testing.T, id uint) float64 {
	t.Helper()
	var c models.Customer
	if err := config.DB.First(&c, id).Error; err != nil {
		t.Fatal(err)
	}
	return c.CreditBalance
}

func count(t *testing.T, model interface{}) int64 {
	t.Helper()
	var n int64
	config.DB.Model(model).Count(&n)
	return n
}

func TestC2B_SharedPaybill_MatchesUniqueAccountNumber(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	seedCustomer(t, a.ZoneID, "A1", "254711000001", "ZYR#A1")
	target := seedCustomer(t, b.ZoneID, "B1", "254722000002", "ZYR#B1")

	postC2B(t, platformPaybill, "TX1", "ZYR#B1", "254722000002", 30)

	if got := balance(t, target.ID); got != 30 {
		t.Errorf("ISP B's customer balance = %v, want 30", got)
	}
	var p models.Payment
	config.DB.Where("mpesa_receipt_number = ?", "TX1").First(&p)
	if p.ZoneID != b.ZoneID {
		t.Errorf("payment recorded against zone %d, want ISP B's zone %d", p.ZoneID, b.ZoneID)
	}
}

func TestC2B_ShortAccountRefDoesNotMatchArbitraryCustomerByPhoneWildcard(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "A1", "254711000007", "ZYR#A1")

	// The old query was `phone LIKE %<BillRefNumber>`: "7" matched this customer.
	postC2B(t, platformPaybill, "TX2", "7", "hashedmsisdnabc", 30)

	if got := balance(t, c.ID); got != 0 {
		t.Errorf("customer wrongly credited %v via wildcard phone match", got)
	}
	if count(t, &models.UnmatchedC2BPayment{}) != 1 {
		t.Error("payment should be queued as unmatched")
	}
}

func TestC2B_SharedPaybill_SamePhoneInTwoISPsIsAmbiguousNotGuessed(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	ca := seedCustomer(t, a.ZoneID, "A1", "254733000003", "ZYR#A1")
	cb := seedCustomer(t, b.ZoneID, "B1", "254733000003", "ZYR#B1")

	postC2B(t, platformPaybill, "TX3", "0733000003", "254733000003", 30)

	if balance(t, ca.ID) != 0 || balance(t, cb.ID) != 0 {
		t.Error("an ambiguous payment must not be credited to either ISP")
	}
	if count(t, &models.UnmatchedC2BPayment{}) != 1 {
		t.Error("ambiguous payment should be queued for manual reconciliation")
	}
}

func TestC2B_SharedPaybill_UnknownPayerIsQueuedNotFiledUnderFirstZone(t *testing.T) {
	setupC2BTestDB(t)
	seedISP(t, "a", "", 1)
	seedISP(t, "b", "", 1)

	postC2B(t, platformPaybill, "TX4", "GHOST", "254799999999", 30)

	if n := count(t, &models.Customer{}); n != 0 {
		t.Errorf("%d customer(s) auto-created under an arbitrary ISP", n)
	}
	if count(t, &models.Payment{}) != 0 {
		t.Error("no payment should be recorded for an unattributable payer")
	}
	if count(t, &models.UnmatchedC2BPayment{}) != 1 {
		t.Error("payment should be queued as unmatched")
	}
}

func TestC2B_OwnShortcode_OnlyThatISPsCustomersMatch(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "222333", 2) // own shortcode, two zones
	other := seedCustomer(t, a.ZoneID, "A1", "254711000001", "ZYR#A1")

	// Paid to ISP B's own shortcode but quoting ISP A's account number.
	postC2B(t, "222333", "TX5", "ZYR#A1", "254711000001", 30)

	if got := balance(t, other.ID); got != 0 {
		t.Errorf("ISP A's customer credited %v for a payment to ISP B's shortcode", got)
	}
	if count(t, &models.UnmatchedC2BPayment{}) != 1 {
		t.Error("payment should be queued as unmatched")
	}
	_ = b
}

func TestC2B_OwnShortcode_SingleZoneISPAutoRegistersNewPayer(t *testing.T) {
	setupC2BTestDB(t)
	b := seedISP(t, "b", "222333", 1)

	postC2B(t, "222333", "TX6", "NEWBIE", "254744000004", 30)

	var c models.Customer
	if err := config.DB.Where("phone = ?", "254744000004").First(&c).Error; err != nil {
		t.Fatalf("payer was not registered: %v", err)
	}
	if c.ZoneID != b.ZoneID {
		t.Errorf("customer created in zone %d, want ISP B's zone %d", c.ZoneID, b.ZoneID)
	}
	if c.CreditBalance != 30 {
		t.Errorf("balance = %v, want 30", c.CreditBalance)
	}
}

func TestC2B_OwnShortcode_MultiZoneISPQueuesNewPayer(t *testing.T) {
	setupC2BTestDB(t)
	seedISP(t, "b", "222333", 2)

	postC2B(t, "222333", "TX7", "NEWBIE", "254744000004", 30)

	if count(t, &models.Customer{}) != 0 {
		t.Error("with several zones the right one can't be inferred — must not auto-create")
	}
	if count(t, &models.UnmatchedC2BPayment{}) != 1 {
		t.Error("payment should be queued as unmatched")
	}
}

func TestC2B_DuplicateDeliveryCreditsOnce(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "a", "", 1)
	c := seedCustomer(t, a.ZoneID, "A1", "254711000001", "ZYR#A1")

	postC2B(t, platformPaybill, "TX8", "ZYR#A1", "254711000001", 30)
	postC2B(t, platformPaybill, "TX8", "ZYR#A1", "254711000001", 30) // Safaricom retry

	if got := balance(t, c.ID); got != 30 {
		t.Errorf("balance = %v after a retried confirmation, want 30", got)
	}
	if n := count(t, &models.Payment{}); n != 1 {
		t.Errorf("%d payment rows, want 1", n)
	}
}

func TestPhoneSuffix9(t *testing.T) {
	for in, want := range map[string]string{
		"254712345678":         "712345678",
		"0712345678":           "712345678",
		"+254 712 345678":      "712345678",
		"7":                    "",
		"12345678":             "",
		"ZYR#254712345678":     "",
		"a1b2c3d4e5f6a7b8c9d0": "",
	} {
		if got := phoneSuffix9(in); got != want {
			t.Errorf("phoneSuffix9(%q) = %q, want %q", in, got, want)
		}
	}
}
