package services

import (
	"sort"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Insights is the dashboard's "how is it working" report for a range of
// Kenyan (EAT) days. Every figure is counted from stored rows: payments,
// packages and router health snapshots. Nothing is estimated or filled in.

// InsightsScope is whose numbers to count: one ISP, optionally one zone.
type InsightsScope struct {
	OrganizationID uint
	ZoneID         *uint
}

type InsightsSummary struct {
	Kes             float64 `json:"kes"`
	Paid            int64   `json:"paid"`
	Failed          int64   `json:"failed"`
	Pending         int64   `json:"pending"` // prompts never answered either way
	Attempts        int64   `json:"attempts"`
	SuccessRate     float64 `json:"success_rate"` // paid / (paid + failed), 0..1
	Buyers          int64   `json:"buyers"`
	NewBuyers       int64   `json:"new_buyers"` // first ever paid in this range
	ReturningBuyers int64   `json:"returning_buyers"`
	AvgTicket       float64 `json:"avg_ticket"`
	KesPerBuyer     float64 `json:"kes_per_buyer"`
	ActiveDays      int     `json:"active_days"` // days with at least one sale
}

type InsightsDay struct {
	Date      string  `json:"date"`
	Kes       float64 `json:"kes"`
	Paid      int64   `json:"paid"`
	Failed    int64   `json:"failed"`
	Pending   int64   `json:"pending"`
	Buyers    int64   `json:"buyers"`
	NewBuyers int64   `json:"new_buyers"`
}

type InsightsPackage struct {
	ID    uint    `json:"id"` // 0 = no package recorded / "Other"
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Count int64   `json:"count"`
	Kes   float64 `json:"kes"`
}

type InsightsMethod struct {
	Method string  `json:"method"`
	Label  string  `json:"label"`
	Count  int64   `json:"count"`
	Kes    float64 `json:"kes"`
}

type InsightsLoyalty struct {
	Label  string  `json:"label"` // "1 time", "2–3 times", ...
	Buyers int64   `json:"buyers"`
	Kes    float64 `json:"kes"`
}

type InsightsRouterPoint struct {
	At         string `json:"at"` // EAT, "2006-01-02" (day) or "2006-01-02 15:00" (hour)
	Readings   int64  `json:"readings"`
	CPUAvg     int    `json:"cpu_avg"`
	CPUMax     int    `json:"cpu_max"`
	MemPct     int    `json:"mem_pct"` // average memory used, % of total
	ClientsAvg int    `json:"clients_avg"`
	ClientsMax int    `json:"clients_max"`
}

type InsightsRouter struct {
	ZoneID          uint                  `json:"zone_id"`
	Name            string                `json:"name"`
	Removed         bool                  `json:"removed"`
	Status          string                `json:"status"` // online | offline | never
	LastSeenEAT     string                `json:"last_seen_eat"`
	Bucket          string                `json:"bucket"` // hour | day
	ReportingBucket int                   `json:"reporting_buckets"`
	TotalBuckets    int                   `json:"total_buckets"`
	PeakClients     int                   `json:"peak_clients"`
	CPUAvg          int                   `json:"cpu_avg"`
	Points          []InsightsRouterPoint `json:"points"`
}

type Insights struct {
	From        string              `json:"from"`
	To          string              `json:"to"`
	Days        int                 `json:"days"`
	PrevFrom    string              `json:"prev_from"`
	PrevTo      string              `json:"prev_to"`
	Summary     InsightsSummary     `json:"summary"`
	Prev        InsightsSummary     `json:"prev"`
	Daily       []InsightsDay       `json:"daily"`
	Failures    []LiveFailureBucket `json:"failures"`
	Packages    []InsightsPackage   `json:"packages"`
	Methods     []InsightsMethod    `json:"methods"`
	Heat        [7][24]int64        `json:"heat"`     // [Mon..Sun][hour EAT] paid count
	HeatKes     [7][24]float64      `json:"heat_kes"` // same cells, KES
	Loyalty     []InsightsLoyalty   `json:"loyalty"`
	Routers     []InsightsRouter    `json:"routers"`
	FirstSale   string              `json:"first_sale"` // EAT date of the first paid sale ever (scope), "" if none
	LastSale    string              `json:"last_sale"`
	GeneratedAt string              `json:"generated_at_eat"`
}

// insightsMaxPackages: the package chart shows this many, then "Other".
const insightsMaxPackages = 7

type insPay struct {
	Phone        string
	Amount       float64
	Method       string
	Status       string
	StatusReason *string
	PackageID    *uint
	CreatedAt    time.Time
}

func normPhone(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "+")
	if strings.HasPrefix(p, "0") && len(p) == 10 {
		p = "254" + p[1:]
	}
	return p
}

func methodLabel(m string) (string, string) {
	switch m {
	case "mpesa", "stk":
		return "mpesa", "M-Pesa prompt"
	case "mpesa_c2b":
		return "mpesa_c2b", "Paybill"
	case "cash", "manual":
		return "cash", "Cash"
	case "bank":
		return "bank", "Bank"
	case "credit":
		return "credit", "Account credit"
	}
	return m, m
}

// scopeZoneIDs returns the ISP's zones (deleted ones too: money already
// earned stays in the books), narrowed to one zone if asked.
func scopeZoneIDs(db *gorm.DB, scope InsightsScope) ([]uint, error) {
	var ids []uint
	q := db.Unscoped().Model(&models.Zone{}).Where("organization_id = ?", scope.OrganizationID)
	if scope.ZoneID != nil {
		q = q.Where("id = ?", *scope.ZoneID)
	}
	if err := q.Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

func summarise(pays []insPay, firstPaid map[string]time.Time, start time.Time) InsightsSummary {
	var s InsightsSummary
	buyers := map[string]bool{}
	days := map[string]bool{}
	for _, p := range pays {
		switch p.Status {
		case "completed":
			s.Paid++
			s.Kes += p.Amount
			buyers[normPhone(p.Phone)] = true
			days[p.CreatedAt.In(EAT).Format("2006-01-02")] = true
		case "failed":
			s.Failed++
		case "pending":
			s.Pending++
		}
	}
	s.Attempts = s.Paid + s.Failed + s.Pending
	if s.Paid+s.Failed > 0 {
		s.SuccessRate = round2(float64(s.Paid) / float64(s.Paid+s.Failed))
	}
	s.Buyers = int64(len(buyers))
	for ph := range buyers {
		if f, ok := firstPaid[ph]; ok && !f.Before(start) {
			s.NewBuyers++
		}
	}
	s.ReturningBuyers = s.Buyers - s.NewBuyers
	if s.Paid > 0 {
		s.AvgTicket = round2(s.Kes / float64(s.Paid))
	}
	if s.Buyers > 0 {
		s.KesPerBuyer = round2(s.Kes / float64(s.Buyers))
	}
	s.Kes = round2(s.Kes)
	s.ActiveDays = len(days)
	return s
}

// BuildInsights counts the report for EAT days fromDay..toDay (inclusive).
// The previous period is the same number of days just before it.
func BuildInsights(db *gorm.DB, scope InsightsScope, fromDay, toDay time.Time, now time.Time) (*Insights, error) {
	start := time.Date(fromDay.Year(), fromDay.Month(), fromDay.Day(), 0, 0, 0, 0, EAT)
	endDay := time.Date(toDay.Year(), toDay.Month(), toDay.Day(), 0, 0, 0, 0, EAT)
	end := endDay.AddDate(0, 0, 1)
	days := int(end.Sub(start).Hours()/24 + 0.5)
	prevStart := start.AddDate(0, 0, -days)

	out := &Insights{
		From:        start.Format("2006-01-02"),
		To:          endDay.Format("2006-01-02"),
		Days:        days,
		PrevFrom:    prevStart.Format("2006-01-02"),
		PrevTo:      start.AddDate(0, 0, -1).Format("2006-01-02"),
		GeneratedAt: now.In(EAT).Format("15:04"),
		Daily:       []InsightsDay{},
		Failures:    []LiveFailureBucket{},
		Packages:    []InsightsPackage{},
		Methods:     []InsightsMethod{},
		Loyalty:     []InsightsLoyalty{},
		Routers:     []InsightsRouter{},
	}

	zoneIDs, err := scopeZoneIDs(db, scope)
	if err != nil {
		return nil, err
	}
	if len(zoneIDs) == 0 {
		return out, nil
	}

	// Payments for this period and the one before (for the comparison).
	var all []insPay
	if err := db.Model(&models.Payment{}).
		Select("phone, amount, method, status, status_reason, package_id, created_at").
		Where("zone_id IN (?)", zoneIDs).
		Where("created_at >= ? AND created_at < ?", dbT(prevStart), dbT(end)).
		Order("created_at ASC").
		Find(&all).Error; err != nil {
		return nil, err
	}
	var cur, prev []insPay
	for _, p := range all {
		if p.CreatedAt.Before(start) {
			prev = append(prev, p)
		} else {
			cur = append(cur, p)
		}
	}

	// First ever paid sale per phone, for new vs returning buyers. Only the
	// phones that bought in this or the previous period are looked up.
	phoneSet := map[string]bool{}
	var rawPhones []string
	for _, p := range all {
		if p.Status == "completed" && !phoneSet[p.Phone] {
			phoneSet[p.Phone] = true
			rawPhones = append(rawPhones, p.Phone)
		}
	}
	// The same person may be stored as 07.., 2547.. or +2547..
	var lookups []string
	for _, ph := range rawPhones {
		n := normPhone(ph)
		lookups = append(lookups, ph, n, "+"+n)
		if strings.HasPrefix(n, "254") && len(n) == 12 {
			lookups = append(lookups, "0"+n[3:])
		}
	}
	type paidRow struct {
		Phone     string
		CreatedAt time.Time
	}
	firstPaid := map[string]time.Time{}
	for i := 0; i < len(lookups); i += 500 {
		chunk := lookups[i:min(i+500, len(lookups))]
		var rows []paidRow
		if err := db.Model(&models.Payment{}).Select("phone, created_at").
			Where("zone_id IN (?) AND status = ? AND phone IN (?)", zoneIDs, "completed", chunk).
			Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			ph := normPhone(r.Phone)
			if old, ok := firstPaid[ph]; !ok || r.CreatedAt.Before(old) {
				firstPaid[ph] = r.CreatedAt
			}
		}
	}

	var firstRow, lastRow models.Payment
	if db.Select("created_at").Where("zone_id IN (?) AND status = ?", zoneIDs, "completed").
		Order("created_at ASC").Limit(1).Find(&firstRow).RowsAffected > 0 {
		out.FirstSale = firstRow.CreatedAt.In(EAT).Format("2006-01-02")
	}
	if db.Select("created_at").Where("zone_id IN (?) AND status = ?", zoneIDs, "completed").
		Order("created_at DESC").Limit(1).Find(&lastRow).RowsAffected > 0 {
		out.LastSale = lastRow.CreatedAt.In(EAT).Format("2006-01-02")
	}

	out.Summary = summarise(cur, firstPaid, start)
	out.Prev = summarise(prev, firstPaid, prevStart)

	// Daily rows, every day present (zero days included).
	dayIdx := map[string]int{}
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		dayIdx[key] = len(out.Daily)
		out.Daily = append(out.Daily, InsightsDay{Date: key})
	}
	dayBuyers := make([]map[string]bool, len(out.Daily))

	failCounts := map[string]int64{}
	pkgAgg := map[uint]*InsightsPackage{}
	methAgg := map[string]*InsightsMethod{}
	perBuyer := map[string]*struct {
		n   int64
		kes float64
	}{}

	for _, p := range cur {
		t := p.CreatedAt.In(EAT)
		i, ok := dayIdx[t.Format("2006-01-02")]
		if !ok {
			continue
		}
		d := &out.Daily[i]
		switch p.Status {
		case "failed":
			d.Failed++
			reason := ""
			if p.StatusReason != nil {
				reason = *p.StatusReason
			}
			failCounts[ClassifyFailureReason(reason)]++
			continue
		case "pending":
			d.Pending++
			continue
		case "completed":
		default:
			continue
		}
		ph := normPhone(p.Phone)
		d.Paid++
		d.Kes += p.Amount
		if dayBuyers[i] == nil {
			dayBuyers[i] = map[string]bool{}
		}
		if !dayBuyers[i][ph] {
			dayBuyers[i][ph] = true
			d.Buyers++
			if f, ok := firstPaid[ph]; ok && f.In(EAT).Format("2006-01-02") == d.Date {
				d.NewBuyers++
			}
		}

		// Monday = 0.
		dow := (int(t.Weekday()) + 6) % 7
		out.Heat[dow][t.Hour()]++
		out.HeatKes[dow][t.Hour()] += p.Amount

		var pid uint
		if p.PackageID != nil {
			pid = *p.PackageID
		}
		if pkgAgg[pid] == nil {
			pkgAgg[pid] = &InsightsPackage{ID: pid}
		}
		pkgAgg[pid].Count++
		pkgAgg[pid].Kes += p.Amount

		key, label := methodLabel(p.Method)
		if methAgg[key] == nil {
			methAgg[key] = &InsightsMethod{Method: key, Label: label}
		}
		methAgg[key].Count++
		methAgg[key].Kes += p.Amount

		if perBuyer[ph] == nil {
			perBuyer[ph] = &struct {
				n   int64
				kes float64
			}{}
		}
		perBuyer[ph].n++
		perBuyer[ph].kes += p.Amount
	}
	for i := range out.Daily {
		out.Daily[i].Kes = round2(out.Daily[i].Kes)
	}
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			out.HeatKes[d][h] = round2(out.HeatKes[d][h])
		}
	}

	for _, r := range failReasonOrder {
		if n := failCounts[r]; n > 0 {
			out.Failures = append(out.Failures, LiveFailureBucket{Reason: r, Label: failReasonLabels[r], Count: n, CustomerSide: r != FailSystem})
		}
	}

	// Packages: names from the packages table (deleted ones too).
	if len(pkgAgg) > 0 {
		var ids []uint
		for id := range pkgAgg {
			if id != 0 {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			var pk []models.Package
			db.Unscoped().Select("id, name, price").Where("id IN (?)", ids).Find(&pk)
			for _, x := range pk {
				if a := pkgAgg[x.ID]; a != nil {
					a.Name = strings.TrimSpace(x.Name)
					a.Price = x.Price
				}
			}
		}
		list := make([]InsightsPackage, 0, len(pkgAgg))
		for _, a := range pkgAgg {
			if a.ID == 0 {
				a.Name = "No package recorded"
			} else if a.Name == "" {
				a.Name = "Removed package"
			}
			a.Kes = round2(a.Kes)
			list = append(list, *a)
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Kes != list[j].Kes {
				return list[i].Kes > list[j].Kes
			}
			return list[i].Count > list[j].Count
		})
		if len(list) > insightsMaxPackages {
			other := InsightsPackage{Name: "Other packages"}
			for _, x := range list[insightsMaxPackages-1:] {
				other.Count += x.Count
				other.Kes += x.Kes
			}
			other.Kes = round2(other.Kes)
			list = append(list[:insightsMaxPackages-1], other)
		}
		out.Packages = list
	}

	for _, m := range methAgg {
		m.Kes = round2(m.Kes)
		out.Methods = append(out.Methods, *m)
	}
	sort.Slice(out.Methods, func(i, j int) bool { return out.Methods[i].Kes > out.Methods[j].Kes })

	// How often each buyer bought in the period.
	bands := []struct {
		label  string
		lo, hi int64
	}{{"Once", 1, 1}, {"2–3 times", 2, 3}, {"4–9 times", 4, 9}, {"10+ times", 10, 1 << 62}}
	for _, b := range bands {
		row := InsightsLoyalty{Label: b.label}
		for _, v := range perBuyer {
			if v.n >= b.lo && v.n <= b.hi {
				row.Buyers++
				row.Kes += v.kes
			}
		}
		row.Kes = round2(row.Kes)
		out.Loyalty = append(out.Loyalty, row)
	}

	routers, err := buildRouterHealth(db, scope, start, end, now)
	if err != nil {
		return nil, err
	}
	out.Routers = routers
	return out, nil
}

// buildRouterHealth turns router snapshots into hourly (ranges up to 3 days)
// or daily points. A bucket with no snapshot means the router did not check
// in then; the chart shows that as a gap, never as zero.
func buildRouterHealth(db *gorm.DB, scope InsightsScope, start, end, now time.Time) ([]InsightsRouter, error) {
	var zones []models.Zone
	q := db.Unscoped().Select("id, name, last_seen_at, deleted_at").Where("organization_id = ?", scope.OrganizationID)
	if scope.ZoneID != nil {
		q = q.Where("id = ?", *scope.ZoneID)
	}
	if err := q.Order("id ASC").Find(&zones).Error; err != nil {
		return nil, err
	}

	hourly := end.Sub(start) <= 72*time.Hour
	bucket := "day"
	step := func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	keyOf := func(t time.Time) string { return t.In(EAT).Format("2006-01-02") }
	if hourly {
		bucket = "hour"
		step = func(t time.Time) time.Time { return t.Add(time.Hour) }
		keyOf = func(t time.Time) string { return t.In(EAT).Format("2006-01-02 15:00") }
	}
	// Don't count buckets in the future as "missed".
	stop := end
	if now.Before(stop) {
		stop = now
	}

	out := []InsightsRouter{}
	for _, z := range zones {
		var stats []models.ZoneStat
		if err := db.Where("zone_id = ? AND recorded_at >= ? AND recorded_at < ?", z.ID, dbT(start), dbT(end)).
			Order("recorded_at ASC").Find(&stats).Error; err != nil {
			return nil, err
		}
		removed := z.DeletedAt.Valid
		// Removed zones only appear if they reported in this range.
		if removed && len(stats) == 0 {
			continue
		}
		r := InsightsRouter{ZoneID: z.ID, Name: z.Name, Removed: removed, Bucket: bucket, Status: "never", Points: []InsightsRouterPoint{}}
		if z.LastSeenAt != nil {
			r.LastSeenEAT = z.LastSeenAt.In(EAT).Format("Mon 2 Jan, 15:04")
			if now.Sub(*z.LastSeenAt) <= RouterOfflineAfter {
				r.Status = "online"
			} else {
				r.Status = "offline"
			}
		}

		type agg struct {
			n, cpuSum, memSum, cliSum int64
			cpuMax, cliMax            int
		}
		buckets := map[string]*agg{}
		var cpuSum, cpuN int64
		for _, s := range stats {
			k := keyOf(s.RecordedAt)
			a := buckets[k]
			if a == nil {
				a = &agg{}
				buckets[k] = a
			}
			a.n++
			a.cpuSum += int64(s.CPULoad)
			if s.CPULoad > a.cpuMax {
				a.cpuMax = s.CPULoad
			}
			if s.MemoryTotalMB > 0 {
				a.memSum += int64(s.MemoryUsedMB * 100 / s.MemoryTotalMB)
			}
			a.cliSum += int64(s.ConnectedClients)
			if s.ConnectedClients > a.cliMax {
				a.cliMax = s.ConnectedClients
			}
			if s.ConnectedClients > r.PeakClients {
				r.PeakClients = s.ConnectedClients
			}
			cpuSum += int64(s.CPULoad)
			cpuN++
		}
		if cpuN > 0 {
			r.CPUAvg = int(cpuSum / cpuN)
		}
		for t := start; t.Before(end); t = step(t) {
			k := keyOf(t)
			p := InsightsRouterPoint{At: k}
			if t.Before(stop) {
				r.TotalBuckets++
			}
			if a := buckets[k]; a != nil {
				r.ReportingBucket++
				p.Readings = a.n
				p.CPUAvg = int(a.cpuSum / a.n)
				p.CPUMax = a.cpuMax
				p.MemPct = int(a.memSum / a.n)
				p.ClientsAvg = int(a.cliSum / a.n)
				p.ClientsMax = a.cliMax
			}
			r.Points = append(r.Points, p)
		}
		out = append(out, r)
	}
	// Online first, then offline, then never seen.
	sort.SliceStable(out, func(i, j int) bool { return zoneRank(out[i].Status) < zoneRank(out[j].Status) })
	return out, nil
}
