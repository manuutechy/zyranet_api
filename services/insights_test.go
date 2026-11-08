package services

import (
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
)

func TestBuildInsights(t *testing.T) {
	db := setupOpsDB(t)
	now := eat(2026, 11, 8, 18, 0)
	z := mkZone(t, db, 1, "Main", ptrT(now.Add(-2*time.Minute)))
	other := mkZone(t, db, 2, "Other ISP", nil)

	pkg := models.Package{Name: "1 Hour", Price: 10, Status: "active"}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	withPkg := func(p models.Payment) {
		db.Model(&models.Payment{}).Where("id = ?", p.ID).Update("package_id", pkg.ID)
	}

	// Before the range: 0711 already a buyer, so a returning buyer later.
	withPkg(mkPay(t, db, z.ID, "0711000001", 10, "completed", "", eat(2026, 10, 20, 9, 0)))
	// In range (2..8 Nov). Mon 2 Nov 22:15 and Sun 8 Nov 00:30 EAT.
	withPkg(mkPay(t, db, z.ID, "+254711000001", 10, "completed", "", eat(2026, 11, 2, 22, 15)))
	withPkg(mkPay(t, db, z.ID, "0722000002", 10, "completed", "", eat(2026, 11, 2, 22, 40)))
	mkPay(t, db, z.ID, "0722000002", 5, "completed", "", eat(2026, 11, 8, 0, 30))
	mkPay(t, db, z.ID, "0733000003", 10, "failed", "Payment cancelled. You closed the M-Pesa prompt before approving it.", eat(2026, 11, 3, 12, 0))
	mkPay(t, db, z.ID, "0733000003", 10, "failed", "daraja auth returned status 503", eat(2026, 11, 3, 12, 5))
	mkPay(t, db, z.ID, "0744000004", 10, "pending", "", eat(2026, 11, 4, 12, 0))
	// Another ISP: must never show.
	mkPay(t, db, other.ID, "0799000009", 999, "completed", "", eat(2026, 11, 3, 12, 0))
	// Previous period (26 Oct..1 Nov).
	mkPay(t, db, z.ID, "0755000005", 20, "completed", "", eat(2026, 10, 30, 10, 0))

	// Router readings: two on 7 Nov, none on other days.
	for _, s := range []models.ZoneStat{
		{ZoneID: z.ID, CPULoad: 10, MemoryUsedMB: 50, MemoryTotalMB: 100, ConnectedClients: 4, RecordedAt: eat(2026, 11, 7, 20, 0).In(time.Local)},
		{ZoneID: z.ID, CPULoad: 30, MemoryUsedMB: 70, MemoryTotalMB: 100, ConnectedClients: 8, RecordedAt: eat(2026, 11, 7, 21, 0).In(time.Local)},
	} {
		if err := db.Create(&s).Error; err != nil {
			t.Fatal(err)
		}
	}

	in, err := BuildInsights(db, InsightsScope{OrganizationID: 1}, eat(2026, 11, 2, 0, 0), eat(2026, 11, 8, 0, 0), now)
	if err != nil {
		t.Fatal(err)
	}
	s := in.Summary
	if in.Days != 7 || len(in.Daily) != 7 || in.PrevFrom != "2026-10-26" || in.PrevTo != "2026-11-01" {
		t.Fatalf("range wrong: days=%d daily=%d prev=%s..%s", in.Days, len(in.Daily), in.PrevFrom, in.PrevTo)
	}
	if s.Kes != 25 || s.Paid != 3 || s.Failed != 2 || s.Pending != 1 || s.Attempts != 6 {
		t.Fatalf("summary wrong: %+v", s)
	}
	if s.SuccessRate != 0.6 {
		t.Errorf("success rate = %v, want 0.6 (3 of 5 answered)", s.SuccessRate)
	}
	if s.Buyers != 2 || s.NewBuyers != 1 || s.ReturningBuyers != 1 {
		t.Errorf("buyers wrong (0711 and +254711 are the same person): %+v", s)
	}
	if s.ActiveDays != 2 || s.AvgTicket != 8.33 || s.KesPerBuyer != 12.5 {
		t.Errorf("averages wrong: %+v", s)
	}
	if in.Prev.Kes != 20 || in.Prev.Paid != 1 {
		t.Errorf("prev wrong: %+v", in.Prev)
	}
	if d := in.Daily[0]; d.Date != "2026-11-02" || d.Kes != 20 || d.Paid != 2 || d.Buyers != 2 || d.NewBuyers != 1 {
		t.Errorf("day 1 wrong: %+v", d)
	}
	if d := in.Daily[6]; d.Date != "2026-11-08" || d.Kes != 5 || d.NewBuyers != 0 {
		t.Errorf("last day wrong (00:30 EAT belongs to 8 Nov): %+v", d)
	}
	if in.Heat[0][22] != 2 || in.Heat[6][0] != 1 || in.HeatKes[0][22] != 20 {
		t.Errorf("heat wrong: mon22=%d sun00=%d", in.Heat[0][22], in.Heat[6][0])
	}
	if len(in.Failures) != 2 || in.Failures[0].Reason != FailCancelled || in.Failures[1].Reason != FailSystem || in.Failures[1].CustomerSide {
		t.Errorf("failures wrong: %+v", in.Failures)
	}
	if len(in.Packages) != 2 || in.Packages[0].Name != "1 Hour" || in.Packages[0].Count != 2 || in.Packages[1].Name != "No package recorded" {
		t.Errorf("packages wrong: %+v", in.Packages)
	}
	if len(in.Methods) != 1 || in.Methods[0].Label != "M-Pesa prompt" || in.Methods[0].Kes != 25 {
		t.Errorf("methods wrong: %+v", in.Methods)
	}
	if in.Loyalty[0].Buyers != 1 || in.Loyalty[1].Buyers != 1 || in.Loyalty[1].Kes != 15 {
		t.Errorf("loyalty wrong: %+v", in.Loyalty)
	}
	if in.FirstSale != "2026-10-20" || in.LastSale != "2026-11-08" {
		t.Errorf("first/last sale wrong: %s %s", in.FirstSale, in.LastSale)
	}

	if len(in.Routers) != 1 {
		t.Fatalf("routers: want 1 (own ISP only), got %d", len(in.Routers))
	}
	r := in.Routers[0]
	if r.Status != "online" || r.Bucket != "day" || r.ReportingBucket != 1 || r.TotalBuckets != 7 || r.PeakClients != 8 || r.CPUAvg != 20 {
		t.Errorf("router wrong: %+v", r)
	}
	p := r.Points[5]
	if p.At != "2026-11-07" || p.Readings != 2 || p.CPUAvg != 20 || p.CPUMax != 30 || p.MemPct != 60 || p.ClientsMax != 8 || p.ClientsAvg != 6 {
		t.Errorf("router point wrong: %+v", p)
	}
	if r.Points[0].Readings != 0 {
		t.Errorf("a day with no readings must stay empty, got %+v", r.Points[0])
	}

	// One zone, hourly buckets for a 1-day range.
	zid := z.ID
	one, err := BuildInsights(db, InsightsScope{OrganizationID: 1, ZoneID: &zid}, eat(2026, 11, 7, 0, 0), eat(2026, 11, 7, 0, 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if one.Routers[0].Bucket != "hour" || len(one.Routers[0].Points) != 24 || one.Routers[0].ReportingBucket != 2 {
		t.Errorf("hourly router wrong: %+v", one.Routers[0])
	}

	// Another ISP's zone id must give nothing.
	oid := other.ID
	x, _ := BuildInsights(db, InsightsScope{OrganizationID: 1, ZoneID: &oid}, eat(2026, 11, 2, 0, 0), eat(2026, 11, 8, 0, 0), now)
	if x.Summary.Paid != 0 || len(x.Routers) != 0 {
		t.Errorf("leaked another ISP: %+v", x.Summary)
	}
}
