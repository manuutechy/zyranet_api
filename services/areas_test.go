package services

import (
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

type areaFixture struct {
	org   models.Organization
	area  models.Area
	zones []models.Zone
	tpl   models.AreaPackage
}

func setupAreaDB(t *testing.T) {
	t.Helper()
	db := setupTestDB(t)
	if err := db.AutoMigrate(&models.Area{}, &models.AreaPackage{}, &models.UserArea{}); err != nil {
		t.Fatal(err)
	}
}

func newAreaFixture(t *testing.T, nZones int) areaFixture {
	t.Helper()
	f := areaFixture{org: models.Organization{Name: "isp", Slug: "isp"}}
	config.DB.Create(&f.org)
	f.area = models.Area{OrganizationID: f.org.ID, Name: "Maseno"}
	config.DB.Create(&f.area)
	for i := 0; i < nZones; i++ {
		z := models.Zone{Name: string(rune('A'+i)) + " block", Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: f.org.ID, AreaID: &f.area.ID}
		config.DB.Create(&z)
		f.zones = append(f.zones, z)
	}
	f.tpl = models.AreaPackage{AreaID: f.area.ID, OrganizationID: f.org.ID, Name: "Daily", Type: "hotspot", Category: "single", DeviceLimit: 1,
		Price: 30, SpeedUploadKbps: 2000, SpeedDownloadKbps: 5000, BillingCycle: "daily", Status: "active"}
	config.DB.Create(&f.tpl)
	return f
}

func linked(t *testing.T, zoneID, tplID uint) models.Package {
	t.Helper()
	var p models.Package
	if err := config.DB.Unscoped().Where("zone_id = ? AND area_package_id = ?", zoneID, tplID).First(&p).Error; err != nil {
		t.Fatalf("no linked package in zone %d: %v", zoneID, err)
	}
	return p
}

func mustApply(t *testing.T, f areaFixture, dry bool) *AreaApplyResult {
	t.Helper()
	r, err := ApplyAreaPriceList(config.DB, f.org.ID, f.area.ID, 0, dry)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAreaApply_AddsThenIsIdempotent(t *testing.T) {
	setupAreaDB(t)
	f := newAreaFixture(t, 2)

	preview := mustApply(t, f, true)
	if preview.Added != 2 {
		t.Fatalf("preview added = %d, want 2", preview.Added)
	}
	var n int64
	config.DB.Model(&models.Package{}).Count(&n)
	if n != 0 {
		t.Fatalf("preview wrote %d packages", n)
	}

	r := mustApply(t, f, false)
	if r.Added != 2 || r.Updated != 0 {
		t.Fatalf("first apply: %+v", r)
	}
	p := linked(t, f.zones[0].ID, f.tpl.ID)
	if p.Price != 30 || p.SpeedDownloadKbps != 5000 || p.Status != "active" || p.AreaOverride {
		t.Errorf("copied package wrong: %+v", p)
	}

	r = mustApply(t, f, false)
	if r.Added != 0 || r.Updated != 0 || r.Same != 2 {
		t.Fatalf("second apply should change nothing: %+v", r)
	}
	config.DB.Model(&models.Package{}).Count(&n)
	if n != 2 {
		t.Fatalf("packages = %d, want 2", n)
	}
}

func TestAreaApply_SkipsOwnPriceAndUpdatesOthers(t *testing.T) {
	setupAreaDB(t)
	f := newAreaFixture(t, 2)
	mustApply(t, f, false)

	own := linked(t, f.zones[0].ID, f.tpl.ID)
	config.DB.Model(&own).Updates(map[string]interface{}{"price": 25, "area_override": true})

	config.DB.Model(&f.tpl).Updates(map[string]interface{}{"price": 35, "speed_download_kbps": 8000})
	preview := mustApply(t, f, true)
	if preview.Updated != 1 || preview.Skipped != 1 {
		t.Fatalf("preview: %+v", preview)
	}
	r := mustApply(t, f, false)
	if r.Updated != 1 || r.Skipped != 1 {
		t.Fatalf("apply: %+v", r)
	}
	if p := linked(t, f.zones[0].ID, f.tpl.ID); p.Price != 25 {
		t.Errorf("own price was overwritten: %v", p.Price)
	}
	if p := linked(t, f.zones[1].ID, f.tpl.ID); p.Price != 35 || p.SpeedDownloadKbps != 8000 {
		t.Errorf("area price not applied: %+v", p)
	}

	// Clearing the override and applying again puts the area price back.
	config.DB.Model(&models.Package{}).Where("id = ?", own.ID).Update("area_override", false)
	if _, err := ApplyAreaPriceList(config.DB, f.org.ID, f.area.ID, f.zones[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if p := linked(t, f.zones[0].ID, f.tpl.ID); p.Price != 35 {
		t.Errorf("use area price again: %v", p.Price)
	}
}

func TestAreaApply_AdoptsSameNamePlanAndKeepsDeletedOnesDeleted(t *testing.T) {
	setupAreaDB(t)
	f := newAreaFixture(t, 2)
	existing := models.Package{ZoneID: f.zones[0].ID, Name: "daily", Type: "hotspot", Price: 20, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "active"}
	config.DB.Create(&existing)

	r := mustApply(t, f, false)
	if r.Added != 1 || r.Updated != 1 {
		t.Fatalf("apply: %+v", r)
	}
	var count int64
	config.DB.Model(&models.Package{}).Where("zone_id = ?", f.zones[0].ID).Count(&count)
	if count != 1 {
		t.Fatalf("duplicate plan created next to the existing one: %d", count)
	}
	if p := linked(t, f.zones[0].ID, f.tpl.ID); p.ID != existing.ID || p.Price != 30 {
		t.Errorf("existing plan not adopted: %+v", p)
	}

	// The zone deleted its copy: apply must not bring it back.
	p := linked(t, f.zones[1].ID, f.tpl.ID)
	config.DB.Delete(&p)
	r = mustApply(t, f, false)
	if r.Added != 0 || r.Skipped != 1 {
		t.Fatalf("deleted copy: %+v", r)
	}
}

func TestAreaTemplateDelete_TurnsOffLinkedPlansNeverDeletes(t *testing.T) {
	setupAreaDB(t)
	f := newAreaFixture(t, 2)
	mustApply(t, f, false)
	own := linked(t, f.zones[0].ID, f.tpl.ID)
	config.DB.Model(&own).Update("area_override", true)
	follows := linked(t, f.zones[1].ID, f.tpl.ID)
	pkgID := follows.ID
	config.DB.Create(&models.Payment{ZoneID: f.zones[1].ID, PackageID: &pkgID, Phone: "254700000001", Amount: 30, Method: "mpesa", Status: "completed"})

	off, err := DeleteAreaTemplate(config.DB, f.org.ID, f.area.ID, f.tpl.ID)
	if err != nil || off != 1 {
		t.Fatalf("off = %d, err = %v", off, err)
	}
	if p := linked(t, f.zones[1].ID, f.tpl.ID); p.Status != "inactive" || p.DeletedAt.Valid {
		t.Errorf("following plan: %+v", p)
	}
	if p := linked(t, f.zones[0].ID, f.tpl.ID); p.Status != "active" {
		t.Errorf("own-price plan was turned off")
	}
	// Another org can't delete it.
	if _, err := DeleteAreaTemplate(config.DB, f.org.ID+1, f.area.ID, f.tpl.ID); err == nil {
		t.Error("other org deleted a template")
	}
}

func TestAssignZonesToArea_AppliesAndMovesBetweenAreas(t *testing.T) {
	setupAreaDB(t)
	f := newAreaFixture(t, 1)
	mustApply(t, f, false)

	other := models.Area{OrganizationID: f.org.ID, Name: "Kisumu"}
	config.DB.Create(&other)
	otpl := models.AreaPackage{AreaID: other.ID, OrganizationID: f.org.ID, Name: "Weekly", Type: "hotspot", Category: "single", DeviceLimit: 1,
		Price: 180, SpeedUploadKbps: 1000, SpeedDownloadKbps: 3000, BillingCycle: "weekly", Status: "active"}
	config.DB.Create(&otpl)

	r, err := AssignZonesToArea(config.DB, f.org.ID, other.ID, []uint{f.zones[0].ID})
	if err != nil || r.Added != 1 {
		t.Fatalf("assign: %+v %v", r, err)
	}
	var z models.Zone
	config.DB.First(&z, f.zones[0].ID)
	if z.AreaID == nil || *z.AreaID != other.ID {
		t.Fatalf("zone not moved: %v", z.AreaID)
	}
	// The old area's plan stays on sale as the zone's own plan.
	var old models.Package
	config.DB.Where("zone_id = ? AND name = ?", z.ID, "Daily").First(&old)
	if old.AreaPackageID != nil || old.Status != "active" {
		t.Errorf("old plan: %+v", old)
	}
	linked(t, z.ID, otpl.ID)

	// Another org's zone can't be pulled in.
	stranger := models.Zone{Name: "x", Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: f.org.ID + 99}
	config.DB.Create(&stranger)
	if _, err := AssignZonesToArea(config.DB, f.org.ID, other.ID, []uint{stranger.ID}); err == nil {
		t.Error("assigned another ISP's zone")
	}
}

func TestBuildAreaMoney_ByAreaWithNoAreaAndRemovedZones(t *testing.T) {
	setupAreaDB(t)
	f := newAreaFixture(t, 2)
	loose := models.Zone{Name: "Loose", Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: f.org.ID}
	config.DB.Create(&loose)

	// 2 Nov EAT 00:30 = 1 Nov 21:30 UTC: belongs to 2 Nov in Kenya.
	in := time.Date(2026, 11, 1, 21, 30, 0, 0, time.UTC)
	before := time.Date(2026, 11, 1, 20, 59, 0, 0, time.UTC) // 1 Nov 23:59 EAT
	mk := func(zone uint, phone string, amt float64, status string, at time.Time) {
		p := models.Payment{ZoneID: zone, Phone: phone, Amount: amt, Method: "mpesa", Status: status}
		config.DB.Create(&p)
		config.DB.Model(&p).Update("created_at", at.In(time.Local))
	}
	mk(f.zones[0].ID, "254700000001", 30, "completed", in)
	mk(f.zones[1].ID, "254700000001", 30, "completed", in) // same buyer, other zone
	mk(f.zones[1].ID, "254700000002", 50, "completed", in)
	mk(f.zones[1].ID, "254700000003", 50, "failed", in)
	mk(f.zones[0].ID, "254700000004", 99, "completed", before)
	mk(loose.ID, "254700000005", 20.5, "completed", in)

	// A removed zone that took money in the range still counts.
	gone := models.Zone{Name: "Gone", Location: "L", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: f.org.ID, AreaID: &f.area.ID}
	config.DB.Create(&gone)
	mk(gone.ID, "254700000006", 100, "completed", in)
	config.DB.Delete(&gone)

	exp := time.Now().Add(time.Hour)
	config.DB.Create(&models.Customer{Name: "a", Phone: "254700000001", ZoneID: f.zones[0].ID, PackageID: 1, Type: "hotspot", Status: "active", ExpiresAt: &exp})
	past := time.Now().Add(-time.Hour)
	config.DB.Create(&models.Customer{Name: "b", Phone: "254700000002", ZoneID: f.zones[1].ID, PackageID: 1, Type: "hotspot", Status: "active", ExpiresAt: &past})

	var ids []uint
	config.DB.Unscoped().Model(&models.Zone{}).Where("organization_id = ?", f.org.ID).Pluck("id", &ids)
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, EAT)
	rep, err := BuildAreaMoney(config.DB, f.org.ID, ids, []models.Area{f.area}, start, start.AddDate(0, 0, 1), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Areas) != 2 || rep.Areas[0].Name != "Maseno" || rep.Areas[1].Name != "No area" {
		t.Fatalf("areas: %+v", rep.Areas)
	}
	m := rep.Areas[0]
	if m.Takings != 210 || m.Payments != 4 || m.PayingCustomers != 3 || m.ActiveCustomers != 1 {
		t.Errorf("Maseno: %+v", m)
	}
	if len(m.Zones) != 3 {
		t.Errorf("Maseno zones: %+v", m.Zones)
	}
	var sawGone bool
	for _, z := range m.Zones {
		if z.Name == "Gone" {
			sawGone = z.Removed && z.Takings == 100
		}
	}
	if !sawGone {
		t.Error("removed zone's money missing")
	}
	if n := rep.Areas[1]; n.Takings != 20.5 || n.Payments != 1 {
		t.Errorf("No area: %+v", n)
	}
	if rep.Total.Takings != 230.5 || rep.Total.Payments != 5 || rep.Total.PayingCustomers != 4 {
		t.Errorf("total: %+v", rep.Total)
	}
}
