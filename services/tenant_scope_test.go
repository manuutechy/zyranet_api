package services

import (
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

type ispFixture struct {
	org  models.Organization
	zone models.Zone
	pkg  models.Package
}

func newISP(t *testing.T, slug string) ispFixture {
	t.Helper()
	f := ispFixture{org: models.Organization{Name: slug, Slug: slug}}
	config.DB.Create(&f.org)
	f.zone = models.Zone{Name: slug, Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: f.org.ID}
	config.DB.Create(&f.zone)
	f.pkg = models.Package{ZoneID: f.zone.ID, Name: slug + " daily", Type: "hotspot", Price: 50, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "active"}
	config.DB.Create(&f.pkg)
	return f
}

func TestScopedCustomerLookups(t *testing.T) {
	setupTestDB(t)
	a, b := newISP(t, "a"), newISP(t, "b")
	cust := models.Customer{Name: "Jane", Phone: "254711000001", ZoneID: a.zone.ID, PackageID: a.pkg.ID, Type: "hotspot", Status: "active"}
	config.DB.Create(&cust)
	config.DB.Create(&models.CustomerDevice{CustomerID: cust.ID, MacAddress: "AA:BB:CC:00:00:01", LastSeenAt: time.Now()})

	if _, ok := CustomerByPhoneForZone(a.zone.ID, "254711000001"); !ok {
		t.Error("same ISP: phone should match")
	}
	if _, ok := CustomerByPhoneForZone(b.zone.ID, "254711000001"); ok {
		t.Error("another ISP must not match by phone")
	}
	if _, ok := CustomerByDeviceForZone(a.zone.ID, "AA:BB:CC:00:00:01"); !ok {
		t.Error("same ISP: device should match")
	}
	if _, ok := CustomerByDeviceForZone(b.zone.ID, "AA:BB:CC:00:00:01"); ok {
		t.Error("another ISP must not match by device")
	}
	if !CustomerBelongsToZoneISP(cust.ID, a.zone.ID) || CustomerBelongsToZoneISP(cust.ID, b.zone.ID) {
		t.Error("CustomerBelongsToZoneISP is wrong")
	}
}

// Regression: paying on ISP B's WiFi with a phone registered at ISP A used to
// attach the payment to A's customer and move them into B's zone.
func TestPaymentOnAnotherISPDoesNotHijackCustomer(t *testing.T) {
	setupTestDB(t)
	a, b := newISP(t, "a"), newISP(t, "b")
	aCust := models.Customer{Name: "Jane at A", Phone: "254711000001", ZoneID: a.zone.ID, PackageID: a.pkg.ID, Type: "hotspot", Status: "active"}
	config.DB.Create(&aCust)

	checkout := "ws_CO_hijack_test"
	pay := models.Payment{ZoneID: b.zone.ID, PackageID: &b.pkg.ID, Phone: "254711000001", Amount: 50, Method: "mpesa", Status: "pending", MpesaTransactionID: &checkout}
	config.DB.Create(&pay)

	if err := newTestMpesaService().HandleCallback(successCallbackPayload(checkout, 50, "RCP1", "254711000001")); err != nil {
		t.Fatal(err)
	}

	var still models.Customer
	config.DB.First(&still, aCust.ID)
	if still.ZoneID != a.zone.ID || still.PackageID != a.pkg.ID {
		t.Fatalf("ISP A's customer was moved to zone %d / package %d", still.ZoneID, still.PackageID)
	}
	config.DB.First(&pay, pay.ID)
	if pay.CustomerID == nil || *pay.CustomerID == aCust.ID {
		t.Fatalf("payment on ISP B was attached to ISP A's customer (%v)", pay.CustomerID)
	}
	var bCust models.Customer
	config.DB.First(&bCust, *pay.CustomerID)
	if bCust.ZoneID != b.zone.ID || bCust.Status != "active" || bCust.ExpiresAt == nil {
		t.Errorf("ISP B should get its own active customer: %+v", bCust)
	}
}

func TestAccountNumbersStayUniqueAcrossISPs(t *testing.T) {
	setupTestDB(t)
	a, b := newISP(t, "a"), newISP(t, "b")
	mk := func(zone models.Zone, pkg models.Package) models.Customer {
		c := models.Customer{Name: "J", Phone: "254711000001", ZoneID: zone.ID, PackageID: pkg.ID, Type: "hotspot", Status: "active"}
		if err := config.DB.Create(&c).Error; err != nil {
			t.Fatalf("create in zone %d: %v", zone.ID, err)
		}
		return c
	}
	first, second, third := mk(a.zone, a.pkg), mk(b.zone, b.pkg), mk(b.zone, b.pkg)
	if first.AccountNumber != "ZYR#254711000001" {
		t.Errorf("first = %q", first.AccountNumber)
	}
	if second.AccountNumber == first.AccountNumber || third.AccountNumber == second.AccountNumber {
		t.Errorf("collision: %q %q %q", first.AccountNumber, second.AccountNumber, third.AccountNumber)
	}
}
