package services

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

func setupOpsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t)
	if err := db.AutoMigrate(&models.ZoneOutage{}); err != nil {
		t.Fatal(err)
	}
	if err := MigrateOpsTables(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&models.Payment{}, "idx_payments_zone_created") {
		t.Fatal("payments (zone_id, created_at) index not created")
	}
	// Running it again must be a no-op.
	if err := MigrateOpsTables(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func eat(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, EAT)
}

func mkZone(t *testing.T, db *gorm.DB, org uint, name string, lastSeen *time.Time) models.Zone {
	t.Helper()
	z := models.Zone{Name: name, Location: "Test", RouterName: "RB", RouterIP: "10.0.0.1", OrganizationID: org, Status: "active", LastSeenAt: lastSeen}
	if err := db.Create(&z).Error; err != nil {
		t.Fatal(err)
	}
	return z
}

func mkPay(t *testing.T, db *gorm.DB, zone uint, phone string, amt float64, status, reason string, at time.Time) models.Payment {
	t.Helper()
	p := models.Payment{ZoneID: zone, Phone: phone, Amount: amt, Method: "mpesa", Status: status, CreatedAt: at.In(time.Local), UpdatedAt: at.In(time.Local)}
	if reason != "" {
		r := reason
		p.StatusReason = &r
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func ptrT(t time.Time) *time.Time { return &t }

func TestClassifyFailureReason(t *testing.T) {
	cases := map[string]string{
		"Payment cancelled. You closed the M-Pesa prompt before approving it.": FailCancelled,
		STKNoResponseReason: FailNoResponse,
		"Payment timed out. You didn't enter your M-Pesa PIN in time.":                                FailNoResponse,
		"Payment request expired. Please try again.":                                                  FailNoResponse,
		"Payment failed: not enough money in your M-Pesa account. Top up M-Pesa and try again.":       FailLowBalance,
		"Payment failed: insufficient M-Pesa balance.":                                                FailLowBalance,
		"Payment failed: wrong M-Pesa PIN entered. Please try again with the correct PIN.":            FailWrongPIN,
		"Payment failed: you have another M-Pesa transaction in progress. Please finish or cancel it": FailBusy, // mentions "cancel" but isn't one
		"Payment could not be completed. Please try again, or contact support if you were charged.":   FailSystem,
		STKUnconfirmedReason:              FailSystem,
		"daraja auth returned status 503": FailSystem,
		"":                                FailSystem,
		"Failed to initiate M-Pesa STK Push payment.": FailSystem,
	}
	for in, want := range cases {
		if got := ClassifyFailureReason(in); got != want {
			t.Errorf("ClassifyFailureReason(%q) = %s, want %s", in, got, want)
		}
	}
	if got := ClassifyFailureReason("Payment failed: you have another M-Pesa transaction in progress."); got != FailBusy {
		t.Errorf("busy: got %s", got)
	}
}

func TestBuildLiveOps(t *testing.T) {
	db := setupOpsDB(t)
	now := eat(2026, 10, 22, 14, 30) // Thursday
	zA := mkZone(t, db, 1, "Alpha", ptrT(now.Add(-time.Minute)))
	zB := mkZone(t, db, 1, "Bravo", ptrT(now.Add(-20*time.Minute)))
	zC := mkZone(t, db, 1, "Deleted", ptrT(now))
	zD := mkZone(t, db, 2, "OtherISP", ptrT(now))
	zN := mkZone(t, db, 1, "NeverSeen", nil)
	_ = zN
	db.Delete(&zC)

	// Today.
	p50 := mkPay(t, db, zA.ID, "254711000001", 50, "completed", "", eat(2026, 10, 22, 8, 10))
	mkPay(t, db, zA.ID, "254711000002", 100, "completed", "", eat(2026, 10, 22, 13, 5))
	p20 := mkPay(t, db, zB.ID, "254711000001", 20, "completed", "", eat(2026, 10, 22, 13, 40))
	mkPay(t, db, zA.ID, "254711000003", 50, "failed", "Payment cancelled. You closed the M-Pesa prompt before approving it.", eat(2026, 10, 22, 9, 0))
	mkPay(t, db, zA.ID, "254711000003", 50, "failed", "Payment cancelled. You closed the M-Pesa prompt before approving it.", eat(2026, 10, 22, 9, 1))
	mkPay(t, db, zA.ID, "254711000004", 50, "failed", STKNoResponseReason, eat(2026, 10, 22, 10, 0))
	mkPay(t, db, zA.ID, "254711000005", 50, "failed", "Payment failed: insufficient M-Pesa balance.", eat(2026, 10, 22, 11, 0))
	mkPay(t, db, zB.ID, "254711000006", 50, "failed", "Payment failed: wrong M-Pesa PIN entered.", eat(2026, 10, 22, 12, 0))
	mkPay(t, db, zB.ID, "254711000007", 50, "failed", "daraja auth returned status 503: no healthy upstream", eat(2026, 10, 22, 12, 30))
	mkPay(t, db, zA.ID, "254711000008", 50, "pending", "", eat(2026, 10, 22, 14, 29))
	// Yesterday: 30 before the same time, 40 after.
	mkPay(t, db, zA.ID, "254711000009", 30, "completed", "", eat(2026, 10, 21, 10, 0))
	mkPay(t, db, zA.ID, "254711000010", 40, "completed", "", eat(2026, 10, 21, 16, 0))
	// Same weekday last week: 60 before, 90 after.
	mkPay(t, db, zA.ID, "254711000011", 60, "completed", "", eat(2026, 10, 15, 9, 0))
	mkPay(t, db, zA.ID, "254711000012", 90, "completed", "", eat(2026, 10, 15, 15, 0))
	// Noise that must never count.
	mkPay(t, db, zA.ID, "254711000013", 999, "completed", "", eat(2026, 10, 20, 12, 0))
	mkPay(t, db, zC.ID, "254711000014", 500, "completed", "", eat(2026, 10, 22, 12, 0))
	mkPay(t, db, zD.ID, "254711000015", 700, "completed", "", eat(2026, 10, 22, 12, 0))
	mkPay(t, db, zA.ID, "254711000016", 5, "completed", "", eat(2026, 10, 22, 14, 31)) // after "now"? still today: counts

	// Routers' client reports.
	db.Create(&models.ZoneStat{ZoneID: zA.ID, ConnectedClients: 3, RecordedAt: now.Add(-30 * time.Minute).In(time.Local)})
	db.Create(&models.ZoneStat{ZoneID: zA.ID, ConnectedClients: 7, RecordedAt: now.Add(-2 * time.Minute).In(time.Local)})
	db.Create(&models.ZoneStat{ZoneID: zB.ID, ConnectedClients: 5, RecordedAt: now.Add(-21 * time.Minute).In(time.Local)})
	db.Create(&models.ZoneStat{ZoneID: zD.ID, ConnectedClients: 50, RecordedAt: now.In(time.Local)})

	// Customers with time running.
	pkg := models.Package{ZoneID: zA.ID, Name: "1 Hour", Type: "hotspot", Price: 20, BillingCycle: "hourly", SpeedUploadKbps: 1, SpeedDownloadKbps: 1}
	db.Create(&pkg)
	for i, exp := range []time.Time{now.Add(time.Hour), now.Add(2 * time.Hour), now.Add(-time.Hour)} {
		st := "active"
		if i == 2 {
			st = "expired"
		}
		e := exp.In(time.Local)
		c := models.Customer{Name: "c", Phone: "2547000000" + string(rune('0'+i)), ZoneID: zA.ID, PackageID: pkg.ID, Type: "hotspot", Status: st, ExpiresAt: &e}
		if err := db.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Router actions owed to payers.
	failedPay := mkPay(t, db, zA.ID, "254711000017", 10, "failed", "x", eat(2026, 10, 21, 7, 0))
	mkAction := func(pay uint, status string, at time.Time) {
		pid := pay
		a := models.PendingRouterAction{ZoneID: zA.ID, PaymentID: &pid, Action: RouterActionWhitelistMAC, Status: status, NextAttemptAt: at.In(time.Local), CreatedAt: at.In(time.Local), UpdatedAt: at.In(time.Local)}
		if pay == p20.ID {
			a.ZoneID = zB.ID
		}
		if err := db.Create(&a).Error; err != nil {
			t.Fatal(err)
		}
	}
	mkAction(p20.ID, "pending", now.Add(-5*time.Minute))        // listed (and zone offline)
	mkAction(p50.ID, "pending", now.Add(-1*time.Minute))        // too fresh
	mkAction(p50.ID, "done", now.Add(-10*time.Minute))          // done
	mkAction(failedPay.ID, "pending", now.Add(-10*time.Minute)) // payment not completed
	mkAction(p50.ID, "failed", now.Add(-2*time.Hour))           // listed
	mkAction(p50.ID, "failed", now.Add(-30*time.Hour))          // too old

	live, err := BuildLiveOps(db, LiveScope{OrganizationID: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	m := live.Money
	if m.TodayAmount != 175 || m.TodayCount != 4 {
		t.Errorf("today = %v / %d, want 175 / 4", m.TodayAmount, m.TodayCount)
	}
	if m.Buyers != 3 {
		t.Errorf("buyers = %d, want 3", m.Buyers)
	}
	if m.YesterdaySameTime != 30 || m.YesterdaySameTimeCount != 1 || m.YesterdayFullDay != 70 {
		t.Errorf("yesterday = %+v", m)
	}
	if m.LastWeekSameTime != 60 || m.LastWeekSameTimeCount != 1 || m.LastWeekDay != "Thursday" {
		t.Errorf("last week = %+v", m)
	}

	f := live.Failures
	if f.Failed != 6 || f.Attempts != 10 || f.Pending != 1 || f.CustomerSideCount != 5 {
		t.Errorf("failures = %+v", f)
	}
	if math.Abs(f.Rate-0.6) > 1e-9 {
		t.Errorf("rate = %v", f.Rate)
	}
	got := map[string]int64{}
	for _, b := range f.Breakdown {
		got[b.Reason] = b.Count
		if (b.Reason == FailSystem) == b.CustomerSide {
			t.Errorf("customer_side wrong for %s", b.Reason)
		}
	}
	want := map[string]int64{FailCancelled: 2, FailNoResponse: 1, FailLowBalance: 1, FailWrongPIN: 1, FailSystem: 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("breakdown[%s] = %d, want %d", k, got[k], v)
		}
	}

	if live.Hourly[8].Amount != 50 || live.Hourly[13].Amount != 120 || live.Hourly[14].Amount != 5 || live.Hourly[13].Count != 2 {
		t.Errorf("hourly today wrong: 8=%v 13=%v 14=%v", live.Hourly[8], live.Hourly[13], live.Hourly[14])
	}
	if live.Hourly[10].Yesterday != 30 || live.Hourly[16].Yesterday != 40 {
		t.Errorf("hourly yesterday wrong: 10=%v 16=%v", live.Hourly[10], live.Hourly[16])
	}

	if live.Online.RouterReported != 7 || live.Online.RoutersReporting != 1 || live.Online.ActivePlans != 2 {
		t.Errorf("online = %+v", live.Online)
	}

	if len(live.Zones) != 3 {
		t.Fatalf("zones = %d, want 3 (deleted and other ISP excluded)", len(live.Zones))
	}
	if live.Zones[0].Name != "Bravo" || live.Zones[0].Status != "offline" || live.Zones[0].OutageSeconds != 1200 {
		t.Errorf("first zone should be offline Bravo: %+v", live.Zones[0])
	}
	if live.Zones[1].Name != "Alpha" || live.Zones[1].Status != "online" || live.Zones[1].TodayAmount != 155 || live.Zones[1].Clients == nil || *live.Zones[1].Clients != 7 {
		t.Errorf("Alpha wrong: %+v", live.Zones[1])
	}
	if live.Zones[2].Status != "never_seen" {
		t.Errorf("NeverSeen wrong: %+v", live.Zones[2])
	}

	if len(live.RecentPayments) != 10 {
		t.Fatalf("recent = %d", len(live.RecentPayments))
	}
	for _, p := range live.RecentPayments {
		if p.ZoneID == zC.ID || p.ZoneID == zD.ID {
			t.Errorf("recent payment from foreign/deleted zone: %+v", p)
		}
		if !strings.Contains(p.Phone, "*") {
			t.Errorf("phone not masked: %s", p.Phone)
		}
	}
	if live.RecentPayments[0].ID < live.RecentPayments[1].ID {
		t.Error("recent payments not newest first")
	}

	if len(live.PaidNotConnected) != 2 {
		t.Fatalf("paid-not-connected = %d, want 2: %+v", len(live.PaidNotConnected), live.PaidNotConnected)
	}
	// Gave-up actions first, then the longest waiting.
	if live.PaidNotConnected[0].Status != "failed" || live.PaidNotConnected[0].PaymentID != p50.ID {
		t.Errorf("stuck[0] = %+v", live.PaidNotConnected[0])
	}
	if s := live.PaidNotConnected[1]; s.PaymentID != p20.ID || !s.ZoneOffline || s.WaitingSecs != 300 || s.Amount != 20 {
		t.Errorf("stuck[1] = %+v", s)
	}

	// One zone only.
	only := zA.ID
	la, err := BuildLiveOps(db, LiveScope{OrganizationID: 1, ZoneID: &only}, now)
	if err != nil {
		t.Fatal(err)
	}
	if la.Money.TodayAmount != 155 || la.Failures.Failed != 4 || len(la.Zones) != 1 || len(la.PaidNotConnected) != 1 {
		t.Errorf("zone scope wrong: money=%+v failed=%d zones=%d stuck=%d", la.Money, la.Failures.Failed, len(la.Zones), len(la.PaidNotConnected))
	}

	// An ISP with no zones gets zeros, not an error.
	empty, err := BuildLiveOps(db, LiveScope{OrganizationID: 99}, now)
	if err != nil || empty.Money.TodayAmount != 0 || len(empty.Zones) != 0 {
		t.Errorf("empty org: %v %+v", err, empty)
	}
}

func TestEATDayBoundary(t *testing.T) {
	db := setupOpsDB(t)
	now := eat(2026, 10, 22, 0, 30)
	z := mkZone(t, db, 1, "Z", ptrT(now))
	mkPay(t, db, z.ID, "254711000001", 10, "completed", "", eat(2026, 10, 21, 23, 59)) // yesterday in EAT (20:59 UTC)
	mkPay(t, db, z.ID, "254711000002", 20, "completed", "", eat(2026, 10, 22, 0, 1))   // today in EAT (21:01 UTC the day before)
	live, err := BuildLiveOps(db, LiveScope{OrganizationID: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if live.Money.TodayAmount != 20 || live.Money.YesterdayFullDay != 10 || live.Hourly[0].Amount != 20 || live.Hourly[23].Yesterday != 10 {
		t.Errorf("EAT day boundary wrong: %+v hourly0=%+v hourly23=%+v", live.Money, live.Hourly[0], live.Hourly[23])
	}
}
