package services

import (
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
)

func TestBuildNetworkLive(t *testing.T) {
	db := setupWatchDB(t)
	if err := db.AutoMigrate(&models.Area{}, &models.ZoneStat{}); err != nil {
		t.Fatal(err)
	}
	now := eat(2026, 11, 12, 19, 30)
	seen := now.Add(-30 * time.Second)
	old := now.Add(-3 * time.Hour)
	a := models.Area{OrganizationID: 1, Name: "Maseno"}
	db.Create(&a)
	z1 := mkZone(t, db, 1, "Alpha", &seen)
	z2 := mkZone(t, db, 1, "Beta", &seen)
	z3 := mkZone(t, db, 1, "Gamma (dead)", &old)
	other := mkZone(t, db, 2, "Other ISP", &seen)
	db.Model(&models.Zone{}).Where("id IN ?", []uint{z1.ID, z2.ID}).Updates(map[string]interface{}{"uplink_down_mbps": 100, "uplink_up_mbps": 20})
	db.Model(&models.Zone{}).Where("id = ?", z1.ID).Update("area_id", a.ID)
	z1.UplinkDownMbps, z1.UplinkUpMbps, z2.UplinkDownMbps, z2.UplinkUpMbps = 100, 20, 100, 20

	// 10 minutes of readings: Alpha 30 Mbps down, Beta 10 Mbps down.
	feed := func(z *models.Zone, mbps float64, ppp, hs int) {
		var rx, tx uint64 = 1e9, 1e8
		for m := -10; m <= 0; m++ {
			IngestLineReport(db, z, LineReport{RxBytes: u64p(rx), TxBytes: u64p(tx), PPPoE: intp(ppp), Hotspot: intp(hs)}, now.Add(time.Duration(m)*time.Minute))
			rx += uint64(mbps * 1e6 / 8 * 60)
			tx += uint64(2e6 / 8 * 60)
		}
	}
	feed(&z1, 30, 12, 3)
	feed(&z2, 10, 4, 1)
	feed(&other, 999, 99, 99)
	// The dead router's old sessions must not count as "now".
	IngestLineReport(db, &z3, LineReport{PPPoE: intp(50)}, old)

	fast := models.Package{ZoneID: z1.ID, Name: "Home 10", Type: "pppoe", Price: 1500, SpeedDownloadKbps: 10240, SpeedUploadKbps: 5120, BillingCycle: "monthly"}
	hour := models.Package{ZoneID: z2.ID, Name: "1 hour", Type: "hotspot", Price: 10, SpeedDownloadKbps: 2048, SpeedUploadKbps: 1024, BillingCycle: "hourly"}
	db.Create(&fast)
	db.Create(&hour)
	mkC := func(z uint, p uint, typ, status string, exp time.Time, phone string) {
		e := exp.In(time.Local)
		c := models.Customer{Name: phone, Phone: phone, ZoneID: z, PackageID: p, Type: typ, Status: status, ExpiresAt: &e}
		if err := db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
	}
	mkC(z1.ID, fast.ID, "pppoe", "active", now.Add(72*time.Hour), "0700000001")
	mkC(z1.ID, fast.ID, "pppoe", "active", now.Add(-time.Hour), "0700000002") // ran out
	mkC(z1.ID, fast.ID, "pppoe", "suspended", now.Add(72*time.Hour), "0700000003")
	mkC(z2.ID, hour.ID, "hotspot", "active", now.Add(20*time.Minute), "0700000004")
	mkC(z2.ID, hour.ID, "hotspot", "active", now.Add(50*time.Minute), "0700000005")
	mkC(z2.ID, hour.ID, "hotspot", "expired", now.Add(-time.Minute), "0700000006")

	out, err := BuildNetworkLive(db, 1, nil, 60, now)
	if err != nil {
		t.Fatal(err)
	}
	c, sp := out.Counts, out.Speed
	if c.RoutersTotal != 3 || c.RoutersOnline != 2 {
		t.Errorf("routers %d/%d, want 2/3", c.RoutersOnline, c.RoutersTotal)
	}
	if sp.DownMbps == nil || *sp.DownMbps != 40 || sp.Reporting != 2 || sp.UplinkDownMbps != 200 {
		t.Fatalf("speed: %+v", sp)
	}
	if sp.DownPct == nil || *sp.DownPct != 20 || sp.UpPct == nil || *sp.UpPct != 10 {
		t.Errorf("pct: down %v up %v, want 20 / 10", sp.DownPct, sp.UpPct)
	}
	if sp.PeakTodayMbps == nil || *sp.PeakTodayMbps != 40 {
		t.Errorf("peak today %v, want 40", sp.PeakTodayMbps)
	}
	if c.PPPoE == nil || *c.PPPoE != 16 || c.Hotspot == nil || *c.Hotspot != 4 {
		t.Errorf("sessions ppp %v hs %v, want 16 / 4 (other ISP + dead router excluded)", c.PPPoE, c.Hotspot)
	}
	if c.ActivePlans != 3 || c.HomesOn != 1 || c.HomesExpired != 1 || c.HomesOff != 1 || c.HotspotActive != 2 || c.ExpiringNextHr != 2 {
		t.Errorf("counts: %+v", c)
	}
	if len(out.Minutes) != 60 {
		t.Fatalf("minutes: %d", len(out.Minutes))
	}
	last := out.Minutes[len(out.Minutes)-1]
	if last.DownMbps == nil || *last.DownMbps != 40 || last.Sessions == nil || *last.Sessions != 20 || last.Routers != 2 {
		t.Errorf("last minute: %+v", last)
	}
	if out.Minutes[0].DownMbps != nil {
		t.Errorf("an hour ago had no readings, got %v", *out.Minutes[0].DownMbps)
	}
	if len(out.Uptime) != 3 {
		t.Fatalf("uptime rows %d", len(out.Uptime))
	}
	for _, u := range out.Uptime {
		if len(u.Slots) != liveSlots {
			t.Fatalf("slots %d", len(u.Slots))
		}
		if u.Name == "Gamma (dead)" {
			if u.Online || u.Pct == nil || *u.Pct >= 50 {
				t.Errorf("dead router uptime: %+v", u)
			}
		} else if u.Slots[0] != -1 || u.Pct == nil || *u.Pct != 100 {
			t.Errorf("%s: before first reading should be -1 and uptime 100%%, got %v %v", u.Name, u.Slots[0], u.Pct)
		}
	}
	share := func(list []LiveShare, label string) int {
		for _, s := range list {
			if s.Label == label {
				return s.Count
			}
		}
		return -1
	}
	if share(out.ByZone, "Beta") != 2 || share(out.ByZone, "Alpha") != 1 {
		t.Errorf("by zone: %+v", out.ByZone)
	}
	if share(out.BySpeed, "2 Mbps") != 2 || share(out.BySpeed, "10 Mbps") != 1 || out.BySpeed[0].Label != "2 Mbps" {
		t.Errorf("by speed: %+v", out.BySpeed)
	}
	if share(out.ByArea, "Maseno") != 1 || share(out.ByArea, "No area") != 2 {
		t.Errorf("by area: %+v", out.ByArea)
	}

	// A zone manager's scope sees only their router.
	one, err := BuildNetworkLive(db, 1, []uint{z2.ID}, 60, now)
	if err != nil {
		t.Fatal(err)
	}
	if one.Counts.RoutersTotal != 1 || *one.Speed.DownMbps != 10 || one.Counts.ActivePlans != 2 || len(one.ByArea) != 0 {
		t.Errorf("scoped: %+v speed %v areas %+v", one.Counts, *one.Speed.DownMbps, one.ByArea)
	}
}

func TestSpeedLabel(t *testing.T) {
	for kbps, want := range map[int]string{0: "No limit", 512: "512 kbps", 1024: "1 Mbps", 10240: "10 Mbps", 1536: "1.5 Mbps"} {
		if got := speedLabel(kbps); got != want {
			t.Errorf("%d: %q want %q", kbps, got, want)
		}
	}
}
