package services

import (
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
)

func TestParseLaneCounters(t *testing.T) {
	c, ok := ParseLaneCounters("10,20,30,1,2,3,")
	if !ok || c != [6]uint64{10, 20, 30, 1, 2, 3} {
		t.Errorf("got %v %v", c, ok)
	}
	for _, bad := range []string{"", "1,2,3", "1,2,3,4,5,x", "1,2,3,4,5,6,7", "-1,2,3,4,5,6"} {
		if _, ok := ParseLaneCounters(bad); ok {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestLaneHeartbeatParts_OnlyWhenActive(t *testing.T) {
	if s, q := laneHeartbeatParts(&models.Zone{SmartSpeed: true}); s != "" || q != "" {
		t.Error("no line speed → nothing sent")
	}
	if s, _ := laneHeartbeatParts(&models.Zone{SmartSpeed: false, UplinkDownMbps: 20, UplinkUpMbps: 10}); s != "" {
		t.Error("switched off → nothing sent")
	}
	z := &models.Zone{ID: 10, SmartSpeed: true, UplinkDownMbps: 20, UplinkUpMbps: 10, WanPort: "ether1", ProvisionToken: "tok"}
	s, q := laneHeartbeatParts(z)
	if !strings.Contains(s, `"zq-down-rt";"zq-down-quick";"zq-down-heavy";"zq-up-rt";"zq-up-quick";"zq-up-heavy"`) || !strings.Contains(s, `on-error={ :set ql "" }`) || q != ` . "&lanes=" . $ql` {
		t.Errorf("script %q query %q", s, q)
	}
	hb := heartbeatSource(z)
	if !strings.Contains(hb, `&lanes=\" . \$ql`) || strings.Count(hb, "{") != strings.Count(hb, "}") {
		t.Errorf("heartbeat:\n%s", hb)
	}
}

func TestSmartSpeed_IngestAndReport(t *testing.T) {
	db := setupWatchDB(t)
	if err := MigrateSmartSpeed(db); err != nil {
		t.Fatal(err)
	}
	now := eat(2026, 11, 15, 14, 30)
	seen := now.Add(-20 * time.Second)
	old := now.Add(-3 * 24 * time.Hour)
	on := mkZone(t, db, 1, "Oxygen", &seen)
	db.Model(&models.Zone{}).Where("id = ?", on.ID).Updates(map[string]interface{}{"uplink_down_mbps": 20, "uplink_up_mbps": 10})
	on.UplinkDownMbps, on.UplinkUpMbps, on.SmartSpeed = 20, 10, true
	mkZone(t, db, 1, "No speed", &seen)
	off := mkZone(t, db, 1, "Off", &seen)
	db.Model(&models.Zone{}).Where("id = ?", off.ID).Updates(map[string]interface{}{"uplink_down_mbps": 20, "uplink_up_mbps": 10, "smart_speed": false})
	dead := mkZone(t, db, 1, "Dead", &old)
	db.Model(&models.Zone{}).Where("id = ?", dead.ID).Updates(map[string]interface{}{"uplink_down_mbps": 20, "uplink_up_mbps": 10})
	mkZone(t, db, 2, "Other ISP", &seen)

	// 10 one-minute readings: games 1 Mbps, browsing 3 Mbps, heavy 6 Mbps down; 0.5 Mbps up each.
	perMin := func(mbps float64) uint64 { return uint64(mbps * 1e6 / 8 * 60) }
	var c [6]uint64
	for m := -10; m <= 0; m++ {
		IngestLaneReport(db, &on, c, now.Add(time.Duration(m)*time.Minute))
		c[0] += perMin(1)
		c[1] += perMin(3)
		c[2] += perMin(6)
		for i := 3; i < 6; i++ {
			c[i] += perMin(0.5)
		}
	}
	// A counter going backwards (lanes rebuilt) is skipped, not turned into a figure.
	if r := IngestLaneReport(db, &on, [6]uint64{}, now.Add(30*time.Second)); r == nil || r.DeltaDownRt != nil {
		t.Errorf("reset reading must carry no deltas: %+v", r)
	}
	db.Where("recorded_at > ?", dbT(now)).Delete(&models.ZoneLaneStat{})

	rep, err := BuildSmartSpeed(db, 1, nil, 60, now)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]SmartZone{}
	for _, z := range rep.Zones {
		by[z.Name] = z
	}
	if len(rep.Zones) != 4 {
		t.Fatalf("own ISP's 4 routers only, got %d", len(rep.Zones))
	}
	z := by["Oxygen"]
	if z.Status != "running" || !z.Running || len(z.Lanes) != 3 {
		t.Fatalf("oxygen: %+v", z)
	}
	want := []float64{1, 3, 6}
	share := []float64{10, 30, 60}
	for i, l := range z.Lanes {
		if l.DownMbps == nil || *l.DownMbps != want[i] || l.UpMbps == nil || *l.UpMbps != 0.5 {
			t.Errorf("lane %s now: %v / %v", l.Key, l.DownMbps, l.UpMbps)
		}
		if l.SharePct == nil || *l.SharePct != share[i] {
			t.Errorf("lane %s share: %v", l.Key, l.SharePct)
		}
	}
	// Today: 10 minutes of (1+0.5) Mbps = 112.5 MB for games.
	if mb := z.Lanes[0].TodayMB; mb == nil || *mb != 112.5 {
		t.Errorf("games today: %v", mb)
	}
	if len(z.Points) != 60 || z.BucketMin != 1 {
		t.Errorf("points %d bucket %d", len(z.Points), z.BucketMin)
	}
	lastPt := z.Points[len(z.Points)-1]
	if lastPt.Total == nil || *lastPt.Total != 10 || z.Points[0].Total != nil {
		t.Errorf("chart: last %v first %v", lastPt.Total, z.Points[0].Total)
	}
	for name, st := range map[string]string{"No speed": "needs_speed", "Off": "off", "Dead": "waiting"} {
		if by[name].Status != st || by[name].Running {
			t.Errorf("%s: status %q", name, by[name].Status)
		}
		for _, l := range by[name].Lanes {
			if l.DownMbps != nil || l.TodayMB != nil {
				t.Errorf("%s: figure invented for lane %s", name, l.Key)
			}
		}
	}
	if !strings.Contains(by["Dead"].StatusText, "router to come online") {
		t.Errorf("dead text: %q", by["Dead"].StatusText)
	}
	// Scope: a zone manager sees only their router.
	one, _ := BuildSmartSpeed(db, 1, []uint{off.ID}, 60, now)
	if len(one.Zones) != 1 || one.Zones[0].Name != "Off" {
		t.Errorf("scope: %+v", one.Zones)
	}
	// Stale figures aren't shown as "now".
	later, _ := BuildSmartSpeed(db, 1, []uint{on.ID}, 60, now.Add(20*time.Minute))
	if l := later.Zones[0]; l.Running || l.Lanes[0].DownMbps != nil || l.Lanes[0].TodayMB == nil {
		t.Errorf("20 min later: %+v", l.Lanes[0])
	}
}
