package services

import (
	"errors"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

type installFixture struct {
	db      *gorm.DB
	org     models.Organization
	zone    models.Zone
	pkg     models.Package
	crew    models.InstallCrew
	crew2   models.InstallCrew
	owner   InstallActor
	ontItem models.StockItem
	cable   models.StockItem
}

func setupInstalls(t *testing.T) *installFixture {
	t.Helper()
	db := setupTestDB(t)
	if err := MigrateInstalls(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	f := &installFixture{db: db}
	f.org = models.Organization{Name: "Maseno Fibre", Slug: "maseno-" + t.Name(), Status: "active"}
	db.Create(&f.org)
	f.zone = models.Zone{Name: "Maseno", Location: "Maseno", RouterName: "rb5009", RouterIP: "10.0.0.1", OrganizationID: f.org.ID}
	db.Create(&f.zone)
	f.pkg = models.Package{ZoneID: f.zone.ID, Name: "Home daily", Type: "pppoe", Price: 30, BillingCycle: "daily", Status: "active",
		SpeedUploadKbps: 5000, SpeedDownloadKbps: 10000}
	db.Create(&f.pkg)
	f.crew = models.InstallCrew{OrganizationID: f.org.ID, Name: "Crew A", Active: true}
	db.Create(&f.crew)
	f.crew2 = models.InstallCrew{OrganizationID: f.org.ID, Name: "Crew B", Active: true}
	db.Create(&f.crew2)
	f.owner = InstallActor{OrgID: f.org.ID, UserID: 1, UserName: "Owner", Role: "super_admin"}

	ont, err := CreateStockItem(db, f.owner, StockItemInput{Name: "ONT HG8145", Unit: "pcs", TracksSerials: true, ReorderLevel: 5})
	if err != nil {
		t.Fatal(err)
	}
	f.ontItem = *ont
	cable, err := CreateStockItem(db, f.owner, StockItemInput{Name: "Drop cable", Unit: "metres", ReorderLevel: 100})
	if err != nil {
		t.Fatal(err)
	}
	f.cable = *cable
	if _, err := PurchaseStock(db, f.owner, ont.ID, PurchaseInput{UnitCost: 1850, Serials: []string{"hwtc0001", "HWTC0002", "HWTC0003"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := PurchaseStock(db, f.owner, cable.ID, PurchaseInput{Quantity: 100, UnitCost: 12.5}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *installFixture) newAssignedJob(t *testing.T, crew uint) *models.InstallJob {
	t.Helper()
	z, p := f.zone.ID, f.pkg.ID
	job, err := CreateInstallJob(f.db, f.owner, InstallJobInput{CustomerName: "Jane Achieng", Phone: "0712345678", Location: "Near Maseno market",
		ZoneID: &z, PlanPackageID: &p}, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssignInstallJob(f.db, f.owner, job.ID, crew, nil); err != nil {
		t.Fatal(err)
	}
	return job
}

func onHand(db *gorm.DB, id uint) float64 {
	var it models.StockItem
	db.First(&it, id)
	return it.OnHand
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var le *LedgerError
	if !errors.As(err, &le) || le.Status != status {
		t.Fatalf("want error status %d, got %v", status, err)
	}
}

func TestInstallTransitions(t *testing.T) {
	ok := [][2]string{{"requested", "scheduled"}, {"requested", "assigned"}, {"scheduled", "assigned"}, {"assigned", "installed"},
		{"installed", "live"}, {"requested", "cancelled"}, {"assigned", "cancelled"}, {"assigned", "scheduled"}}
	bad := [][2]string{{"requested", "installed"}, {"requested", "live"}, {"installed", "cancelled"}, {"live", "installed"},
		{"cancelled", "requested"}, {"cancelled", "assigned"}, {"live", "cancelled"}, {"scheduled", "live"}}
	for _, p := range ok {
		if !CanInstallTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be allowed", p[0], p[1])
		}
	}
	for _, p := range bad {
		if CanInstallTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be refused", p[0], p[1])
		}
	}
}

func TestCostMaths(t *testing.T) {
	if got := lineCents(toHundredths(35.5), toCents(12.5)); got != 44375 { // 35.5 m × 12.50 = 443.75
		t.Fatalf("lineCents = %d", got)
	}
	if got := lineCents(toHundredths(0.33), toCents(10)); got != 330 {
		t.Fatalf("lineCents small = %d", got)
	}
	// 10 @ 100 + 10 @ 200 → avg 150
	if got := weightedAverageCents(1000, 10000, 1000, 20000); got != 15000 {
		t.Fatalf("avg = %d", got)
	}
	// nothing on hand: the new price
	if got := weightedAverageCents(0, 9999, 500, 1234); got != 1234 {
		t.Fatalf("avg empty = %d", got)
	}
	if FormatKES(123456789) != "1,234,567.89" || FormatKES(100000) != "1,000" || FormatKES(-5050) != "-50.50" {
		t.Fatalf("FormatKES: %s %s %s", FormatKES(123456789), FormatKES(100000), FormatKES(-5050))
	}
	// 0.1 + 0.2 style drift can't happen: 300 installs at 1,016.35 total exactly.
	var sum int64
	for i := 0; i < 300; i++ {
		sum += toCents(1016.35)
	}
	if sum != 30490500 {
		t.Fatalf("sum = %d", sum)
	}
}

func TestStockNeverNegative(t *testing.T) {
	f := setupInstalls(t)
	if onHand(f.db, f.cable.ID) != 100 || onHand(f.db, f.ontItem.ID) != 3 {
		t.Fatalf("start stock wrong: cable %v ont %v", onHand(f.db, f.cable.ID), onHand(f.db, f.ontItem.ID))
	}
	// Write off more than there is: refused, nothing changes.
	_, err := ChangeStock(f.db, f.owner, f.cable.ID, models.MoveWriteOff, StockChangeInput{Quantity: 100.01, Note: "flood"})
	wantStatus(t, err, 409)
	_, err = ChangeStock(f.db, f.owner, f.cable.ID, models.MoveAdjustment, StockChangeInput{Quantity: -150, Note: "count"})
	wantStatus(t, err, 409)
	if onHand(f.db, f.cable.ID) != 100 {
		t.Fatalf("on hand changed after refusal: %v", onHand(f.db, f.cable.ID))
	}
	// Exactly all of it is fine; then nothing more.
	if _, err := ChangeStock(f.db, f.owner, f.cable.ID, models.MoveAdjustment, StockChangeInput{Quantity: -100, Note: "count"}); err != nil {
		t.Fatal(err)
	}
	_, err = ChangeStock(f.db, f.owner, f.cable.ID, models.MoveWriteOff, StockChangeInput{Quantity: 0.01, Note: "x"})
	wantStatus(t, err, 409)
	// The guard is the UPDATE itself: even a direct takeOut with a stale
	// struct can't push it below zero.
	item := f.cable
	item.OnHand = 999
	if err := takeOut(f.db, &item, 100); err == nil {
		t.Fatal("takeOut below zero should fail")
	}
	for _, id := range []uint{f.cable.ID, f.ontItem.ID} {
		if got, want := StockOnHandFromMoves(f.db, id), onHand(f.db, id); got != want {
			t.Fatalf("item %d: ledger sum %v != on_hand %v", id, got, want)
		}
	}
	// Serial items move only by serial.
	_, err = ChangeStock(f.db, f.owner, f.ontItem.ID, models.MoveWriteOff, StockChangeInput{Quantity: 1, Note: "x"})
	wantStatus(t, err, 422)
	// Pieces are whole.
	_, err = PurchaseStock(f.db, f.owner, f.ontItem.ID, PurchaseInput{Quantity: 2, UnitCost: 1, Serials: []string{"A1"}})
	wantStatus(t, err, 422)
	_, err = PurchaseStock(f.db, f.owner, f.ontItem.ID, PurchaseInput{UnitCost: 1, Serials: []string{"HWTC0001"}})
	wantStatus(t, err, 409) // serial already listed (case-insensitive)
}

func TestInstallEndToEnd(t *testing.T) {
	f := setupInstalls(t)
	job := f.newAssignedJob(t, f.crew.ID)
	installer := InstallActor{OrgID: f.org.ID, UserID: 7, Role: "installer", CrewID: &f.crew.ID}
	now := time.Now()

	got, cust, err := MarkInstalled(f.db, installer, job.ID, MarkInstalledInput{OntSerial: " hwtc0002 ", LabourCost: 500,
		Materials: []MaterialLine{{StockItemID: f.cable.ID, Quantity: 35.5}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	// 1,850 ONT + 35.5 m × 12.50 (443.75) = 2,293.75 materials; + 500 labour.
	if got.MaterialsCost != 2293.75 || got.LabourCost != 500 || got.TotalCost != 2793.75 {
		t.Fatalf("costs: materials %v labour %v total %v", got.MaterialsCost, got.LabourCost, got.TotalCost)
	}
	var stored models.InstallJob
	f.db.First(&stored, job.ID)
	if stored.Status != models.InstallInstalled || stored.TotalCost != 2793.75 || stored.InstalledAt == nil || stored.CustomerID == nil || *stored.OntSerial != "HWTC0002" {
		t.Fatalf("stored job wrong: %+v", stored)
	}
	// The home: PPPoE, on the job's zone and plan, created OFF (expired now).
	if cust.Type != "pppoe" || cust.ZoneID != f.zone.ID || cust.PackageID != f.pkg.ID || cust.Status != "active" ||
		cust.PPPoEUsername == nil || len(*cust.PPPoEUsername) != 8 || cust.PPPoEPassword == nil || len(*cust.PPPoEPassword) != 12 {
		t.Fatalf("customer wrong: %+v", cust)
	}
	if cust.ExpiresAt == nil || cust.ExpiresAt.After(now.Add(time.Second)) {
		t.Fatalf("home should start expired, expires_at=%v", cust.ExpiresAt)
	}
	var ont models.OntUnit
	f.db.Where("serial = ?", "HWTC0002").First(&ont)
	if ont.Status != models.OntInstalled || ont.CustomerID == nil || *ont.CustomerID != cust.ID {
		t.Fatalf("ont wrong: %+v", ont)
	}
	if onHand(f.db, f.ontItem.ID) != 2 || onHand(f.db, f.cable.ID) != 64.5 {
		t.Fatalf("stock after install: ont %v cable %v", onHand(f.db, f.ontItem.ID), onHand(f.db, f.cable.ID))
	}
	// A double tap can't install twice or take stock twice.
	_, _, err = MarkInstalled(f.db, installer, job.ID, MarkInstalledInput{OntSerial: "HWTC0003", LabourCost: 500}, now)
	wantStatus(t, err, 409)
	if onHand(f.db, f.ontItem.ID) != 2 {
		t.Fatal("second install took stock")
	}
	// An installed job can't be cancelled.
	_, err = CancelInstallJob(f.db, f.owner, job.ID, "changed mind")
	wantStatus(t, err, 409)

	// Leftover cable back from the job lowers its cost.
	if _, err := ChangeStock(f.db, f.owner, f.cable.ID, models.MoveReturned, StockChangeInput{Quantity: 5.5, JobID: &job.ID, Note: "leftover"}); err != nil {
		t.Fatal(err)
	}
	f.db.First(&stored, job.ID)
	if stored.MaterialsCost != 2225 || stored.TotalCost != 2725 { // 2,293.75 − 68.75
		t.Fatalf("after return: materials %v total %v", stored.MaterialsCost, stored.TotalCost)
	}
	_, err = ChangeStock(f.db, f.owner, f.cable.ID, models.MoveReturned, StockChangeInput{Quantity: 31, JobID: &job.ID, Note: "too much"})
	wantStatus(t, err, 422)

	// Not live until a real payment; free credit doesn't count.
	if n, _ := MarkInstallsLive(f.db, nil); n != 0 {
		t.Fatalf("went live with no payment")
	}
	f.db.Create(&models.Payment{CustomerID: &cust.ID, ZoneID: f.zone.ID, Phone: cust.Phone, Amount: 30, Method: "credit", Status: "completed"})
	f.db.Create(&models.Payment{CustomerID: &cust.ID, ZoneID: f.zone.ID, Phone: cust.Phone, Amount: 30, Method: "mpesa", Status: "pending"})
	if n, _ := MarkInstallsLive(f.db, nil); n != 0 {
		t.Fatalf("went live on credit/pending payment")
	}
	f.db.Create(&models.Payment{CustomerID: &cust.ID, ZoneID: f.zone.ID, Phone: cust.Phone, Amount: 30, Method: "mpesa", Status: "completed"})
	if n, err := MarkInstallsLive(f.db, nil); n != 1 || err != nil {
		t.Fatalf("live: n=%d err=%v", n, err)
	}
	if n, _ := MarkInstallsLive(f.db, nil); n != 0 {
		t.Fatalf("went live twice")
	}
	f.db.First(&stored, job.ID)
	if stored.Status != models.InstallLive || stored.LiveAt == nil {
		t.Fatalf("not live: %+v", stored)
	}
	var events []models.InstallJobEvent
	f.db.Where("job_id = ?", job.ID).Order("id").Find(&events)
	want := []string{"created", "assigned", "installed", "live"}
	if len(events) != len(want) {
		t.Fatalf("events = %d", len(events))
	}
	for i, e := range events {
		if e.Action != want[i] {
			t.Fatalf("event %d = %s, want %s", i, e.Action, want[i])
		}
	}
	for _, id := range []uint{f.cable.ID, f.ontItem.ID} {
		if StockOnHandFromMoves(f.db, id) != onHand(f.db, id) {
			t.Fatalf("ledger and on hand disagree for %d", id)
		}
	}
}

func TestInstallRollsBackWhenShort(t *testing.T) {
	f := setupInstalls(t)
	job := f.newAssignedJob(t, f.crew.ID)
	_, _, err := MarkInstalled(f.db, f.owner, job.ID, MarkInstalledInput{OntSerial: "HWTC0001", LabourCost: 400,
		Materials: []MaterialLine{{StockItemID: f.cable.ID, Quantity: 250}}}, time.Now())
	wantStatus(t, err, 409)
	var stored models.InstallJob
	f.db.First(&stored, job.ID)
	var ont models.OntUnit
	f.db.Where("serial = ?", "HWTC0001").First(&ont)
	var custs, moves int64
	f.db.Model(&models.Customer{}).Count(&custs)
	f.db.Model(&models.StockMove{}).Where("type = ?", models.MoveUsed).Count(&moves)
	if stored.Status != models.InstallAssigned || ont.Status != models.OntInStock || onHand(f.db, f.ontItem.ID) != 3 ||
		onHand(f.db, f.cable.ID) != 100 || custs != 0 || moves != 0 {
		t.Fatalf("partial install left behind: job %s ont %s onts %v cable %v customers %d moves %d",
			stored.Status, ont.Status, onHand(f.db, f.ontItem.ID), onHand(f.db, f.cable.ID), custs, moves)
	}
	// Unknown / used serial.
	_, _, err = MarkInstalled(f.db, f.owner, job.ID, MarkInstalledInput{OntSerial: "NOPE"}, time.Now())
	wantStatus(t, err, 422)
	// Serial item as a material line.
	_, _, err = MarkInstalled(f.db, f.owner, job.ID, MarkInstalledInput{OntSerial: "HWTC0001",
		Materials: []MaterialLine{{StockItemID: f.ontItem.ID, Quantity: 1}}}, time.Now())
	wantStatus(t, err, 422)
	// Labour typo guard.
	_, _, err = MarkInstalled(f.db, f.owner, job.ID, MarkInstalledInput{OntSerial: "HWTC0001", LabourCost: 250000}, time.Now())
	wantStatus(t, err, 422)
}

func TestInstallScopes(t *testing.T) {
	f := setupInstalls(t)
	job := f.newAssignedJob(t, f.crew.ID)
	other := InstallActor{OrgID: f.org.ID, UserID: 8, Role: "installer", CrewID: &f.crew2.ID}
	noCrew := InstallActor{OrgID: f.org.ID, UserID: 9, Role: "installer"}
	_, _, err := MarkInstalled(f.db, other, job.ID, MarkInstalledInput{OntSerial: "HWTC0001"}, time.Now())
	if !errors.Is(err, ErrInstallJobNotFound) {
		t.Fatalf("other crew should not find the job: %v", err)
	}
	var n int64
	InstallJobScope(f.db.Model(&models.InstallJob{}), noCrew).Count(&n)
	if n != 0 {
		t.Fatal("installer with no crew sees jobs")
	}
	InstallJobScope(f.db.Model(&models.InstallJob{}), other).Count(&n)
	if n != 0 {
		t.Fatal("other crew sees jobs")
	}
	mine := InstallActor{OrgID: f.org.ID, Role: "installer", CrewID: &f.crew.ID}
	InstallJobScope(f.db.Model(&models.InstallJob{}), mine).Count(&n)
	if n != 1 {
		t.Fatal("own crew should see its job")
	}
	// Installers can't create, assign or cancel.
	_, err = CreateInstallJob(f.db, mine, InstallJobInput{CustomerName: "X", Phone: "0700000000"}, time.Now())
	wantStatus(t, err, 403)
	_, err = CancelInstallJob(f.db, mine, job.ID, "no")
	wantStatus(t, err, 403)
	// Another ISP sees nothing.
	stranger := InstallActor{OrgID: f.org.ID + 99, Role: "super_admin"}
	_, err = CancelInstallJob(f.db, stranger, job.ID, "no")
	if !errors.Is(err, ErrInstallJobNotFound) {
		t.Fatalf("other ISP reached the job: %v", err)
	}
	_, err = PurchaseStock(f.db, stranger, f.cable.ID, PurchaseInput{Quantity: 1, UnitCost: 1})
	wantStatus(t, err, 404)
	// Zone manager of another zone can't see it; office roles only buy stock as owner.
	z2 := models.Zone{Name: "Luanda", Location: "Luanda", RouterName: "r", RouterIP: "10.0.0.2", OrganizationID: f.org.ID}
	f.db.Create(&z2)
	zm := InstallActor{OrgID: f.org.ID, Role: "zone_manager", ZoneID: &z2.ID}
	InstallJobScope(f.db.Model(&models.InstallJob{}), zm).Count(&n)
	if n != 0 {
		t.Fatal("zone manager sees another zone's job")
	}
	_, err = PurchaseStock(f.db, zm, f.cable.ID, PurchaseInput{Quantity: 1, UnitCost: 1})
	wantStatus(t, err, 403)
	// A zone manager's new request is pinned to their zone.
	j2, err := CreateInstallJob(f.db, zm, InstallJobInput{CustomerName: "Otieno", Phone: "0711111111", ZoneID: &f.zone.ID}, time.Now())
	if err != nil || j2.ZoneID == nil || *j2.ZoneID != z2.ID {
		t.Fatalf("zone manager job zone: %v %v", err, j2)
	}
	// Plans must belong to the job's zone.
	_, err = CreateInstallJob(f.db, f.owner, InstallJobInput{CustomerName: "A", Phone: "0722222222", ZoneID: &z2.ID, PlanPackageID: &f.pkg.ID}, time.Now())
	wantStatus(t, err, 422)
}

func TestTransitionsEnforced(t *testing.T) {
	f := setupInstalls(t)
	z, p := f.zone.ID, f.pkg.ID
	job, _ := CreateInstallJob(f.db, f.owner, InstallJobInput{CustomerName: "Jane", Phone: "0712345678", ZoneID: &z, PlanPackageID: &p}, time.Now())
	// Can't install before a crew.
	_, _, err := MarkInstalled(f.db, f.owner, job.ID, MarkInstalledInput{OntSerial: "HWTC0001"}, time.Now())
	wantStatus(t, err, 409)
	when := time.Now().Add(24 * time.Hour)
	j, err := ScheduleInstallJob(f.db, f.owner, job.ID, when)
	if err != nil || j.Status != models.InstallScheduled {
		t.Fatalf("schedule: %v %v", err, j)
	}
	j, _ = AssignInstallJob(f.db, f.owner, job.ID, f.crew.ID, nil)
	// Rescheduling an assigned job keeps it assigned.
	j, err = ScheduleInstallJob(f.db, f.owner, job.ID, when.Add(time.Hour))
	if err != nil || j.Status != models.InstallAssigned {
		t.Fatalf("reschedule: %v %v", err, j)
	}
	_, err = CancelInstallJob(f.db, f.owner, job.ID, "")
	wantStatus(t, err, 422)
	if _, err = CancelInstallJob(f.db, f.owner, job.ID, "Customer moved away"); err != nil {
		t.Fatal(err)
	}
	_, err = AssignInstallJob(f.db, f.owner, job.ID, f.crew.ID, nil)
	wantStatus(t, err, 409)
	_, err = ScheduleInstallJob(f.db, f.owner, job.ID, when)
	wantStatus(t, err, 409)
	// Inactive crew can't take jobs.
	f.db.Model(&f.crew2).Update("active", false)
	j3, _ := CreateInstallJob(f.db, f.owner, InstallJobInput{CustomerName: "B", Phone: "0733333333"}, time.Now())
	_, err = AssignInstallJob(f.db, f.owner, j3.ID, f.crew2.ID, nil)
	wantStatus(t, err, 422)
	// No zone/plan yet: crew can't finish it.
	j3, _ = AssignInstallJob(f.db, f.owner, j3.ID, f.crew.ID, nil)
	_, _, err = MarkInstalled(f.db, f.owner, j3.ID, MarkInstalledInput{OntSerial: "HWTC0001"}, time.Now())
	wantStatus(t, err, 422)
}

func TestFreeFirstDayAndOntActions(t *testing.T) {
	f := setupInstalls(t)
	f.db.Create(&models.InstallSetting{OrganizationID: f.org.ID, FreeFirstDay: true})
	job := f.newAssignedJob(t, f.crew.ID)
	now := time.Now()
	_, cust, err := MarkInstalled(f.db, f.owner, job.ID, MarkInstalledInput{OntSerial: "HWTC0001"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if d := cust.ExpiresAt.Sub(now); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("free first day expiry off by %v", d)
	}
	var ont models.OntUnit
	f.db.Where("serial = ?", "HWTC0001").First(&ont)
	_, err = OntAction(f.db, f.owner, ont.ID, "faulty", "")
	wantStatus(t, err, 409) // installed: must be taken back first
	if _, err := OntAction(f.db, f.owner, ont.ID, "take_back", "customer left"); err != nil {
		t.Fatal(err)
	}
	if onHand(f.db, f.ontItem.ID) != 3 {
		t.Fatalf("take back should add to on hand: %v", onHand(f.db, f.ontItem.ID))
	}
	if _, err := OntAction(f.db, f.owner, ont.ID, "faulty", "dead PON light"); err != nil {
		t.Fatal(err)
	}
	if onHand(f.db, f.ontItem.ID) != 2 || StockOnHandFromMoves(f.db, f.ontItem.ID) != 2 {
		t.Fatalf("after write-off: %v", onHand(f.db, f.ontItem.ID))
	}
}

func TestInstallSummaryWeeks(t *testing.T) {
	f := setupInstalls(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) // Wed
	thisMon := WeekStartEAT(now)
	lastWeek := thisMon.AddDate(0, 0, -5) // Wed of last week
	mk := func(at time.Time, labour, mat float64, status string, crew uint) {
		cid := crew
		inst := at
		f.db.Create(&models.InstallJob{OrganizationID: f.org.ID, CustomerName: "x", Phone: "254700000000", Status: status,
			CrewID: &cid, LabourCost: labour, MaterialsCost: mat, TotalCost: labour + mat, RequestedAt: at.Add(-72 * time.Hour), InstalledAt: &inst})
	}
	mk(lastWeek, 500, 1500, models.InstallLive, f.crew.ID)
	mk(lastWeek, 500, 1600, models.InstallInstalled, f.crew.ID)
	mk(thisMon.Add(30*time.Hour), 400, 1400, models.InstallInstalled, f.crew2.ID)
	f.db.Create(&models.InstallJob{OrganizationID: f.org.ID, CustomerName: "y", Phone: "254700000001", Status: models.InstallRequested, RequestedAt: now.Add(-96 * time.Hour)})

	s, err := BuildInstallSummary(f.db, f.owner, thisMon.AddDate(0, 0, -14), WeekStartEAT(now).AddDate(0, 0, 6), now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Installed != 3 || s.TotalCost != 5900 || s.AvgCostPerInstall != 1966.67 {
		t.Fatalf("totals: %d %v %v", s.Installed, s.TotalCost, s.AvgCostPerInstall)
	}
	if s.LastWeek.Installed != 2 || s.LastWeek.AvgCostPerHome != 2050 || s.ThisWeek.Installed != 1 || s.ThisWeek.AvgCostPerHome != 1800 {
		t.Fatalf("weeks: last %+v this %+v", s.LastWeek, s.ThisWeek)
	}
	if s.PayingOfInstalled != 1 || s.CostPerPayingHome != 5900 {
		t.Fatalf("paying: %d %v", s.PayingOfInstalled, s.CostPerPayingHome)
	}
	if s.AvgDaysToInstall != 3 || s.Backlog.Requested != 1 || s.Backlog.WaitingPayment != 2 || s.Backlog.OldestWaitDays != 4 {
		t.Fatalf("days/backlog: %v %+v", s.AvgDaysToInstall, s.Backlog)
	}
	if len(s.Weeks) != 3 {
		t.Fatalf("weeks len %d", len(s.Weeks))
	}
	if len(s.Crews) != 2 || s.Crews[0].Installed != 2 || s.Crews[0].AvgCostPerInstall != 2050 {
		t.Fatalf("crews: %+v", s.Crews)
	}
}
