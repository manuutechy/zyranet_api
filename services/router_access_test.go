package services

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// setupRouterAccessDB is setupTestDB plus the per-device login tables.
func setupRouterAccessDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t)
	if err := MigrateRouterAccess(db); err != nil {
		t.Fatalf("migrate router access: %v", err)
	}
	return db
}

type raFixture struct {
	org  models.Organization
	zone models.Zone
	pkg  models.Package
}

func seedRouterAccess(t *testing.T, db *gorm.DB, slug string, deviceLimit int) raFixture {
	t.Helper()
	org := models.Organization{Name: slug, Slug: slug}
	if err := db.Create(&org).Error; err != nil {
		t.Fatal(err)
	}
	zone := models.Zone{Name: slug + " zone", Location: "L", RouterName: "r", RouterIP: "196.0.0.9", OrganizationID: org.ID, ServiceMode: "hotspot"}
	if err := db.Create(&zone).Error; err != nil {
		t.Fatal(err)
	}
	pkg := models.Package{ZoneID: zone.ID, Name: "Daily Fast", Type: "hotspot", Status: "active", Price: 50, DeviceLimit: deviceLimit,
		SpeedUploadKbps: 2000, SpeedDownloadKbps: 5000, BillingCycle: "daily"}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	return raFixture{org, zone, pkg}
}

func seedPaidCustomer(t *testing.T, db *gorm.DB, f raFixture, phone, mac string, expires time.Time) models.Customer {
	t.Helper()
	c := models.Customer{Name: "Jane", Phone: phone, ZoneID: f.zone.ID, PackageID: f.pkg.ID, Type: "hotspot", Status: "active", ExpiresAt: &expires}
	if mac != "" {
		c.MacAddress = &mac
	}
	if err := db.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewCredentialSecrets_RandomAndRouterSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		u, p, err := NewCredentialSecrets()
		if err != nil {
			t.Fatal(err)
		}
		if !credNameRe.MatchString(u) || !credPassRe.MatchString(p) || len(p) < 14 {
			t.Fatalf("bad secrets %q / %q", u, p)
		}
		if strings.HasPrefix(u, "pkg-") || u == "free" || u == p {
			t.Fatalf("guessable login %q/%q", u, p)
		}
		if seen[u] || seen[p] {
			t.Fatalf("repeat after %d draws", i)
		}
		seen[u], seen[p] = true, true
	}
}

func TestRouterOSDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute:                 "30m",
		24 * time.Hour:                   "1d",
		26*time.Hour + 3*time.Minute + 4: "1d2h3m",
		90 * time.Second:                 "1m30s",
		0:                                "0s",
	} {
		if got := RouterOSDuration(d); got != want {
			t.Errorf("RouterOSDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestEnsureCredential_OwnLoginBoundToMAC(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	exp := time.Now().Add(24 * time.Hour)
	c := seedPaidCustomer(t, db, f, "254711000001", "aa:bb:cc:dd:ee:01", exp)

	cred, err := EnsureCredential(c.ID, "aa-bb-cc-dd-ee-01", "paid")
	if err != nil {
		t.Fatal(err)
	}
	if cred.MacAddress != "AA:BB:CC:DD:EE:01" || cred.OrganizationID != f.org.ID || !cred.ExpiresAt.Equal(exp) {
		t.Errorf("credential = %+v", cred)
	}
	again, err := EnsureCredential(c.ID, "AA:BB:CC:DD:EE:01", "")
	if err != nil || again.ID != cred.ID || again.Username != cred.Username || again.Password != cred.Password {
		t.Errorf("same device must keep the same login: %+v %v", again, err)
	}

	// No plan, no login.
	expired := seedPaidCustomer(t, db, f, "254711000002", "", time.Now().Add(-time.Hour))
	if _, err := EnsureCredential(expired.ID, "AA:BB:CC:DD:EE:02", ""); !errors.Is(err, ErrNoActivePlan) {
		t.Errorf("expired customer: err = %v, want ErrNoActivePlan", err)
	}
	if _, err := EnsureCredential(c.ID, "not-a-mac", ""); !errors.Is(err, ErrBadMAC) {
		t.Errorf("bad MAC: err = %v", err)
	}
}

func TestMoveCredential_RetiresOldDeviceAndItsTokens(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	c := seedPaidCustomer(t, db, f, "254711000001", "", time.Now().Add(5*time.Hour))

	old, err := EnsureCredential(c.ID, "AA:BB:CC:DD:EE:01", "")
	if err != nil {
		t.Fatal(err)
	}
	rl, err := IssueRouterLogin(old, true)
	if err != nil {
		t.Fatal(err)
	}

	moved, err := MoveCredential(c.ID, f.zone.ID, "AA:BB:CC:DD:EE:02", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID == old.ID || moved.MacAddress != "AA:BB:CC:DD:EE:02" || moved.Status != "active" {
		t.Fatalf("moved = %+v", moved)
	}
	var o models.HotspotCredential
	db.First(&o, old.ID)
	if o.Status != "revoked" || o.RevokedAt == nil {
		t.Errorf("old device login should be revoked, got %s", o.Status)
	}
	if _, err := CredentialByDeviceToken(rl.DeviceToken); err == nil {
		t.Error("the old device token must die with the old login")
	}
	if !moved.ExpiresAt.Equal(*c.ExpiresAt) {
		t.Error("remaining time must move unchanged")
	}

	// Another ISP's WiFi can't receive this customer's plan.
	other := seedRouterAccess(t, db, "other", 1)
	if _, err := MoveCredential(c.ID, other.zone.ID, "AA:BB:CC:DD:EE:03", 0); !errors.Is(err, ErrWrongISP) {
		t.Errorf("cross-ISP move: err = %v, want ErrWrongISP", err)
	}
}

func TestDeviceLimit_RetiresLeastRecentlyUsed(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 2)
	c := seedPaidCustomer(t, db, f, "254711000001", "", time.Now().Add(5*time.Hour))

	a, _ := EnsureCredential(c.ID, "AA:BB:CC:DD:EE:01", "")
	time.Sleep(5 * time.Millisecond)
	b, _ := EnsureCredential(c.ID, "AA:BB:CC:DD:EE:02", "")
	TouchCredentialLogin(a.ID) // a used more recently than b
	time.Sleep(5 * time.Millisecond)
	if _, err := EnsureCredential(c.ID, "AA:BB:CC:DD:EE:03", ""); err != nil {
		t.Fatal(err)
	}
	var active int64
	db.Model(&models.HotspotCredential{}).Where("customer_id = ? AND status = ?", c.ID, "active").Count(&active)
	if active != 2 {
		t.Errorf("active logins = %d, want the plan's 2", active)
	}
	var gotB models.HotspotCredential
	db.First(&gotB, b.ID)
	if gotB.Status != "revoked" {
		t.Errorf("least recently used device should be retired, b is %s", gotB.Status)
	}
}

func TestLoginTicket_OneTimeAndShortLived(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	c := seedPaidCustomer(t, db, f, "254711000001", "", time.Now().Add(time.Hour))
	cred, _ := EnsureCredential(c.ID, "AA:BB:CC:DD:EE:01", "")

	rl, err := IssueRouterLogin(cred, true)
	if err != nil || rl.LoginTicket == "" || rl.DeviceToken == "" {
		t.Fatalf("issue: %+v %v", rl, err)
	}
	got, err := ConsumeLoginTicket(rl.LoginTicket)
	if err != nil || got.ID != cred.ID {
		t.Fatalf("first use: %v", err)
	}
	if _, err := ConsumeLoginTicket(rl.LoginTicket); err == nil {
		t.Error("a ticket must work once")
	}
	if _, err := ConsumeLoginTicket(rl.DeviceToken); err == nil {
		t.Error("a device token is not a ticket")
	}
	if dc, err := CredentialByDeviceToken(rl.DeviceToken); err != nil || dc.ID != cred.ID {
		t.Errorf("device token: %v", err)
	}

	rl2, _ := IssueRouterLogin(cred, false)
	db.Model(&models.HotspotAccessToken{}).Where("token_hash = ?", HashAccessToken(rl2.LoginTicket)).
		Update("expires_at", time.Now().Add(-time.Second))
	if _, err := ConsumeLoginTicket(rl2.LoginTicket); err == nil {
		t.Error("an expired ticket must be refused")
	}
	var stored int64
	db.Model(&models.HotspotAccessToken{}).Where("token_hash = ?", rl.DeviceToken).Count(&stored)
	if stored != 0 {
		t.Error("tokens must be stored hashed, never in clear")
	}
}

func TestSyncScript_PerDeviceLoginsAndNoSharedUsers(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	free := models.Package{ZoneID: f.zone.ID, Name: "FREE TIER", Type: "hotspot", Status: "active", IsFreeTier: true, SpeedUploadKbps: 1000, SpeedDownloadKbps: 1000, BillingCycle: "hourly"}
	db.Create(&free)
	c := seedPaidCustomer(t, db, f, "254711000001", "00:11:22:33:44:55", time.Now().Add(24*time.Hour))
	db.Create(&models.Voucher{Code: "VCH12345", ZoneID: f.zone.ID, PackageID: f.pkg.ID, Status: "unused"})

	ReconcileCredentials(time.Time{})
	var cred models.HotspotCredential
	if err := db.Where("customer_id = ?", c.ID).First(&cred).Error; err != nil {
		t.Fatal("reconcile should give the paid device its own login")
	}

	svc := NewMikroTikScriptService()
	sync, err := svc.GenerateSyncScript(f.zone.ID)
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := svc.GenerateScript(f.zone.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name="` + cred.Username + `"`,
		`password="` + cred.Password + `"`,
		"mac-address=00:11:22:33:44:55",
		"limit-uptime=",
		"shared-users=1",
		`remove [find comment="Zyra Net Package Auto-Connect"]`,
		`remove [find name="free"]`,
		`remove [find comment~"^pkg:"]`,
	} {
		if !strings.Contains(sync, want) {
			t.Errorf("sync script missing %q", want)
		}
	}
	for name, script := range map[string]string{"sync": sync, "setup": full} {
		for _, bad := range []string{
			`add name="pkg-`, `password="pkg-`, `password="free"`, `add name="free"`,
			`name="VCH12345"`, `password="00:11:22:33:44:55"`, "shared-users=500",
		} {
			if strings.Contains(script, bad) {
				t.Errorf("%s script still hands out a shared/guessable login: %q", name, bad)
			}
		}
	}
}

func TestSyncScript_RemovesRetiredLoginsAndNeverWritesCustomers(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	c := seedPaidCustomer(t, db, f, "254711000001", "", time.Now().Add(time.Hour))
	cred, _ := EnsureCredential(c.ID, "AA:BB:CC:DD:EE:01", "")
	retireCredentials(db, "id = ?", cred.ID)

	// A customer whose time ran out but the watchdog hasn't caught yet.
	lapsed := seedPaidCustomer(t, db, f, "254711000002", "", time.Now().Add(-time.Minute))

	sync, err := NewMikroTikScriptService().GenerateSyncScript(f.zone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sync, `/ip hotspot user remove [find name="`+cred.Username+`"]`) ||
		!strings.Contains(sync, `/ip hotspot active remove [find user="`+cred.Username+`"]`) {
		t.Error("a retired login must be removed from the router and its session ended")
	}
	var after models.Customer
	db.First(&after, lapsed.ID)
	if after.Status != "active" || !after.UpdatedAt.Equal(lapsed.UpdatedAt) {
		t.Error("the router's sync GET must not write customers (that's the watchdog's job)")
	}
}

func TestManagementAccess_TunnelOnly(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	t.Setenv("ROUTER_MGMT_ALLOWED", "203.0.113.7, 1.2.3.4; /system reset ,198.51.100.0/24")
	old := config.Config.RadiusServerAddr
	config.Config.RadiusServerAddr = "10.200.0.1"
	t.Cleanup(func() { config.Config.RadiusServerAddr = old })

	svc := NewMikroTikScriptService()
	full, _, _ := svc.GenerateScript(f.zone.ID)
	sync, _ := svc.GenerateSyncScript(f.zone.ID)
	for name, s := range map[string]string{"setup": full, "sync": sync} {
		for _, want := range []string{
			"/ip service set winbox disabled=no port=8291 address=10.200.0.0/24,203.0.113.7/32,198.51.100.0/24",
			"/ip service set www disabled=no port=80 address=10.200.0.0/24,203.0.113.7/32,198.51.100.0/24",
			"/ip service set api disabled=no port=8728 address=10.200.0.1/32",
			"/ip service set api-ssl disabled=no port=8729 address=10.200.0.1/32",
			"src-address-list=zyranet-mgmt action=accept",
			"src-address-list=!zyranet-mgmt action=drop",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s script missing %q", name, want)
			}
		}
		if strings.Contains(s, "/system reset") || strings.Contains(s, "1.2.3.4;") {
			t.Errorf("%s: invalid ROUTER_MGMT_ALLOWED entries must never reach the router", name)
		}
		if strings.Contains(s, "in-interface=ether1 action=accept") {
			t.Errorf("%s: management must not be open to the whole WAN", name)
		}
	}
}

func TestFetchVerifiesCertificateWhenEnabled(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	svc := NewMikroTikScriptService()
	sync, _ := svc.GenerateSyncScript(f.zone.ID)
	if strings.Contains(sync, "check-certificate=yes") {
		t.Error("off by default (needs a router that trusts our chain)")
	}
	db.Model(&models.Zone{}).Where("id = ?", f.zone.ID).Update("verify_tls", true)
	sync, _ = svc.GenerateSyncScript(f.zone.ID)
	full, _, _ := svc.GenerateScript(f.zone.ID)
	if strings.Count(sync, "check-certificate=yes") < 3 || strings.Count(full, "check-certificate=yes") < 4 {
		t.Error("every /tool fetch to our API must check the certificate when the zone has it on")
	}
}

func TestZoneAcceptingPayments(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	set := func(seen *time.Time, status string) {
		db.Model(&models.Zone{}).Where("id = ?", f.zone.ID).Updates(map[string]interface{}{"last_seen_at": seen, "status": status})
	}
	if ok, msg := ZoneAcceptingPayments(f.zone.ID); ok || msg != ZoneOfflineMessage {
		t.Errorf("never-seen router: %v %q", ok, msg)
	}
	recent := time.Now().Add(-time.Minute)
	set(&recent, "active")
	if ok, _ := ZoneAcceptingPayments(f.zone.ID); !ok {
		t.Error("router seen a minute ago should take payments")
	}
	stale := time.Now().Add(-6 * time.Minute)
	set(&stale, "active")
	if ok, msg := ZoneAcceptingPayments(f.zone.ID); ok || !strings.Contains(msg, "not been charged") {
		t.Errorf("router silent for 6 min: %v %q", ok, msg)
	}
	set(&recent, "inactive")
	if ok, _ := ZoneAcceptingPayments(f.zone.ID); ok {
		t.Error("an inactive zone must not take payments")
	}
	if ok, _ := ZoneAcceptingPayments(99999); ok {
		t.Error("unknown zone must not take payments")
	}
}

func TestOutageCompensation(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	now := time.Now()
	start := now.Add(-30 * time.Minute)
	db.Model(&models.Zone{}).Where("id = ?", f.zone.ID).Update("last_seen_at", start)

	running := seedPaidCustomer(t, db, f, "254711000001", "", now.Add(2*time.Hour))
	ranOut := seedPaidCustomer(t, db, f, "254711000002", "", start.Add(10*time.Minute))
	db.Model(&ranOut).Update("status", "expired") // the watchdog expired it during the outage
	before := seedPaidCustomer(t, db, f, "254711000003", "", start.Add(-time.Minute))
	freePkg := models.Package{ZoneID: f.zone.ID, Name: "Trial", Type: "hotspot", Status: "active", IsFreeTier: true, BillingCycle: "hourly", SpeedUploadKbps: 1, SpeedDownloadKbps: 1}
	db.Create(&freePkg)
	trialExp := now.Add(5 * time.Minute)
	trial := models.Customer{Name: "t", Phone: "FREE1", ZoneID: f.zone.ID, PackageID: freePkg.ID, Type: "hotspot", Status: "active", ExpiresAt: &trialExp}
	db.Create(&trial)

	var zone models.Zone
	db.First(&zone, f.zone.ID)
	RecordRouterCheckIn(&zone, RouterCheckIn{SourceIP: "196.0.0.9", Heartbeat: true})

	get := func(id uint) models.Customer {
		var c models.Customer
		db.First(&c, id)
		return c
	}
	near := func(a, b time.Time) bool { d := a.Sub(b); return d < 5*time.Second && d > -5*time.Second }

	if r := get(running.ID); !near(*r.ExpiresAt, now.Add(2*time.Hour+30*time.Minute)) {
		t.Errorf("running plan should gain the 30 min outage: %v", r.ExpiresAt)
	}
	if r := get(ranOut.ID); r.Status != "active" || !near(*r.ExpiresAt, now.Add(10*time.Minute)) {
		t.Errorf("plan that ran out mid-outage gets its 10 lost minutes from now: %s %v", r.Status, r.ExpiresAt)
	}
	if r := get(before.ID); !r.ExpiresAt.Equal(*before.ExpiresAt) {
		t.Error("a plan that ended before the outage gets nothing")
	}
	if r := get(trial.ID); !r.ExpiresAt.Equal(*trial.ExpiresAt) {
		t.Error("free trials are not compensated")
	}
	var outages []models.ZoneOutage
	db.Find(&outages)
	if len(outages) != 1 || outages[0].CustomersCompensated != 2 || outages[0].DurationSeconds < 1790 {
		t.Fatalf("outage log = %+v", outages)
	}

	// The next check-in a moment later is not another outage.
	db.First(&zone, f.zone.ID)
	RecordRouterCheckIn(&zone, RouterCheckIn{SourceIP: "196.0.0.9", Heartbeat: true})
	var n int64
	db.Model(&models.ZoneOutage{}).Count(&n)
	if n != 1 {
		t.Errorf("outage recorded %d times, want once", n)
	}
	// And a check-in racing on stale data can't compensate twice.
	stale := zone
	stale.LastSeenAt = &start
	RecordRouterCheckIn(&stale, RouterCheckIn{Heartbeat: true})
	db.Model(&models.ZoneOutage{}).Count(&n)
	if n != 1 {
		t.Errorf("stale concurrent check-in compensated again (%d outages)", n)
	}
}

func TestRecordRouterCheckIn_RouterIP(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	seen := time.Now().Add(-time.Minute)
	db.Model(&models.Zone{}).Where("id = ?", f.zone.ID).Update("last_seen_at", seen)

	var zone models.Zone
	db.First(&zone, f.zone.ID)
	RecordRouterCheckIn(&zone, RouterCheckIn{SourceIP: "41.90.1.2", Heartbeat: true})
	db.First(&zone, f.zone.ID)
	if zone.RouterIP != "41.90.1.2" || zone.PublicIP != "41.90.1.2" {
		t.Errorf("router IP should follow the authenticated heartbeat source: %q / %q", zone.RouterIP, zone.PublicIP)
	}
	var logs int64
	db.Model(&models.AuditLog{}).Where("action = ? AND model_id = ?", "router_ip_changed", zone.ID).Count(&logs)
	if logs == 0 {
		t.Error("an IP change must be recorded")
	}

	// Private / loopback sources are ignored.
	RecordRouterCheckIn(&zone, RouterCheckIn{SourceIP: "192.168.1.5", Heartbeat: true})
	db.First(&zone, f.zone.ID)
	if zone.RouterIP != "41.90.1.2" {
		t.Error("a private source address must not replace the router IP")
	}

	// WireGuard-managed: the tunnel address stays (that's what the API dials).
	db.Model(&models.Zone{}).Where("id = ?", f.zone.ID).Update("router_ip", "10.200.0.7")
	db.First(&zone, f.zone.ID)
	RecordRouterCheckIn(&zone, RouterCheckIn{SourceIP: "41.90.9.9", Heartbeat: true})
	db.First(&zone, f.zone.ID)
	if zone.RouterIP != "10.200.0.7" || zone.PublicIP != "41.90.9.9" {
		t.Errorf("WireGuard router: router_ip %q (want tunnel), public_ip %q", zone.RouterIP, zone.PublicIP)
	}
}

func TestReconcile_FollowsCustomerExpiry(t *testing.T) {
	db := setupRouterAccessDB(t)
	f := seedRouterAccess(t, db, "isp", 1)
	c := seedPaidCustomer(t, db, f, "254711000001", "AA:BB:CC:DD:EE:01", time.Now().Add(time.Hour))
	ReconcileCredentials(time.Time{})
	var cred models.HotspotCredential
	if err := db.Where("customer_id = ? AND status = ?", c.ID, "active").First(&cred).Error; err != nil {
		t.Fatal("active customer should get a login")
	}
	since := time.Now()
	time.Sleep(5 * time.Millisecond)
	db.Model(&c).Updates(map[string]interface{}{"status": "expired"})
	ReconcileCredentials(since)
	db.First(&cred, cred.ID)
	if cred.Status != "expired" {
		t.Errorf("login should expire with the customer, got %s", cred.Status)
	}
}
