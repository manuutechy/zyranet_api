package services

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// createRadiusTables makes the slice of FreeRADIUS's schema this app touches.
// (In production these come from installing FreeRADIUS.)
func createRadiusTables(t *testing.T) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE radcheck (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL DEFAULT '', attribute TEXT NOT NULL DEFAULT '', op TEXT NOT NULL DEFAULT '==', value TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE radreply (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL DEFAULT '', attribute TEXT NOT NULL DEFAULT '', op TEXT NOT NULL DEFAULT '=', value TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE nas (id INTEGER PRIMARY KEY AUTOINCREMENT, nasname TEXT NOT NULL, shortname TEXT, type TEXT DEFAULT 'other', ports INTEGER, secret TEXT NOT NULL DEFAULT 'secret', server TEXT, community TEXT, description TEXT)`,
	} {
		if err := config.DB.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	ResetRadiusTableCache()
	t.Cleanup(ResetRadiusTableCache)
}

func radiusRows(t *testing.T, table, username string) map[string]string {
	t.Helper()
	var rows []struct{ Attribute, Value string }
	config.DB.Raw("SELECT attribute, value FROM "+table+" WHERE username = ?", username).Scan(&rows)
	out := map[string]string{}
	for _, r := range rows {
		out[r.Attribute] = r.Value
	}
	return out
}

func setZoneMode(t *testing.T, zoneID uint, mode string) {
	t.Helper()
	config.DB.Model(&models.Zone{}).Where("id = ?", zoneID).Update("auth_mode", mode)
}

func mkCustomer(t *testing.T, zoneID, pkgID uint, mac string, expires time.Time) models.Customer {
	t.Helper()
	// unique phone per customer (it seeds the unique account number)
	phone := "2547" + strings.NewReplacer(":", "", "-", "").Replace(mac)
	c := models.Customer{Name: "C", Phone: phone, ZoneID: zoneID, PackageID: pkgID, Type: "hotspot", Status: "active", ExpiresAt: &expires}
	if mac != "" {
		c.MacAddress = &mac
	}
	if err := config.DB.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRadiusMAC(t *testing.T) {
	for in, want := range map[string]string{"aa-bb-cc-dd-ee-ff": "AA:BB:CC:DD:EE:FF", " aa:bb:cc:dd:ee:ff ": "AA:BB:CC:DD:EE:FF"} {
		if got, ok := RadiusMAC(in); !ok || got != want {
			t.Errorf("RadiusMAC(%q) = %q,%v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "not-a-mac", "AA:BB:CC:DD:EE", "GG:BB:CC:DD:EE:FF"} {
		if _, ok := RadiusMAC(bad); ok {
			t.Errorf("RadiusMAC(%q) should be invalid", bad)
		}
	}
}

func TestDesiredRadiusUsers(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	exp := now.Add(24 * time.Hour)
	past := now.Add(-time.Minute)
	pkg := &models.Package{SpeedUploadKbps: 2048, SpeedDownloadKbps: 5120, DeviceLimit: 2}
	mac := "aa-bb-cc-dd-ee-01"
	base := models.Customer{ID: 7, ZoneID: 3, Type: "hotspot", Status: "active", ExpiresAt: &exp, MacAddress: &mac}

	u := desiredRadiusUsers(&base, pkg, nil, now)
	if len(u) != 1 || u[0].Username != "AA:BB:CC:DD:EE:01" || u[0].Kind != "hotspot" {
		t.Fatalf("hotspot user: %+v", u)
	}
	check := map[string]string{}
	for _, a := range u[0].Check {
		check[a.Attribute] = a.Value
	}
	reply := map[string]string{}
	for _, a := range u[0].Reply {
		reply[a.Attribute] = a.Value
	}
	if check["Auth-Type"] != "Accept" || check["Expiration"] != "Sep 23 2026 10:00:00" {
		t.Errorf("check attrs: %v", check)
	}
	if reply["Mikrotik-Rate-Limit"] != "2048k/5120k" || reply["Acct-Interim-Interval"] != "300" {
		t.Errorf("reply attrs: %v (rate must be upload/download, as the API-driven profiles use)", reply)
	}

	// Not paid up ⇒ no account, so the router rejects them.
	expired := base
	expired.ExpiresAt = &past
	suspended := base
	suspended.Status = "suspended"
	if len(desiredRadiusUsers(&expired, pkg, nil, now)) != 0 || len(desiredRadiusUsers(&suspended, pkg, nil, now)) != 0 || len(desiredRadiusUsers(&base, nil, nil, now)) != 0 {
		t.Error("expired / suspended / package-less customers must get no RADIUS account")
	}

	// Device limit caps devices; junk MACs and duplicates are ignored.
	devs := []string{"AA:BB:CC:DD:EE:01", "not-a-mac", "AA:BB:CC:DD:EE:02", "AA:BB:CC:DD:EE:03"}
	if got := desiredRadiusUsers(&base, pkg, devs, now); len(got) != 2 || got[1].Username != "AA:BB:CC:DD:EE:02" {
		t.Errorf("device limit 2: %+v", got)
	}
	one := &models.Package{SpeedUploadKbps: 1, SpeedDownloadKbps: 1, DeviceLimit: 1}
	if got := desiredRadiusUsers(&base, one, devs, now); len(got) != 1 {
		t.Errorf("device limit 1 allowed %d devices", len(got))
	}

	// PPPoE uses username/password, not a MAC.
	un, pw := "pppoe_jane", "s3cret"
	ppp := models.Customer{ID: 8, ZoneID: 3, Type: "pppoe", Status: "active", ExpiresAt: &exp, PPPoEUsername: &un, PPPoEPassword: &pw}
	pu := desiredRadiusUsers(&ppp, pkg, nil, now)
	if len(pu) != 1 || pu[0].Username != "pppoe_jane" || pu[0].Kind != "pppoe" || pu[0].Check[0].Attribute != "Cleartext-Password" {
		t.Errorf("pppoe user: %+v", pu)
	}
	ppp.PPPoEPassword = nil
	if len(desiredRadiusUsers(&ppp, pkg, nil, now)) != 0 {
		t.Error("a PPPoE customer with no password can't be given an account")
	}
}

func TestSyncZone_WritesRadiusRowsAndKeepsThemCurrent(t *testing.T) {
	setupTestDB(t)
	createRadiusTables(t)
	zoneID, pkgID := seedZone(t)
	setZoneMode(t, zoneID, "radius")
	svc := NewRadiusService()
	exp := time.Now().Add(48 * time.Hour)
	c := mkCustomer(t, zoneID, pkgID, "aa:bb:cc:dd:ee:10", exp)

	if err := svc.SyncZone(zoneID); err != nil {
		t.Fatal(err)
	}
	check := radiusRows(t, "radcheck", "AA:BB:CC:DD:EE:10")
	if check["Auth-Type"] != "Accept" || check["Expiration"] == "" {
		t.Fatalf("radcheck rows: %v", check)
	}
	if reply := radiusRows(t, "radreply", "AA:BB:CC:DD:EE:10"); reply["Mikrotik-Rate-Limit"] != "2048k/5120k" {
		t.Errorf("radreply rows: %v", reply)
	}
	var acct models.RadiusAccount
	config.DB.Where("username = ?", "AA:BB:CC:DD:EE:10").First(&acct)
	if !acct.Active || acct.CustomerID != c.ID || acct.ZoneID != zoneID {
		t.Errorf("mapping: %+v", acct)
	}

	// An unchanged user is not rewritten on the next pass.
	var idBefore int
	config.DB.Raw("SELECT MIN(id) FROM radcheck WHERE username = ?", "AA:BB:CC:DD:EE:10").Scan(&idBefore)
	svc.SyncZone(zoneID)
	var idAfter int
	config.DB.Raw("SELECT MIN(id) FROM radcheck WHERE username = ?", "AA:BB:CC:DD:EE:10").Scan(&idAfter)
	if idBefore != idAfter {
		t.Error("an unchanged customer's rows were rewritten")
	}

	// Renewal extends the Expiration the router will be told about.
	newExp := exp.Add(72 * time.Hour)
	config.DB.Model(&c).Update("expires_at", newExp)
	svc.SyncZone(zoneID)
	if got := radiusRows(t, "radcheck", "AA:BB:CC:DD:EE:10")["Expiration"]; got != radiusExpiration(newExp) {
		t.Errorf("after renewal Expiration = %q, want %q", got, radiusExpiration(newExp))
	}

	// Expiry removes the credentials but keeps the mapping, so history stays attributable.
	config.DB.Model(&c).Update("expires_at", time.Now().Add(-time.Hour))
	svc.SyncZone(zoneID)
	if len(radiusRows(t, "radcheck", "AA:BB:CC:DD:EE:10")) != 0 || len(radiusRows(t, "radreply", "AA:BB:CC:DD:EE:10")) != 0 {
		t.Error("an expired customer must have no RADIUS credentials (so the router rejects them)")
	}
	config.DB.Where("username = ?", "AA:BB:CC:DD:EE:10").First(&acct)
	if acct.Active || acct.CustomerID != c.ID {
		t.Errorf("mapping after expiry: %+v — must stay, inactive", acct)
	}
}

func TestSyncZone_OnlyTouchesItsOwnZone(t *testing.T) {
	setupTestDB(t)
	createRadiusTables(t)
	zoneA, pkgA := seedZone(t)
	zoneB := models.Zone{Name: "B", Location: "L", RouterName: "r", RouterIP: "9.9.9.9"}
	config.DB.Create(&zoneB)
	pkgB := models.Package{ZoneID: zoneB.ID, Name: "P", Type: "hotspot", Price: 10, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "active"}
	config.DB.Create(&pkgB)
	mkCustomer(t, zoneA, pkgA, "aa:bb:cc:00:00:01", time.Now().Add(time.Hour))
	mkCustomer(t, zoneB.ID, pkgB.ID, "aa:bb:cc:00:00:02", time.Now().Add(time.Hour))

	if err := NewRadiusService().SyncZone(zoneA); err != nil {
		t.Fatal(err)
	}
	if len(radiusRows(t, "radcheck", "AA:BB:CC:00:00:01")) == 0 {
		t.Error("zone A's customer should be synced")
	}
	if len(radiusRows(t, "radcheck", "AA:BB:CC:00:00:02")) != 0 {
		t.Error("syncing zone A must not write zone B's customers")
	}
}

func TestSyncCustomer_NoOpUnlessTheZoneIsInRadiusMode(t *testing.T) {
	setupTestDB(t)
	createRadiusTables(t)
	zoneID, pkgID := seedZone(t)
	svc := NewRadiusService()
	c := mkCustomer(t, zoneID, pkgID, "aa:bb:cc:dd:ee:20", time.Now().Add(time.Hour))

	if err := svc.SyncCustomer(c.ID); err != nil {
		t.Fatal(err)
	}
	if len(radiusRows(t, "radcheck", "AA:BB:CC:DD:EE:20")) != 0 {
		t.Fatal("a zone still in API mode must not get RADIUS rows")
	}
	setZoneMode(t, zoneID, "radius")
	if err := svc.SyncCustomer(c.ID); err != nil {
		t.Fatal(err)
	}
	if len(radiusRows(t, "radcheck", "AA:BB:CC:DD:EE:20")) == 0 {
		t.Error("a radius-mode customer should be written immediately")
	}
}

func TestClearZone_RemovesCredentialsKeepsMapping(t *testing.T) {
	setupTestDB(t)
	createRadiusTables(t)
	zoneID, pkgID := seedZone(t)
	setZoneMode(t, zoneID, "radius")
	svc := NewRadiusService()
	mkCustomer(t, zoneID, pkgID, "aa:bb:cc:dd:ee:30", time.Now().Add(time.Hour))
	svc.SyncZone(zoneID)

	if err := svc.ClearZone(zoneID); err != nil {
		t.Fatal(err)
	}
	if len(radiusRows(t, "radcheck", "AA:BB:CC:DD:EE:30")) != 0 {
		t.Error("credentials should be gone when a zone leaves RADIUS mode")
	}
	var n int64
	config.DB.Model(&models.RadiusAccount{}).Where("username = ?", "AA:BB:CC:DD:EE:30").Count(&n)
	if n != 1 {
		t.Error("the mapping must survive so past sessions stay attributable")
	}
}

func TestEnsureNAS(t *testing.T) {
	setupTestDB(t)
	createRadiusTables(t)
	svc := NewRadiusService()
	z := &models.Zone{ID: 4, Name: "Maseno", RouterIP: "10.200.0.2"}
	if err := svc.EnsureNAS(z); err == nil {
		t.Error("a zone with no secret must be refused")
	}
	z.RadiusSecret = "abc123"
	for i := 0; i < 2; i++ { // idempotent: a second call replaces, not duplicates
		if err := svc.EnsureNAS(z); err != nil {
			t.Fatal(err)
		}
	}
	var rows []struct{ Nasname, Shortname, Secret string }
	config.DB.Raw("SELECT nasname, shortname, secret FROM nas").Scan(&rows)
	if len(rows) != 1 || rows[0].Nasname != "10.200.0.2" || rows[0].Shortname != "zyra-zone-4" || rows[0].Secret != "abc123" {
		t.Errorf("nas rows: %+v", rows)
	}
	svc.RemoveNAS(4)
	var remaining int64
	config.DB.Table("nas").Count(&remaining)
	if remaining != 0 {
		t.Error("RemoveNAS should delete the client entry")
	}
}

func TestRadius_WithoutTheSchemaInstalled(t *testing.T) {
	setupTestDB(t) // no RADIUS tables
	ResetRadiusTableCache()
	t.Cleanup(ResetRadiusTableCache)
	zoneID, _ := seedZone(t)
	svc := NewRadiusService()
	if RadiusTablesExist() {
		t.Fatal("tables shouldn't exist here")
	}
	if err := svc.SyncZone(zoneID); !errors.Is(err, ErrRadiusNotInstalled) {
		t.Errorf("SyncZone: %v", err)
	}
	if err := svc.EnsureNAS(&models.Zone{ID: 1, RouterIP: "1.1.1.1", RadiusSecret: "x"}); !errors.Is(err, ErrRadiusNotInstalled) {
		t.Errorf("EnsureNAS: %v", err)
	}
	svc.Reconcile() // must not panic or error when nothing is set up
}
