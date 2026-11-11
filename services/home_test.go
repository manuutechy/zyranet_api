package services

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// fakeHomeRouter records switch-offs instead of dialling a router.
type fakeHomeRouter struct {
	mu    sync.Mutex
	users []string
}

func (f *fakeHomeRouter) DecommissionPPPoECustomer(_ *models.Zone, username string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = append(f.users, username)
	return nil
}
func (f *fakeHomeRouter) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.users...)
}

// fakeRadius replaces the RADIUS sync and Disconnect-Request for a test.
type fakeRadius struct {
	mu          sync.Mutex
	synced      []uint
	disconnects []string
}

func useFakeRadius(t *testing.T) *fakeRadius {
	t.Helper()
	f := &fakeRadius{}
	oldSync, oldDisc := homeRadiusSync, homeRadiusDisconnect
	homeRadiusSync = func(id uint) error { f.mu.Lock(); f.synced = append(f.synced, id); f.mu.Unlock(); return nil }
	homeRadiusDisconnect = func(z *models.Zone, u string) error {
		f.mu.Lock()
		f.disconnects = append(f.disconnects, fmt.Sprintf("%s@%s", u, z.RouterIP))
		f.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { homeRadiusSync, homeRadiusDisconnect = oldSync, oldDisc })
	return f
}

func (f *fakeRadius) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.synced), len(f.disconnects)
}

func setupHomeDB(t *testing.T) *gorm.DB {
	db := setupTestDB(t)
	if err := db.AutoMigrate(&models.HomeRenewal{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedHome makes a zone (auth mode as given) with a KES 30 daily home plan
// and one home customer.
func seedHome(t *testing.T, authMode string, credit float64, expires *time.Time, status string) (models.Customer, models.Package, models.Zone) {
	t.Helper()
	zone := models.Zone{Name: "Fibre", Location: "Maseno", RouterName: "rb5009", RouterIP: "10.200.0.9", AuthMode: authMode, RadiusSecret: "s3cret"}
	if err := config.DB.Create(&zone).Error; err != nil {
		t.Fatal(err)
	}
	pkg := models.Package{ZoneID: zone.ID, Name: "Home Daily", Type: "pppoe", Price: 30, SpeedUploadKbps: 5000, SpeedDownloadKbps: 10000, BillingCycle: "daily", Status: "active"}
	if err := config.DB.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	user, pw := "home1", "pw"
	c := models.Customer{Name: "Jane Otieno", Phone: fmt.Sprintf("2547%08d", time.Now().UnixNano()%1e8), ZoneID: zone.ID, PackageID: pkg.ID, Type: "pppoe",
		PPPoEUsername: &user, PPPoEPassword: &pw, Status: status, ExpiresAt: expires, CreditBalance: credit}
	if err := config.DB.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	return c, pkg, zone
}

func reloadHome(t *testing.T, id uint) models.Customer {
	t.Helper()
	var c models.Customer
	if err := config.DB.First(&c, id).Error; err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHomePeriodsCovered(t *testing.T) {
	cases := []struct {
		bal, price float64
		max, want  int
	}{
		{90, 30, 366, 3}, {89.99, 30, 366, 2}, {29.99, 30, 366, 0}, {30, 30, 366, 1},
		{100000, 30, 366, 366}, {50, 0, 366, 0}, {0.3, 0.1, 366, 3}, {210, 30, 1, 1},
	}
	for _, c := range cases {
		if got := HomePeriodsCovered(c.bal, c.price, c.max); got != c.want {
			t.Errorf("HomePeriodsCovered(%v, %v, %d) = %d, want %d", c.bal, c.price, c.max, got, c.want)
		}
	}
}

func TestRenewHomeFromCredit_BuysWholePeriodsKeepsRemainder(t *testing.T) {
	setupHomeDB(t)
	c, _, _ := seedHome(t, "api", 100, nil, "expired")

	var res HomeRenewResult
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		res, err = RenewHomeFromCredit(tx, c.ID, HomeMaxPeriods, "test")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Periods != 3 || res.Debited != 90 {
		t.Fatalf("bought %d periods for %v, want 3 for 90", res.Periods, res.Debited)
	}
	got := reloadHome(t, c.ID)
	if got.CreditBalance != 10 {
		t.Errorf("credit left %v, want 10", got.CreditBalance)
	}
	if got.Status != "active" || got.ExpiresAt == nil {
		t.Fatalf("status %q expires %v, want active with an expiry", got.Status, got.ExpiresAt)
	}
	if d := time.Until(*got.ExpiresAt); d < 71*time.Hour || d > 73*time.Hour {
		t.Errorf("paid for %v, want ~72h", d)
	}
	var logs []models.CreditLog
	config.DB.Where("customer_id = ? AND type = ?", c.ID, "debit").Find(&logs)
	if len(logs) != 1 || logs[0].Amount != 90 {
		t.Errorf("debit logs = %+v, want one of KES 90", logs)
	}
}

func TestRenewHomeFromCredit_ExtendsFromCurrentPaidUntil(t *testing.T) {
	setupHomeDB(t)
	until := time.Now().Add(48 * time.Hour)
	c, _, _ := seedHome(t, "api", 60, &until, "active")
	config.DB.Transaction(func(tx *gorm.DB) error { _, err := RenewHomeFromCredit(tx, c.ID, 0, "test"); return err })
	got := reloadHome(t, c.ID)
	if d := time.Until(*got.ExpiresAt); d < 95*time.Hour || d > 97*time.Hour {
		t.Errorf("paid until in %v, want ~96h (48h left + 2 days)", d)
	}
}

func TestRenewHomeFromCredit_NotEnoughOrNotHome(t *testing.T) {
	setupHomeDB(t)
	c, _, _ := seedHome(t, "api", 29, nil, "expired")
	var res HomeRenewResult
	config.DB.Transaction(func(tx *gorm.DB) error { var err error; res, err = RenewHomeFromCredit(tx, c.ID, 0, "t"); return err })
	if res.Periods != 0 || reloadHome(t, c.ID).CreditBalance != 29 {
		t.Error("29 of 30 must buy nothing and keep the credit")
	}

	// A hotspot customer is never renewed by this path.
	zoneID, pkgID := seedZone(t)
	h := models.Customer{Name: "H", Phone: "254700000123", ZoneID: zoneID, PackageID: pkgID, Type: "hotspot", Status: "expired", CreditBalance: 300}
	config.DB.Create(&h)
	config.DB.Transaction(func(tx *gorm.DB) error { var err error; res, err = RenewHomeFromCredit(tx, h.ID, 0, "t"); return err })
	if res.Periods != 0 || reloadHome(t, h.ID).CreditBalance != 300 {
		t.Error("hotspot customer must be untouched")
	}
}

func TestExpirySweep_HomeWithCreditIsRenewedNotCut(t *testing.T) {
	setupHomeDB(t)
	fr := useFakeRadius(t)
	past := time.Now().Add(-time.Minute)
	c, _, _ := seedHome(t, "api", 45, &past, "active")
	router := &fakeHomeRouter{}
	w := NewWatchdogService(nil)
	w.homeRouterOverride = router

	w.runOverloadProtectionSweep()

	got := reloadHome(t, c.ID)
	if got.Status != "active" || !got.ExpiresAt.After(time.Now().Add(23*time.Hour)) {
		t.Fatalf("status %q until %v — want renewed for a day", got.Status, got.ExpiresAt)
	}
	if got.CreditBalance != 15 {
		t.Errorf("credit %v, want 15 (one day bought, rest kept)", got.CreditBalance)
	}
	time.Sleep(50 * time.Millisecond)
	if len(router.got()) != 0 {
		t.Error("a renewed home must not be switched off")
	}
	if s, _ := fr.counts(); s != 1 {
		t.Errorf("radius sync called %d times, want 1 (new expiry)", s)
	}
}

func TestExpirySweep_UnpaidHomeSwitchedOff_APIMode(t *testing.T) {
	setupHomeDB(t)
	fr := useFakeRadius(t)
	past := time.Now().Add(-time.Minute)
	c, _, _ := seedHome(t, "api", 10, &past, "active")
	router := &fakeHomeRouter{}
	w := NewWatchdogService(nil)
	w.homeRouterOverride = router

	w.runOverloadProtectionSweep()

	if got := reloadHome(t, c.ID); got.Status != "expired" || got.CreditBalance != 10 {
		t.Fatalf("status %q credit %v, want expired with credit kept", got.Status, got.CreditBalance)
	}
	if !waitFor(t, func() bool { return len(router.got()) == 1 }) || router.got()[0] != "home1" {
		t.Fatalf("PPP secret switch-off calls = %v, want [home1]", router.got())
	}
	if _, d := fr.counts(); d != 0 {
		t.Error("API mode must not send RADIUS disconnects")
	}
}

func TestExpirySweep_UnpaidHomeSwitchedOff_RadiusMode(t *testing.T) {
	setupHomeDB(t)
	fr := useFakeRadius(t)
	past := time.Now().Add(-time.Minute)
	c, _, _ := seedHome(t, "radius", 0, &past, "active")
	router := &fakeHomeRouter{}
	w := NewWatchdogService(nil)
	w.homeRouterOverride = router

	w.runOverloadProtectionSweep()

	if reloadHome(t, c.ID).Status != "expired" {
		t.Fatal("unpaid home must be expired")
	}
	if !waitFor(t, func() bool { _, d := fr.counts(); return d == 1 }) {
		t.Fatal("RADIUS mode must send a Disconnect-Request for the live session")
	}
	if s, _ := fr.counts(); s != 1 {
		t.Errorf("radius rows not rewritten (sync calls %d)", s)
	}
	if fr.disconnects[0] != "home1@10.200.0.9" {
		t.Errorf("disconnect sent as %q", fr.disconnects[0])
	}
	if len(router.got()) != 0 {
		t.Error("RADIUS mode must not touch PPP secrets")
	}
}

func TestExpirySweep_DoesNotCutAHomeThatJustPaid(t *testing.T) {
	setupHomeDB(t)
	useFakeRadius(t)
	past := time.Now().Add(-time.Minute)
	c, _, _ := seedHome(t, "api", 0, &past, "active")
	// The payment lands between the sweep's read and its update.
	future := time.Now().Add(24 * time.Hour)
	config.DB.Model(&models.Customer{}).Where("id = ?", c.ID).Update("expires_at", future)
	router := &fakeHomeRouter{}
	w := NewWatchdogService(nil)
	w.homeRouterOverride = router
	w.runOverloadProtectionSweep()
	time.Sleep(50 * time.Millisecond)
	if reloadHome(t, c.ID).Status != "active" || len(router.got()) != 0 {
		t.Error("a just-paid home must stay on")
	}
}

func TestHomeRenewalPayment_ExtendsByPeriodsAndIsIdempotent(t *testing.T) {
	setupHomeDB(t)
	useFakeRadius(t)
	svc := newTestMpesaService()
	c, pkg, zone := seedHome(t, "api", 5, nil, "expired")
	pid := pkg.ID
	checkout := "ws_CO_home_1"
	p := models.Payment{CustomerID: &c.ID, ZoneID: zone.ID, PackageID: &pid, Phone: c.Phone, Amount: 210, Method: "mpesa", Status: "pending", MpesaTransactionID: &checkout}
	config.DB.Create(&p)
	config.DB.Create(&models.HomeRenewal{PaymentID: p.ID, CustomerID: c.ID, Periods: 7})

	if err := svc.ProcessPaymentSuccess(&p, "RCPT1", c.Phone); err != nil {
		t.Fatal(err)
	}
	got := reloadHome(t, c.ID)
	if got.Status != "active" || got.CreditBalance != 5 {
		t.Fatalf("status %q credit %v, want active with the KES 5 left over", got.Status, got.CreditBalance)
	}
	if d := time.Until(*got.ExpiresAt); d < 167*time.Hour || d > 169*time.Hour {
		t.Errorf("paid for %v, want 7 days", d)
	}
	if got.PackageID != pkg.ID {
		t.Error("home plan must not change")
	}
	// Reactivation queued (API mode) — through the fake router.
	router := svc.Router.(*fakeRouter)
	if !waitFor(t, func() bool { return router.count() == 1 }) {
		t.Error("router reactivation was not queued")
	}

	// A duplicate callback changes nothing.
	again := p
	again.Status = "completed"
	svc.ProcessPaymentSuccess(&again, "RCPT1", c.Phone)
	if g := reloadHome(t, c.ID); g.CreditBalance != 5 || !g.ExpiresAt.Equal(*got.ExpiresAt) {
		t.Errorf("duplicate callback changed the account: credit %v until %v", g.CreditBalance, g.ExpiresAt)
	}
}

func TestHomeRenewalPayment_SuspendedHomeKeepsMoneyAsCredit(t *testing.T) {
	setupHomeDB(t)
	useFakeRadius(t)
	svc := newTestMpesaService()
	c, pkg, zone := seedHome(t, "radius", 0, nil, "suspended")
	pid := pkg.ID
	p := models.Payment{CustomerID: &c.ID, ZoneID: zone.ID, PackageID: &pid, Phone: c.Phone, Amount: 30, Method: "mpesa", Status: "pending"}
	config.DB.Create(&p)
	config.DB.Create(&models.HomeRenewal{PaymentID: p.ID, CustomerID: c.ID, Periods: 1})
	svc.ProcessPaymentSuccess(&p, "RCPT2", c.Phone)
	got := reloadHome(t, c.ID)
	if got.Status != "suspended" || got.CreditBalance != 30 {
		t.Errorf("status %q credit %v — want still suspended with KES 30 credit", got.Status, got.CreditBalance)
	}
}

func TestHomeSwitchOff_BothModes(t *testing.T) {
	setupHomeDB(t)
	fr := useFakeRadius(t)
	router := &fakeHomeRouter{}
	c, _, zone := seedHome(t, "api", 0, nil, "suspended")
	if err := HomeSwitchOff(router, &c, &zone); err != nil {
		t.Fatal(err)
	}
	if len(router.got()) != 1 {
		t.Error("API mode: PPP secret must be disabled")
	}
	zone.AuthMode = "radius"
	HomeSwitchOff(router, &c, &zone)
	if s, d := fr.counts(); s != 1 || d != 1 || len(router.got()) != 1 {
		t.Errorf("RADIUS mode: sync %d disconnect %d router %d", s, d, len(router.got()))
	}
}

func TestHomePeriodLabels(t *testing.T) {
	m := 30 * 24 * 60
	cases := []struct {
		pkg   models.Package
		unit  string
		count int
	}{
		{models.Package{BillingCycle: "daily"}, "day", 1},
		{models.Package{BillingCycle: "weekly"}, "week", 1},
		{models.Package{BillingCycle: "monthly"}, "month", 1},
		{models.Package{TimeLimitMinutes: &m}, "day", 30},
	}
	for _, c := range cases {
		u, n := HomePeriod(&c.pkg)
		if u != c.unit || n != c.count {
			t.Errorf("%+v → %s×%d, want %s×%d", c.pkg.BillingCycle, u, n, c.unit, c.count)
		}
	}
}
