package services

import (
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
)

func TestBuildGrowthWeeks(t *testing.T) {
	db := setupOpsDB(t)
	if err := db.AutoMigrate(&models.GrowthPlan{}); err != nil {
		t.Fatal(err)
	}
	z := mkZone(t, db, 1, "Maseno", nil)
	other := mkZone(t, db, 2, "Other ISP", nil)
	daily := models.Package{ZoneID: z.ID, Name: "Day", Type: "pppoe", Price: 30, BillingCycle: "daily", SpeedUploadKbps: 1, SpeedDownloadKbps: 1}
	monthly := models.Package{ZoneID: z.ID, Name: "Month", Type: "pppoe", Price: 900, BillingCycle: "monthly", SpeedUploadKbps: 1, SpeedDownloadKbps: 1}
	db.Create(&daily)
	db.Create(&monthly)

	pay := func(zone uint, phone string, amt float64, method string, pkg *models.Package, at time.Time) {
		p := models.Payment{ZoneID: zone, Phone: phone, Amount: amt, Method: method, Status: "completed", CreatedAt: at.In(time.Local), UpdatedAt: at.In(time.Local)}
		if pkg != nil {
			p.PackageID = &pkg.ID
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
	}
	// "Now" = Wednesday 11 Nov 2026 12:00 EAT; this week = Mon 9 – Sun 15 Nov.
	now := eat(2026, 11, 11, 12, 0)
	// A: monthly bought 20 Oct → covered last week and this week, paid nothing since.
	pay(z.ID, "0711000001", 900, "mpesa", &monthly, eat(2026, 10, 20, 9, 0))
	// B: daily, paid Mon and Tue this week (first ever on Mon: new).
	pay(z.ID, "254711000002", 30, "mpesa", &daily, eat(2026, 11, 9, 8, 0))
	pay(z.ID, "0711000002", 30, "mpesa", &daily, eat(2026, 11, 10, 8, 0))
	// C: paid last week, renewed with account credit this week (no takings).
	pay(z.ID, "0711000003", 30, "mpesa", &daily, eat(2026, 11, 3, 8, 0))
	pay(z.ID, "0711000003", 30, "credit", &daily, eat(2026, 11, 9, 20, 0))
	// Another ISP's money is never counted.
	pay(other.ID, "0711000009", 5000, "mpesa", &daily, eat(2026, 11, 10, 8, 0))

	rep, err := BuildGrowth(db, 1, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Weeks) != 2 || rep.Weeks[1].WeekStart != "2026-11-09" || !rep.Weeks[1].Current || rep.Weeks[1].Days != 3 {
		t.Fatalf("weeks: %+v", rep.Weeks)
	}
	cur, last := rep.Weeks[1], rep.Weeks[0]
	if cur.People != 3 || cur.PayingCustomers != 1 || cur.NewCustomers != 1 || cur.Takings != 60 {
		t.Errorf("this week: people %d paying %d new %d takings %v", cur.People, cur.PayingCustomers, cur.NewCustomers, cur.Takings)
	}
	if last.People != 2 || last.PayingCustomers != 1 || last.Takings != 30 || last.NewCustomers != 1 {
		t.Errorf("last week: people %d paying %d new %d takings %v", last.People, last.PayingCustomers, last.NewCustomers, last.Takings)
	}
	// Costs: 11.5% fees + (25,000 + 60×3)/30.4375 × 3 days.
	wantFixed := round2((25000 + 60*3) / GrowthDaysPerMonth * 3)
	if cur.Fees != round2(60*0.115) || cur.FixedCosts != wantFixed || cur.Profit != round2(60-cur.Fees-wantFixed) || cur.Reinvest != 0 {
		t.Errorf("costs: %+v", cur)
	}
	if cur.Installs != nil || rep.InstallsAvailable {
		t.Error("no install_jobs table: installs must be nil")
	}
	// No start date: the plan starts this week.
	if rep.StartDateSet || cur.Plan == nil || cur.Plan.Week != 1 || last.Plan != nil {
		t.Errorf("plan placement: set=%v cur=%+v last=%+v", rep.StartDateSet, cur.Plan, last.Plan)
	}

	// Saved plan starting last week; installs table appears.
	s := DefaultGrowthPlan()
	s.StartDate = "2026-11-04" // Wednesday → week of Mon 2 Nov
	if err := SaveGrowthPlan(db, 1, s, nil); err != nil {
		t.Fatal(err)
	}
	s.StartPeople = 210
	if err := SaveGrowthPlan(db, 1, s, nil); err != nil { // update, not a second row
		t.Fatal(err)
	}
	var n int64
	db.Model(&models.GrowthPlan{}).Count(&n)
	if n != 1 {
		t.Fatalf("growth_plans rows = %d", n)
	}
	db.Exec(`CREATE TABLE install_jobs (id INTEGER PRIMARY KEY, organization_id INTEGER, status TEXT, installed_at DATETIME, total_cost REAL)`)
	db.Exec(`INSERT INTO install_jobs (organization_id, status, installed_at, total_cost) VALUES (1,'live',?,1200),(1,'installed',?,800),(1,'cancelled',?,500),(2,'live',?,999)`,
		eat(2026, 11, 10, 9, 0).In(time.Local), eat(2026, 11, 10, 11, 0).In(time.Local), eat(2026, 11, 10, 12, 0).In(time.Local), eat(2026, 11, 10, 12, 0).In(time.Local))

	rep, err = BuildGrowth(db, 1, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	cur, last = rep.Weeks[1], rep.Weeks[0]
	if !rep.StartDateSet || last.Plan == nil || last.Plan.Week != 1 || last.Plan.People != 210 || cur.Plan.Week != 2 {
		t.Fatalf("plan weeks: %+v / %+v", last.Plan, cur.Plan)
	}
	if *cur.PeopleVsPlan != 3-cur.Plan.People || *cur.NewCustomersVsPlan != 1-last.Plan.NewPeople {
		t.Errorf("vs plan: %d %d", *cur.PeopleVsPlan, *cur.NewCustomersVsPlan)
	}
	if !rep.InstallsAvailable || cur.Installs == nil || *cur.Installs != 2 || *cur.InstallCost != 2000 || *cur.CostPerInstall != 1000 || *last.Installs != 0 {
		t.Errorf("installs: %+v %+v", cur.Installs, cur.InstallCost)
	}

	// A broken install_jobs (missing column) never fails the report.
	db.Exec(`DROP TABLE install_jobs`)
	db.Exec(`CREATE TABLE install_jobs (id INTEGER PRIMARY KEY, status TEXT)`)
	if rep, err = BuildGrowth(db, 1, 2, now); err != nil || rep.Weeks[1].Installs != nil {
		t.Errorf("broken table: %v", err)
	}
	if err := ValidateGrowthPlan(GrowthPlanSettings{PricePerDay: 30, CostPerNewPerson: 0}); err == nil {
		t.Error("zero cost per new home must be refused")
	}
	if err := ValidateGrowthPlan(GrowthPlanSettings{StartDate: "11/04/2026", PricePerDay: 30, CostPerNewPerson: 1000}); err == nil {
		t.Error("bad date must be refused")
	}
}

func TestEATWeekStart(t *testing.T) {
	// Sunday 23:30 EAT belongs to the week that started Monday.
	if got := EATWeekStart(eat(2026, 11, 15, 23, 30)); got.Format("2006-01-02") != "2026-11-09" {
		t.Errorf("Sunday night: %s", got)
	}
	// Monday 00:10 EAT (Sunday 21:10 UTC) starts a new week.
	if got := EATWeekStart(eat(2026, 11, 16, 0, 10).UTC()); got.Format("2006-01-02") != "2026-11-16" {
		t.Errorf("Monday early: %s", got)
	}
}
