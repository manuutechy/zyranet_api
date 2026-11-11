package services

import (
	"sort"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
)

// Install summary: how many homes were connected, what each really cost,
// how fast crews work, and what is waiting. Weeks run Monday–Sunday EAT.

type InstallWeek struct {
	WeekStart        string  `json:"week_start"` // EAT Monday, YYYY-MM-DD
	Installed        int     `json:"installed"`
	WentLive         int     `json:"went_live"`
	LabourCost       float64 `json:"labour_cost"`
	MaterialsCost    float64 `json:"materials_cost"`
	TotalCost        float64 `json:"total_cost"`
	AvgCostPerHome   float64 `json:"avg_cost_per_install"`
	AvgLabour        float64 `json:"avg_labour"`
	AvgMaterials     float64 `json:"avg_materials"`
	PayingOfInstalls int     `json:"paying_of_installed"` // of this week's installs, how many have paid since
}

type InstallCrewStat struct {
	CrewID            uint    `json:"crew_id"`
	Name              string  `json:"name"`
	Installed         int     `json:"installed"`
	AvgDaysToInstall  float64 `json:"avg_days_to_install"`
	AvgCostPerInstall float64 `json:"avg_cost_per_install"`
	OpenJobs          int     `json:"open_jobs"`
}

type InstallBacklog struct {
	Requested      int     `json:"requested"`
	Scheduled      int     `json:"scheduled"`
	Assigned       int     `json:"assigned"`
	WaitingPayment int     `json:"installed_not_paying"`
	OldestWaitDays float64 `json:"oldest_wait_days"`
	Total          int     `json:"total"`
}

type InstallSummary struct {
	From              string            `json:"from"`
	To                string            `json:"to"`
	ByStatus          map[string]int    `json:"by_status"` // jobs requested in the range, by current status
	Installed         int               `json:"installed"`
	WentLive          int               `json:"went_live"`
	TotalCost         float64           `json:"total_cost"`
	AvgCostPerInstall float64           `json:"avg_cost_per_install"`
	CostPerPayingHome float64           `json:"cost_per_paying_home"` // cost of the range's installs ÷ those of them now paying
	PayingOfInstalled int               `json:"paying_of_installed"`
	AvgDaysToInstall  float64           `json:"avg_days_to_install"`
	Weeks             []InstallWeek     `json:"weeks"`
	Crews             []InstallCrewStat `json:"crews"`
	Backlog           InstallBacklog    `json:"backlog"`
	ThisWeek          InstallWeek       `json:"this_week"`
	LastWeek          InstallWeek       `json:"last_week"`
}

// WeekStartEAT is the Monday 00:00 EAT of the week holding t.
func WeekStartEAT(t time.Time) time.Time {
	k := utils.Kenya(t)
	offset := (int(k.Weekday()) + 6) % 7
	return time.Date(k.Year(), k.Month(), k.Day(), 0, 0, 0, 0, utils.EAT).AddDate(0, 0, -offset)
}

func avgCents(total int64, n int) float64 {
	if n == 0 {
		return 0
	}
	return centsToKES(int64(float64(total)/float64(n) + 0.5))
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// BuildInstallSummary reports on jobs the scope query allows, for the EAT
// days from..to (inclusive, both at 00:00 EAT).
func BuildInstallSummary(db *gorm.DB, a InstallActor, from, to, now time.Time) (*InstallSummary, error) {
	end := to.AddDate(0, 0, 1)
	out := &InstallSummary{From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), ByStatus: map[string]int{}}
	for _, s := range []string{models.InstallRequested, models.InstallScheduled, models.InstallAssigned, models.InstallInstalled, models.InstallLive, models.InstallCancelled} {
		out.ByStatus[s] = 0
	}

	// Jobs requested in range, by their status now.
	var byStatus []struct {
		Status string
		N      int
	}
	if err := InstallJobScope(db.Model(&models.InstallJob{}), a).Select("status, COUNT(*) AS n").
		Where("requested_at >= ? AND requested_at < ?", from.UTC(), end.UTC()).Group("status").Scan(&byStatus).Error; err != nil {
		return nil, err
	}
	for _, r := range byStatus {
		out.ByStatus[r.Status] = r.N
	}

	// Installs done in range (and the weeks around this week for the card).
	cardFrom := WeekStartEAT(now).AddDate(0, 0, -7)
	qFrom := from
	if cardFrom.Before(qFrom) {
		qFrom = cardFrom
	}
	qEnd := end
	if now.After(qEnd) {
		qEnd = now.Add(time.Second)
	}
	var jobs []models.InstallJob
	if err := InstallJobScope(db.Model(&models.InstallJob{}), a).
		Where("installed_at IS NOT NULL AND installed_at >= ? AND installed_at < ?", qFrom.UTC(), qEnd.UTC()).
		Find(&jobs).Error; err != nil {
		return nil, err
	}

	weeks := map[string]*InstallWeek{}
	type wacc struct{ labour, mat, total int64 }
	acc := map[string]*wacc{}
	getWeek := func(t time.Time) (*InstallWeek, *wacc) {
		k := WeekStartEAT(t).Format("2006-01-02")
		if weeks[k] == nil {
			weeks[k] = &InstallWeek{WeekStart: k}
			acc[k] = &wacc{}
		}
		return weeks[k], acc[k]
	}
	// Make sure every week in the range shows, even empty ones.
	for w := WeekStartEAT(from); w.Before(end); w = w.AddDate(0, 0, 7) {
		getWeek(w)
	}
	type crewAcc struct {
		n    int
		days float64
		cost int64
	}
	crews := map[uint]*crewAcc{}
	var totalCents int64
	var paying int
	var daysSum float64
	inRange := func(t time.Time) bool { return !t.Before(from) && t.Before(end) }
	for _, j := range jobs {
		at := *j.InstalledAt
		w, wa := getWeek(at)
		cost := toCents(j.TotalCost)
		isPaying := j.Status == models.InstallLive
		w.Installed++
		wa.labour += toCents(j.LabourCost)
		wa.mat += toCents(j.MaterialsCost)
		wa.total += cost
		if isPaying {
			w.PayingOfInstalls++
		}
		if !inRange(at) {
			continue
		}
		out.Installed++
		totalCents += cost
		days := at.Sub(j.RequestedAt).Hours() / 24
		if days < 0 {
			days = 0
		}
		daysSum += days
		if isPaying {
			paying++
		}
		if j.CrewID != nil {
			c := crews[*j.CrewID]
			if c == nil {
				c = &crewAcc{}
				crews[*j.CrewID] = c
			}
			c.n++
			c.days += days
			c.cost += cost
		}
	}

	// Homes that started paying, by the week they went live.
	var live []models.InstallJob
	InstallJobScope(db.Model(&models.InstallJob{}), a).Select("id, live_at").
		Where("live_at IS NOT NULL AND live_at >= ? AND live_at < ?", qFrom.UTC(), qEnd.UTC()).Find(&live)
	for _, j := range live {
		w, _ := getWeek(*j.LiveAt)
		w.WentLive++
		if inRange(*j.LiveAt) {
			out.WentLive++
		}
	}

	for k, w := range weeks {
		a := acc[k]
		w.LabourCost, w.MaterialsCost, w.TotalCost = centsToKES(a.labour), centsToKES(a.mat), centsToKES(a.total)
		w.AvgCostPerHome = avgCents(a.total, w.Installed)
		w.AvgLabour = avgCents(a.labour, w.Installed)
		w.AvgMaterials = avgCents(a.mat, w.Installed)
	}
	thisK := WeekStartEAT(now).Format("2006-01-02")
	lastK := WeekStartEAT(now).AddDate(0, 0, -7).Format("2006-01-02")
	if w := weeks[thisK]; w != nil {
		out.ThisWeek = *w
	} else {
		out.ThisWeek = InstallWeek{WeekStart: thisK}
	}
	if w := weeks[lastK]; w != nil {
		out.LastWeek = *w
	} else {
		out.LastWeek = InstallWeek{WeekStart: lastK}
	}
	firstK := WeekStartEAT(from).Format("2006-01-02")
	for k, w := range weeks {
		if k >= firstK && k < end.Format("2006-01-02") {
			out.Weeks = append(out.Weeks, *w)
		}
	}
	sort.Slice(out.Weeks, func(i, j int) bool { return out.Weeks[i].WeekStart < out.Weeks[j].WeekStart })

	out.TotalCost = centsToKES(totalCents)
	out.AvgCostPerInstall = avgCents(totalCents, out.Installed)
	out.PayingOfInstalled = paying
	out.CostPerPayingHome = avgCents(totalCents, paying)
	if out.Installed > 0 {
		out.AvgDaysToInstall = round1(daysSum / float64(out.Installed))
	}

	// Crews: every crew of the ISP, with open jobs now.
	var allCrews []models.InstallCrew
	cq := db.Where("organization_id = ?", a.OrgID)
	if a.Role == "installer" && a.CrewID != nil {
		cq = cq.Where("id = ?", *a.CrewID)
	}
	cq.Order("name").Find(&allCrews)
	var open []struct {
		CrewID uint
		N      int
	}
	InstallJobScope(db.Model(&models.InstallJob{}), a).Select("crew_id, COUNT(*) AS n").
		Where("crew_id IS NOT NULL AND status IN ?", []string{models.InstallScheduled, models.InstallAssigned}).
		Group("crew_id").Scan(&open)
	openBy := map[uint]int{}
	for _, o := range open {
		openBy[o.CrewID] = o.N
	}
	for _, c := range allCrews {
		st := InstallCrewStat{CrewID: c.ID, Name: c.Name, OpenJobs: openBy[c.ID]}
		if ca := crews[c.ID]; ca != nil {
			st.Installed = ca.n
			st.AvgDaysToInstall = round1(ca.days / float64(ca.n))
			st.AvgCostPerInstall = avgCents(ca.cost, ca.n)
		}
		if !c.Active && st.Installed == 0 && st.OpenJobs == 0 {
			continue
		}
		out.Crews = append(out.Crews, st)
	}

	// Backlog now (all time).
	var bl []struct {
		Status string
		N      int
	}
	InstallJobScope(db.Model(&models.InstallJob{}), a).Select("status, COUNT(*) AS n").
		Where("status IN ?", []string{models.InstallRequested, models.InstallScheduled, models.InstallAssigned, models.InstallInstalled}).
		Group("status").Scan(&bl)
	for _, r := range bl {
		switch r.Status {
		case models.InstallRequested:
			out.Backlog.Requested = r.N
		case models.InstallScheduled:
			out.Backlog.Scheduled = r.N
		case models.InstallAssigned:
			out.Backlog.Assigned = r.N
		case models.InstallInstalled:
			out.Backlog.WaitingPayment = r.N
		}
	}
	out.Backlog.Total = out.Backlog.Requested + out.Backlog.Scheduled + out.Backlog.Assigned
	var oldest models.InstallJob
	if InstallJobScope(db.Model(&models.InstallJob{}), a).
		Where("status IN ?", []string{models.InstallRequested, models.InstallScheduled, models.InstallAssigned}).
		Order("requested_at ASC").First(&oldest).Error == nil {
		out.Backlog.OldestWaitDays = round1(now.Sub(oldest.RequestedAt).Hours() / 24)
	}
	return out, nil
}
