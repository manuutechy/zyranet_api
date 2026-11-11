package services

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Growth report: the ISP's real numbers week by week (EAT, Monday–Sunday)
// next to the owner's growth plan (SimulateGrowthPlan).
//
// Definitions (all counted from completed payments; a "person" is one phone
// number, normalised to 2547…, the same way the insights report counts buyers):
//   - People: everyone whose paid time overlaps the week, plus anyone who paid
//     money in the week. Paid time is rebuilt from each completed payment's
//     package (time limit, else hourly/daily/weekly/monthly), stacked the way
//     the API stacks renewals: a renewal bought before expiry starts when the
//     current time runs out.
//   - New customers: people whose first ever completed payment is in the week.
//   - Paying customers: people who paid money (not account credit) in the week.
//   - Takings: completed payments in the week, excluding method "credit"
//     (spending a balance that was already paid in, or given as compensation).
//   - Costs are ESTIMATES from the plan settings: fees % of takings, plus fixed
//     (FixedMonthly + CostPerPersonMonthly × people) prorated by days.
//   - The current week counts only the EAT days so far (today included), so
//     "per day" figures compare fairly with the plan.

// LoadGrowthPlan reads the ISP's plan (growth_plans); no row = the owner's
// defaults. startSet says whether a start date was saved.
func LoadGrowthPlan(db *gorm.DB, orgID uint) (GrowthPlanSettings, bool) {
	s := DefaultGrowthPlan()
	var rows []models.GrowthPlan
	db.Where("organization_id = ?", orgID).Limit(1).Find(&rows)
	if len(rows) == 0 {
		return s, false
	}
	row := rows[0]
	s = GrowthPlanSettings{
		StartDate:            row.StartDate,
		StartPeople:          row.StartPeople,
		PricePerDay:          row.PricePerDay,
		FeePercent:           row.FeePercent,
		FixedMonthly:         row.FixedMonthly,
		CostPerPersonMonthly: row.CostPerPersonMonthly,
		ReinvestPercent:      row.ReinvestPercent,
		CostPerNewPerson:     row.CostPerNewPerson,
		MarketSize:           row.MarketSize,
	}
	if _, err := time.Parse("2006-01-02", s.StartDate); err != nil {
		s.StartDate = ""
	}
	return s, s.StartDate != ""
}

// ValidateGrowthPlan checks owner input before it is saved.
func ValidateGrowthPlan(s GrowthPlanSettings) error {
	if s.StartDate != "" {
		if _, err := time.Parse("2006-01-02", s.StartDate); err != nil {
			return fmt.Errorf("start date must be YYYY-MM-DD")
		}
	}
	switch {
	case s.StartPeople < 0 || s.StartPeople > 1000000:
		return fmt.Errorf("starting homes must be between 0 and 1,000,000")
	case s.PricePerDay <= 0 || s.PricePerDay > 100000:
		return fmt.Errorf("price per day must be more than 0")
	case s.FeePercent < 0 || s.FeePercent >= 100:
		return fmt.Errorf("fees must be between 0 and 99%%")
	case s.FixedMonthly < 0 || s.FixedMonthly > 1e9:
		return fmt.Errorf("fixed monthly costs can't be negative")
	case s.CostPerPersonMonthly < 0 || s.CostPerPersonMonthly > 1e6:
		return fmt.Errorf("cost per home can't be negative")
	case s.ReinvestPercent < 0 || s.ReinvestPercent > 100:
		return fmt.Errorf("reinvest must be between 0 and 100%%")
	case s.CostPerNewPerson <= 0 || s.CostPerNewPerson > 1e7:
		return fmt.Errorf("cost to connect a new home must be more than 0")
	case s.MarketSize < 0 || s.MarketSize > 10000000:
		return fmt.Errorf("market size must be between 0 and 10,000,000")
	}
	return nil
}

// SaveGrowthPlan stores the ISP's plan (one row per ISP).
func SaveGrowthPlan(db *gorm.DB, orgID uint, s GrowthPlanSettings, by *uint) error {
	row := models.GrowthPlan{
		OrganizationID:       orgID,
		StartDate:            s.StartDate,
		StartPeople:          s.StartPeople,
		PricePerDay:          s.PricePerDay,
		FeePercent:           s.FeePercent,
		FixedMonthly:         s.FixedMonthly,
		CostPerPersonMonthly: s.CostPerPersonMonthly,
		ReinvestPercent:      s.ReinvestPercent,
		CostPerNewPerson:     s.CostPerNewPerson,
		MarketSize:           s.MarketSize,
		UpdatedBy:            by,
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var cur models.GrowthPlan
		err := tx.Where("organization_id = ?", orgID).First(&cur).Error
		if err == gorm.ErrRecordNotFound {
			return tx.Create(&row).Error
		}
		if err != nil {
			return err
		}
		return tx.Model(&models.GrowthPlan{}).Where("id = ?", cur.ID).Select("*").Omit("id", "organization_id", "created_at").Updates(&row).Error
	})
}

// GrowthWeek is one EAT week of real figures, with the plan beside it.
type GrowthWeek struct {
	WeekStart       string   `json:"week_start"` // Monday, YYYY-MM-DD
	WeekEnd         string   `json:"week_end"`   // Sunday
	Current         bool     `json:"current"`
	Days            int      `json:"days"` // days counted (7, or days so far this week)
	People          int      `json:"people"`
	NewCustomers    int      `json:"new_customers"`
	PayingCustomers int      `json:"paying_customers"`
	PayingPct       *float64 `json:"paying_pct"` // paying / people × 100; nil when no people
	Takings         float64  `json:"takings"`
	TakingsPerDay   float64  `json:"takings_per_day"`
	Fees            float64  `json:"fees"`
	FixedCosts      float64  `json:"fixed_costs"`
	Costs           float64  `json:"costs"`
	Profit          float64  `json:"profit"`
	ProfitPerDay    float64  `json:"profit_per_day"`
	Reinvest        float64  `json:"reinvest"` // reinvest % of profit (0 when a loss)
	// Installs come from install_jobs when that table exists; nil otherwise.
	Installs       *int     `json:"installs"`
	InstallCost    *float64 `json:"install_cost"`
	CostPerInstall *float64 `json:"cost_per_install"`
	// Plan for the same week (nil before the plan starts).
	Plan               *PlanWeek `json:"plan"`
	PeopleVsPlan       *int      `json:"people_vs_plan"`         // actual − plan
	ProfitPerDayVsPlan *float64  `json:"profit_per_day_vs_plan"` // actual − plan
	NewCustomersVsPlan *int      `json:"new_customers_vs_plan"`  // actual new − plan homes added that week
}

// GrowthReport is GET /reports/growth.
type GrowthReport struct {
	Settings          GrowthPlanSettings `json:"settings"`
	StartDateSet      bool               `json:"start_date_set"`
	PlanStartWeek     string             `json:"plan_start_week"` // Monday of plan week 1
	DaysPerMonth      float64            `json:"days_per_month"`
	InstallsAvailable bool               `json:"installs_available"`
	Weeks             []GrowthWeek       `json:"weeks"` // oldest first
	GeneratedAt       string             `json:"generated_at_eat"`
}

// EATWeekStart is 00:00 EAT on the Monday of t's EAT week.
func EATWeekStart(t time.Time) time.Time {
	e := t.In(EAT)
	d := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, EAT)
	off := (int(d.Weekday()) + 6) % 7 // Monday = 0
	return d.AddDate(0, 0, -off)
}

// growthPeriod is how long a package's paid time lasts from `from`.
func growthPeriodEnd(pkg *models.Package, from time.Time) time.Time {
	if pkg == nil {
		return from // no package: money in, no time on the network
	}
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		return from.Add(time.Duration(*pkg.TimeLimitMinutes) * time.Minute)
	}
	switch pkg.BillingCycle {
	case "hourly":
		return from.Add(time.Hour)
	case "daily":
		return from.Add(24 * time.Hour)
	case "weekly":
		return from.Add(7 * 24 * time.Hour)
	default:
		return from.AddDate(0, 1, 0)
	}
}

type growthPay struct {
	Phone     string
	Amount    float64
	Method    string
	PackageID *uint
	CreatedAt time.Time
}

type growthSpan struct{ from, to time.Time }

// growthLookback: paid time bought this long before the window can still
// cover it (monthly packages, stacked renewals).
const growthLookback = 75 * 24 * time.Hour

// BuildGrowth counts the last `weeks` EAT weeks (the current week last).
func BuildGrowth(db *gorm.DB, orgID uint, weeks int, now time.Time) (*GrowthReport, error) {
	if weeks < 1 {
		weeks = 1
	}
	plan, startSet := LoadGrowthPlan(db, orgID)
	curStart := EATWeekStart(now)
	first := curStart.AddDate(0, 0, -7*(weeks-1))
	end := curStart.AddDate(0, 0, 7)

	planStart := curStart
	if plan.StartDate != "" {
		if d, err := time.ParseInLocation("2006-01-02", plan.StartDate, EAT); err == nil {
			planStart = EATWeekStart(d)
		}
	}
	out := &GrowthReport{
		Settings:      plan,
		StartDateSet:  startSet,
		PlanStartWeek: planStart.Format("2006-01-02"),
		DaysPerMonth:  GrowthDaysPerMonth,
		Weeks:         []GrowthWeek{},
		GeneratedAt:   now.In(EAT).Format("2006-01-02 15:04"),
	}

	zoneIDs, err := scopeZoneIDs(db, InsightsScope{OrganizationID: orgID})
	if err != nil {
		return nil, err
	}

	// Payments: the window plus a lookback for paid time still running.
	var pays []growthPay
	firstPaid := map[string]time.Time{}
	if len(zoneIDs) > 0 {
		if err := db.Model(&models.Payment{}).
			Select("phone, amount, method, package_id, created_at").
			Where("zone_id IN (?) AND status = ?", zoneIDs, "completed").
			Where("created_at >= ? AND created_at < ?", dbT(first.Add(-growthLookback)), dbT(end)).
			Order("created_at ASC").Find(&pays).Error; err != nil {
			return nil, err
		}
		// First payments: from the loaded rows, except phones that already
		// paid before the lookback (they are not new in any shown week).
		var older []string
		if err := db.Model(&models.Payment{}).Distinct("phone").
			Where("zone_id IN (?) AND status = ? AND created_at < ?", zoneIDs, "completed", dbT(first.Add(-growthLookback))).
			Pluck("phone", &older).Error; err != nil {
			return nil, err
		}
		oldTimer := first.Add(-growthLookback - time.Hour)
		for _, ph := range older {
			firstPaid[normPhone(ph)] = oldTimer
		}
		for _, p := range pays {
			ph := normPhone(p.Phone)
			if _, ok := firstPaid[ph]; !ok {
				firstPaid[ph] = p.CreatedAt
			}
		}
	}

	// Packages bought (deleted ones too: they were real sales).
	pkgIDs := map[uint]bool{}
	for _, p := range pays {
		if p.PackageID != nil {
			pkgIDs[*p.PackageID] = true
		}
	}
	pkgs := map[uint]*models.Package{}
	if len(pkgIDs) > 0 {
		ids := make([]uint, 0, len(pkgIDs))
		for id := range pkgIDs {
			ids = append(ids, id)
		}
		var rows []models.Package
		if err := db.Unscoped().Select("id", "billing_cycle", "time_limit_minutes").Where("id IN ?", ids).Find(&rows).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			pkgs[rows[i].ID] = &rows[i]
		}
	}

	// Paid time per person, renewals stacked.
	spans := map[string][]growthSpan{}
	expiry := map[string]time.Time{}
	for _, p := range pays {
		ph := normPhone(p.Phone)
		if p.PackageID == nil {
			continue
		}
		start := p.CreatedAt
		if e, ok := expiry[ph]; ok && e.After(start) {
			start = e
		}
		stop := growthPeriodEnd(pkgs[*p.PackageID], start)
		if !stop.After(start) {
			continue
		}
		expiry[ph] = stop
		spans[ph] = append(spans[ph], growthSpan{start, stop})
	}

	installs := growthInstalls(db, orgID, first, end)
	out.InstallsAvailable = installs != nil

	// Plan weeks from the plan start to the last week shown.
	var planWeeks []PlanWeek
	if !planStart.After(curStart) {
		n := int(curStart.Sub(planStart).Hours()/(24*7)+0.5) + 1
		planWeeks = SimulateGrowthPlan(plan, n, GrowthDaysPerMonth)
	}

	for w := 0; w < weeks; w++ {
		ws := first.AddDate(0, 0, 7*w)
		we := ws.AddDate(0, 0, 7)
		gw := GrowthWeek{WeekStart: ws.Format("2006-01-02"), WeekEnd: we.AddDate(0, 0, -1).Format("2006-01-02"), Days: 7}
		if ws.Equal(curStart) {
			gw.Current = true
			today := time.Date(now.In(EAT).Year(), now.In(EAT).Month(), now.In(EAT).Day(), 0, 0, 0, 0, EAT)
			gw.Days = int(today.Sub(ws).Hours()/24+0.5) + 1
		}

		people := map[string]bool{}
		paying := map[string]bool{}
		for _, p := range pays {
			if p.CreatedAt.Before(ws) || !p.CreatedAt.Before(we) {
				continue
			}
			if p.Method == "credit" {
				continue
			}
			ph := normPhone(p.Phone)
			gw.Takings += p.Amount
			paying[ph] = true
			people[ph] = true
		}
		for ph, ss := range spans {
			for _, s := range ss {
				if s.from.Before(we) && s.to.After(ws) {
					people[ph] = true
					break
				}
			}
		}
		for ph := range people {
			if f, ok := firstPaid[ph]; ok && !f.Before(ws) && f.Before(we) {
				gw.NewCustomers++
			}
		}
		gw.People = len(people)
		gw.PayingCustomers = len(paying)
		if gw.People > 0 {
			v := round2(float64(gw.PayingCustomers) / float64(gw.People) * 100)
			gw.PayingPct = &v
		}
		days := float64(gw.Days)
		gw.Takings = round2(gw.Takings)
		gw.TakingsPerDay = round2(gw.Takings / days)
		gw.Fees = round2(gw.Takings * plan.FeePercent / 100)
		gw.FixedCosts = round2(GrowthFixedPerDay(plan, float64(gw.People), GrowthDaysPerMonth) * days)
		gw.Costs = round2(gw.Fees + gw.FixedCosts)
		gw.Profit = round2(gw.Takings - gw.Costs)
		gw.ProfitPerDay = round2(gw.Profit / days)
		if gw.Profit > 0 {
			gw.Reinvest = round2(gw.Profit * plan.ReinvestPercent / 100)
		}

		if installs != nil {
			n, cost := 0, 0.0
			for _, in := range installs {
				if !in.at.Before(ws) && in.at.Before(we) {
					n++
					cost += in.cost
				}
			}
			cost = round2(cost)
			gw.Installs, gw.InstallCost = &n, &cost
			if n > 0 {
				per := round2(cost / float64(n))
				gw.CostPerInstall = &per
			}
		}

		if idx := int(ws.Sub(planStart).Hours()/(24*7) + 0.5); !ws.Before(planStart) && idx < len(planWeeks) {
			pw := planWeeks[idx]
			pw.TakingsPerDay, pw.CostsPerDay, pw.ProfitPerDay = round2(pw.TakingsPerDay), round2(pw.CostsPerDay), round2(pw.ProfitPerDay)
			pw.Takings, pw.Costs, pw.Profit, pw.Reinvest = round2(pw.Takings), round2(pw.Costs), round2(pw.Profit), round2(pw.Reinvest)
			gw.Plan = &pw
			dp := gw.People - pw.People
			gw.PeopleVsPlan = &dp
			dd := round2(gw.ProfitPerDay - pw.ProfitPerDay)
			gw.ProfitPerDayVsPlan = &dd
			// Plan homes that join this week were bought with last week's pot.
			planNew := 0
			if idx > 0 {
				planNew = planWeeks[idx-1].NewPeople
			}
			dn := gw.NewCustomers - planNew
			gw.NewCustomersVsPlan = &dn
		}
		out.Weeks = append(out.Weeks, gw)
	}
	return out, nil
}

type growthInstall struct {
	at   time.Time
	cost float64
}

// growthInstalls reads finished installs from install_jobs when that table
// exists (another module owns it). Any problem → nil, never an error: the
// report must work without it. Only rows of this ISP are counted.
func growthInstalls(db *gorm.DB, orgID uint, from, to time.Time) (out []growthInstall) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	m := db.Migrator()
	if !m.HasTable("install_jobs") {
		return nil
	}
	for _, col := range []string{"status", "installed_at", "total_cost", "organization_id"} {
		if !m.HasColumn("install_jobs", col) {
			return nil
		}
	}
	type row struct {
		InstalledAt time.Time
		TotalCost   float64
	}
	var rows []row
	q := db.Table("install_jobs").Select("installed_at, total_cost").
		Where("organization_id = ? AND installed_at IS NOT NULL AND installed_at >= ? AND installed_at < ?", orgID, dbT(from), dbT(to)).
		Where("status IS NULL OR status <> ?", "cancelled")
	if m.HasColumn("install_jobs", "deleted_at") {
		q = q.Where("deleted_at IS NULL")
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil
	}
	out = make([]growthInstall, 0, len(rows))
	for _, r := range rows {
		if math.IsNaN(r.TotalCost) {
			r.TotalCost = 0
		}
		out = append(out, growthInstall{at: r.InstalledAt, cost: r.TotalCost})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}
