package services

import (
	"strconv"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func intp(v int) *int { return &v }

func createRadacct(t *testing.T) {
	t.Helper()
	if err := config.DB.Exec(`CREATE TABLE radacct (radacctid INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL DEFAULT '', acctstarttime DATETIME, acctstoptime DATETIME, acctinputoctets INTEGER DEFAULT 0, acctoutputoctets INTEGER DEFAULT 0)`).Error; err != nil {
		t.Fatal(err)
	}
}

func addSession(t *testing.T, username string, start time.Time, in, out int64) {
	t.Helper()
	if err := config.DB.Exec("INSERT INTO radacct (username, acctstarttime, acctinputoctets, acctoutputoctets) VALUES (?, ?, ?, ?)", username, start.UTC(), in, out).Error; err != nil {
		t.Fatal(err)
	}
}

const mb = int64(1024 * 1024)

func TestUsageWindowStart(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	daily := &models.Package{BillingCycle: "daily"}

	// One day bought this morning: the window is that whole day.
	exp := now.Add(20 * time.Hour)
	if got := usageWindowStart(exp, daily, now); !got.Equal(exp.Add(-24 * time.Hour)) {
		t.Errorf("single day: %v", got)
	}
	// Three days stacked: only the current day counts, not all three.
	exp3 := now.Add(20*time.Hour + 48*time.Hour)
	if got := usageWindowStart(exp3, daily, now); !got.Equal(now.Add(-4 * time.Hour)) {
		t.Errorf("stacked days: %v, want %v", got, now.Add(-4*time.Hour))
	}
	// A time-limited plan uses its minutes, not the cycle.
	threeHours := &models.Package{BillingCycle: "daily", TimeLimitMinutes: intp(180)}
	if got := usageWindowStart(now.Add(time.Hour), threeHours, now); !got.Equal(now.Add(-2 * time.Hour)) {
		t.Errorf("3-hour plan: %v", got)
	}
	monthly := &models.Package{BillingCycle: "monthly"}
	if got := usageWindowStart(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), monthly, now); !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("monthly: %v", got)
	}
}

func TestRadiusReply_FairUse(t *testing.T) {
	burst := "1024k/3072k 20000k/20000k 716k/2150k 15/15 8"
	open := &models.Package{SpeedUploadKbps: 1024, SpeedDownloadKbps: 3072}
	capped := &models.Package{SpeedUploadKbps: 1024, SpeedDownloadKbps: 3072, DataLimitMB: intp(10 * 1024)}
	fair := &models.Package{SpeedUploadKbps: 1024, SpeedDownloadKbps: 3072, DataLimitMB: intp(10 * 1024), FairUseKbps: intp(1024)}
	attrs := func(r []radiusAttr) map[string]string {
		m := map[string]string{}
		for _, a := range r {
			m[a.Attribute] = a.Value
		}
		return m
	}

	if r, slow, ok := radiusReply(open, 0); !ok || slow || attrs(r)["Mikrotik-Rate-Limit"] != burst || attrs(r)["Mikrotik-Total-Limit"] != "" {
		t.Errorf("uncapped plan: %v %v %v", r, slow, ok)
	}

	// Fair use: full speed under the cap, slowed (never refused) past it.
	if r, slow, ok := radiusReply(fair, 3*1024*mb); !ok || slow || attrs(r)["Mikrotik-Rate-Limit"] != burst {
		t.Errorf("fair use under cap: %v %v %v", r, slow, ok)
	}
	if r, slow, ok := radiusReply(fair, 11*1024*mb); !ok || !slow || attrs(r)["Mikrotik-Rate-Limit"] != "1024k/1024k" {
		t.Errorf("fair use over cap: %v %v %v", r, slow, ok)
	}

	// Hard cap: told the bytes left (split across the 4 GB gigawords), refused at the cap.
	r, _, ok := radiusReply(capped, 1024*mb) // 1 GB used of 10
	left := 9 * 1024 * mb
	if a := attrs(r); !ok || a["Mikrotik-Total-Limit-Gigawords"] != "2" || a["Mikrotik-Total-Limit"] != itoa64(left&0xFFFFFFFF) {
		t.Errorf("hard cap remaining: %v", a)
	}
	if a := attrs(must(radiusReply(capped, usageUnknown))); a["Mikrotik-Total-Limit-Gigawords"] != "2" || a["Mikrotik-Total-Limit"] != itoa64((10*1024*mb)&0xFFFFFFFF) {
		t.Errorf("unknown usage gets the full cap per session: %v", a)
	}
	if _, _, ok := radiusReply(capped, 10*1024*mb); ok {
		t.Error("hard cap reached must refuse")
	}
}

func must(r []radiusAttr, _ bool, _ bool) []radiusAttr { return r }
func itoa64(v int64) string                            { return strconv.FormatInt(v, 10) }

func TestSyncZone_FairUseFollowsRealUsage(t *testing.T) {
	setupTestDB(t)
	createRadiusTables(t)
	createRadacct(t)
	zoneID, pkgID := seedZone(t)
	setZoneMode(t, zoneID, "radius")
	config.DB.Model(&models.Package{}).Where("id = ?", pkgID).Updates(map[string]interface{}{"data_limit_mb": 1024, "fair_use_kbps": 512})
	svc := NewRadiusService()

	exp := time.Now().Add(20 * time.Hour) // daily plan bought 4 h ago
	mkCustomer(t, zoneID, pkgID, "aa:bb:cc:dd:ee:20", exp)
	user := "AA:BB:CC:DD:EE:20"
	if err := svc.SyncZone(zoneID); err != nil {
		t.Fatal(err)
	}
	if got := radiusRows(t, "radreply", user)["Mikrotik-Rate-Limit"]; got == "512k/512k" {
		t.Fatalf("slowed before using anything: %q", got)
	}

	// Usage from BEFORE this plan window doesn't count.
	addSession(t, user, time.Now().Add(-30*time.Hour), 900*mb, 900*mb)
	svc.SyncZone(zoneID)
	if got := radiusRows(t, "radreply", user)["Mikrotik-Rate-Limit"]; got == "512k/512k" {
		t.Fatalf("old usage counted against the new plan: %q", got)
	}

	// 1.1 GB this window (across two sessions) ⇒ slowed, still connected.
	addSession(t, user, time.Now().Add(-3*time.Hour), 100*mb, 500*mb)
	addSession(t, user, time.Now().Add(-1*time.Hour), 50*mb, 450*mb)
	svc.SyncZone(zoneID)
	if got := radiusRows(t, "radreply", user)["Mikrotik-Rate-Limit"]; got != "512k/512k" {
		t.Errorf("over the cap: rate %q, want 512k/512k", got)
	}
	if len(radiusRows(t, "radcheck", user)) == 0 {
		t.Error("fair use must slow, not refuse")
	}

	// Same plan without fair use ⇒ refused at the cap.
	config.DB.Model(&models.Package{}).Where("id = ?", pkgID).Update("fair_use_kbps", nil)
	svc.SyncZone(zoneID)
	if len(radiusRows(t, "radcheck", user)) != 0 {
		t.Error("hard cap reached: the customer must be refused")
	}
}
