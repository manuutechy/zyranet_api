package services

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// EAT is Kenya time (UTC+3, no daylight saving). A fixed zone, so it works
// without tzdata on the server.
var EAT = time.FixedZone("EAT", 3*60*60)

// PaidNotConnectedAfter: a completed payment whose router change is still
// not done this long after it was queued is shown to the owner.
const PaidNotConnectedAfter = 3 * time.Minute

// paidNotConnectedLookback limits the "paid but not connected" list to
// recent payments, so one old failed action doesn't sit there forever.
const paidNotConnectedLookback = 24 * time.Hour

// zoneStatsFreshFor: a router's reported client count is used as "online
// now" only if it was reported this recently.
const zoneStatsFreshFor = 10 * time.Minute

// Failure reasons, as shown on the dashboard. CustomerSide marks the ones
// the ISP can't fix (the payer cancelled, ignored the prompt, ...).
const (
	FailCancelled  = "cancelled"
	FailNoResponse = "no_response"
	FailLowBalance = "low_balance"
	FailWrongPIN   = "wrong_pin"
	FailBusy       = "busy"
	FailSystem     = "system"
)

var failReasonLabels = map[string]string{
	FailCancelled:  "Customer cancelled",
	FailNoResponse: "Didn't respond to the prompt",
	FailLowBalance: "Not enough M-Pesa balance",
	FailWrongPIN:   "Wrong PIN",
	FailBusy:       "Another M-Pesa payment open",
	FailSystem:     "System / M-Pesa error",
}

var failReasonOrder = []string{FailCancelled, FailNoResponse, FailLowBalance, FailWrongPIN, FailBusy, FailSystem}

// ClassifyFailureReason maps a failed payment's stored status_reason (the
// friendly text from friendlySTKFailureReason, or an older/raw message) to a
// dashboard bucket. Anything not clearly the customer's doing is "system".
func ClassifyFailureReason(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "another m-pesa transaction"):
		return FailBusy // its text says "finish or cancel it", so check first
	case strings.Contains(r, "cancel"):
		return FailCancelled
	case strings.Contains(r, "didn't respond"), strings.Contains(r, "did not respond"),
		strings.Contains(r, "timed out"), strings.Contains(r, "didn't enter"),
		strings.Contains(r, "request expired"), strings.Contains(r, "unable to reach the customer"):
		return FailNoResponse
	case strings.Contains(r, "insufficient"), strings.Contains(r, "not enough money"):
		return FailLowBalance
	case strings.Contains(r, "wrong") && strings.Contains(r, "pin"):
		return FailWrongPIN
	}
	return FailSystem
}

// LiveScope is whose numbers to show: one ISP, optionally one zone.
type LiveScope struct {
	OrganizationID uint
	ZoneID         *uint
}

type LiveMoney struct {
	TodayAmount            float64 `json:"today_amount"`
	TodayCount             int64   `json:"today_count"`
	Buyers                 int64   `json:"buyers"`
	YesterdaySameTime      float64 `json:"yesterday_same_time"`
	YesterdaySameTimeCount int64   `json:"yesterday_same_time_count"`
	YesterdayFullDay       float64 `json:"yesterday_full_day"`
	LastWeekSameTime       float64 `json:"last_week_same_time"`
	LastWeekSameTimeCount  int64   `json:"last_week_same_time_count"`
	LastWeekDay            string  `json:"last_week_day"` // e.g. "Thursday"
}

type LiveFailureBucket struct {
	Reason       string `json:"reason"`
	Label        string `json:"label"`
	Count        int64  `json:"count"`
	CustomerSide bool   `json:"customer_side"`
}

type LiveFailures struct {
	Attempts          int64               `json:"attempts"` // completed + failed today
	Failed            int64               `json:"failed"`
	Pending           int64               `json:"pending"` // prompts still waiting for an answer
	Rate              float64             `json:"rate"`    // failed / attempts, 0..1
	CustomerSideCount int64               `json:"customer_side_count"`
	Breakdown         []LiveFailureBucket `json:"breakdown"`
}

type LiveOnline struct {
	// RouterReported is the sum of connected clients that routers reported
	// in the last 10 minutes; RoutersReporting says how many routers that is.
	RouterReported   int64 `json:"router_reported"`
	RoutersReporting int   `json:"routers_reporting"`
	// ActivePlans is customers with time still running on a paid/free plan.
	ActivePlans int64 `json:"active_plans"`
}

type LiveZone struct {
	ID            uint       `json:"id"`
	Name          string     `json:"name"`
	Location      string     `json:"location"`
	Status        string     `json:"status"` // online | offline | never_seen
	LastSeenAt    *time.Time `json:"last_seen_at"`
	LastSeenEAT   string     `json:"last_seen_eat"`
	OutageSeconds int64      `json:"outage_seconds"`
	Clients       *int       `json:"clients"` // last fresh router report, nil if none
	TodayAmount   float64    `json:"today_amount"`
	TodayCount    int64      `json:"today_count"`
}

type LiveHour struct {
	Hour      int     `json:"hour"` // 0..23 EAT
	Amount    float64 `json:"amount"`
	Count     int64   `json:"count"`
	Yesterday float64 `json:"yesterday"`
}

type LivePayment struct {
	ID          uint      `json:"id"`
	ZoneID      uint      `json:"zone_id"`
	ZoneName    string    `json:"zone_name"`
	Phone       string    `json:"phone"` // masked
	Amount      float64   `json:"amount"`
	Method      string    `json:"method"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	ReasonLabel string    `json:"reason_label,omitempty"`
	Receipt     string    `json:"receipt,omitempty"`
	Package     string    `json:"package,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	TimeEAT     string    `json:"time_eat"`
}

type LiveStuck struct {
	ActionID    uint      `json:"action_id"`
	PaymentID   uint      `json:"payment_id"`
	ZoneID      uint      `json:"zone_id"`
	ZoneName    string    `json:"zone_name"`
	Action      string    `json:"action"`
	Status      string    `json:"status"` // pending | running | failed
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error"`
	Phone       string    `json:"phone"` // masked
	Amount      float64   `json:"amount"`
	Receipt     string    `json:"receipt,omitempty"`
	PaidAt      time.Time `json:"paid_at"`
	PaidAtEAT   string    `json:"paid_at_eat"`
	WaitingSecs int64     `json:"waiting_seconds"`
	ZoneOffline bool      `json:"zone_offline"`
}

type LiveOps struct {
	GeneratedAt      time.Time     `json:"generated_at"`
	GeneratedAtEAT   string        `json:"generated_at_eat"`
	Currency         string        `json:"currency"`
	Money            LiveMoney     `json:"money"`
	Failures         LiveFailures  `json:"failures"`
	Online           LiveOnline    `json:"online"`
	Zones            []LiveZone    `json:"zones"`
	Hourly           []LiveHour    `json:"hourly"`
	RecentPayments   []LivePayment `json:"recent_payments"`
	PaidNotConnected []LiveStuck   `json:"paid_not_connected"`
}

// StartOfDayEAT is midnight (EAT) of the day t falls on, in EAT.
func StartOfDayEAT(t time.Time) time.Time {
	e := t.In(EAT)
	return time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, EAT)
}

// MaskPhone keeps the first 4 and last 3 digits: 2547****678.
func MaskPhone(p string) string {
	p = strings.TrimSpace(p)
	if len(p) <= 7 {
		return p
	}
	return p[:4] + strings.Repeat("*", len(p)-7) + p[len(p)-3:]
}

// BuildLiveOps assembles the dashboard's live section. Every figure is read
// from the database; a handful of indexed queries, no router calls.
func BuildLiveOps(db *gorm.DB, scope LiveScope, now time.Time) (*LiveOps, error) {
	out := &LiveOps{
		GeneratedAt:      now,
		GeneratedAtEAT:   now.In(EAT).Format("15:04:05"),
		Currency:         "KES",
		Zones:            []LiveZone{},
		Hourly:           make([]LiveHour, 24),
		RecentPayments:   []LivePayment{},
		PaidNotConnected: []LiveStuck{},
	}
	for h := range out.Hourly {
		out.Hourly[h].Hour = h
	}
	out.Failures.Breakdown = []LiveFailureBucket{}

	t0 := StartOfDayEAT(now)
	y0 := t0.Add(-24 * time.Hour)
	yNow := now.Add(-24 * time.Hour)
	w0 := t0.AddDate(0, 0, -7)
	wNow := now.AddDate(0, 0, -7)
	out.Money.LastWeekDay = w0.Weekday().String()

	// 1. Zones (soft-deleted ones are excluded by GORM).
	var zones []models.Zone
	zq := db.Select("id", "name", "location", "status", "last_seen_at").
		Where("organization_id = ?", scope.OrganizationID)
	if scope.ZoneID != nil {
		zq = zq.Where("id = ?", *scope.ZoneID)
	}
	if err := zq.Order("name ASC").Find(&zones).Error; err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		return out, nil
	}
	zoneIDs := make([]uint, 0, len(zones))
	zoneByID := map[uint]*LiveZone{}
	for _, z := range zones {
		zoneIDs = append(zoneIDs, z.ID)
		if z.Status != "" && z.Status != "active" {
			continue // disabled zones: no router status shown
		}
		lz := LiveZone{ID: z.ID, Name: z.Name, Location: z.Location, LastSeenAt: z.LastSeenAt}
		switch {
		case z.LastSeenAt == nil:
			lz.Status = "never_seen"
		case now.Sub(*z.LastSeenAt) > RouterOfflineAfter:
			lz.Status = "offline"
			lz.OutageSeconds = int64(now.Sub(*z.LastSeenAt).Seconds())
		default:
			lz.Status = "online"
		}
		if z.LastSeenAt != nil {
			lz.LastSeenEAT = formatEATStamp(*z.LastSeenAt, now)
		}
		out.Zones = append(out.Zones, lz)
	}
	for i := range out.Zones {
		zoneByID[out.Zones[i].ID] = &out.Zones[i]
	}
	zoneName := map[uint]string{}
	for _, z := range zones {
		zoneName[z.ID] = z.Name
	}

	// 2. Money: today, yesterday (same time + full day) and the same weekday
	// last week up to the same time — one pass over the (zone_id, created_at)
	// index, skipping the days in between.
	var agg struct {
		TodayAmount float64
		TodayCount  int64
		Buyers      int64
		Failed      int64
		Pending     int64
		YAmount     float64
		YCount      int64
		YFull       float64
		WAmount     float64
		WCount      int64
	}
	if err := db.Raw(`SELECT
		COALESCE(SUM(CASE WHEN status = 'completed' AND created_at >= @t0 THEN amount ELSE 0 END), 0) AS today_amount,
		COALESCE(SUM(CASE WHEN status = 'completed' AND created_at >= @t0 THEN 1 ELSE 0 END), 0) AS today_count,
		COUNT(DISTINCT CASE WHEN status = 'completed' AND created_at >= @t0 THEN NULLIF(phone, '') END) AS buyers,
		COALESCE(SUM(CASE WHEN status = 'failed' AND created_at >= @t0 THEN 1 ELSE 0 END), 0) AS failed,
		COALESCE(SUM(CASE WHEN status = 'pending' AND created_at >= @t0 THEN 1 ELSE 0 END), 0) AS pending,
		COALESCE(SUM(CASE WHEN status = 'completed' AND created_at >= @y0 AND created_at < @ynow THEN amount ELSE 0 END), 0) AS y_amount,
		COALESCE(SUM(CASE WHEN status = 'completed' AND created_at >= @y0 AND created_at < @ynow THEN 1 ELSE 0 END), 0) AS y_count,
		COALESCE(SUM(CASE WHEN status = 'completed' AND created_at >= @y0 AND created_at < @t0 THEN amount ELSE 0 END), 0) AS y_full,
		COALESCE(SUM(CASE WHEN status = 'completed' AND created_at >= @w0 AND created_at < @wnow THEN amount ELSE 0 END), 0) AS w_amount,
		COALESCE(SUM(CASE WHEN status = 'completed' AND created_at >= @w0 AND created_at < @wnow THEN 1 ELSE 0 END), 0) AS w_count
		FROM payments
		WHERE zone_id IN @zones
		  AND ((created_at >= @w0 AND created_at < @wnow) OR created_at >= @y0)`,
		map[string]interface{}{"t0": dbT(t0), "y0": dbT(y0), "ynow": dbT(yNow), "w0": dbT(w0), "wnow": dbT(wNow), "zones": zoneIDs},
	).Scan(&agg).Error; err != nil {
		return nil, err
	}
	out.Money.TodayAmount = round2(agg.TodayAmount)
	out.Money.TodayCount = agg.TodayCount
	out.Money.Buyers = agg.Buyers
	out.Money.YesterdaySameTime = round2(agg.YAmount)
	out.Money.YesterdaySameTimeCount = agg.YCount
	out.Money.YesterdayFullDay = round2(agg.YFull)
	out.Money.LastWeekSameTime = round2(agg.WAmount)
	out.Money.LastWeekSameTimeCount = agg.WCount

	// 3. Failures today, by reason.
	var reasons []struct {
		StatusReason *string
		N            int64
	}
	if err := db.Raw(`SELECT status_reason, COUNT(*) AS n FROM payments
		WHERE zone_id IN ? AND status = 'failed' AND created_at >= ?
		GROUP BY status_reason`, zoneIDs, dbT(t0)).Scan(&reasons).Error; err != nil {
		return nil, err
	}
	buckets := map[string]int64{}
	for _, r := range reasons {
		reason := ""
		if r.StatusReason != nil {
			reason = *r.StatusReason
		}
		buckets[ClassifyFailureReason(reason)] += r.N
	}
	f := &out.Failures
	f.Failed, f.Pending = agg.Failed, agg.Pending
	f.Attempts = agg.TodayCount + agg.Failed
	if f.Attempts > 0 {
		f.Rate = float64(f.Failed) / float64(f.Attempts)
	}
	for _, k := range failReasonOrder {
		if buckets[k] == 0 {
			continue
		}
		cs := k != FailSystem
		f.Breakdown = append(f.Breakdown, LiveFailureBucket{Reason: k, Label: failReasonLabels[k], Count: buckets[k], CustomerSide: cs})
		if cs {
			f.CustomerSideCount += buckets[k]
		}
	}

	// 4. Hourly bars (today, with yesterday's same hour for comparison) and
	// per-zone takings. Only two small columns per completed payment.
	var rows []struct {
		ZoneID    uint
		Amount    float64
		CreatedAt time.Time
	}
	if err := db.Raw(`SELECT zone_id, amount, created_at FROM payments
		WHERE zone_id IN ? AND status = 'completed' AND created_at >= ?`, zoneIDs, dbT(y0)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		e := r.CreatedAt.In(EAT)
		h := e.Hour()
		if !r.CreatedAt.Before(t0) {
			out.Hourly[h].Amount += r.Amount
			out.Hourly[h].Count++
			if lz := zoneByID[r.ZoneID]; lz != nil {
				lz.TodayAmount += r.Amount
				lz.TodayCount++
			}
		} else {
			out.Hourly[h].Yesterday += r.Amount
		}
	}
	for h := range out.Hourly {
		out.Hourly[h].Amount = round2(out.Hourly[h].Amount)
		out.Hourly[h].Yesterday = round2(out.Hourly[h].Yesterday)
	}
	for i := range out.Zones {
		out.Zones[i].TodayAmount = round2(out.Zones[i].TodayAmount)
	}

	// 5. Who is online: routers' own client counts (fresh reports only) and
	// customers with time running.
	var stats []struct {
		ZoneID           uint
		ConnectedClients int
		RecordedAt       time.Time
	}
	if err := db.Raw(`SELECT zone_id, connected_clients, recorded_at FROM zone_stats
		WHERE id IN (SELECT MAX(id) FROM zone_stats WHERE zone_id IN ? GROUP BY zone_id)`, zoneIDs).Scan(&stats).Error; err != nil {
		return nil, err
	}
	for _, s := range stats {
		lz := zoneByID[s.ZoneID]
		if lz == nil || lz.Status != "online" || now.Sub(s.RecordedAt) > zoneStatsFreshFor {
			continue
		}
		n := s.ConnectedClients
		lz.Clients = &n
		out.Online.RouterReported += int64(n)
		out.Online.RoutersReporting++
	}
	if err := db.Model(&models.Customer{}).
		Where("zone_id IN ? AND status = ? AND expires_at > ?", zoneIDs, "active", dbT(now)).
		Count(&out.Online.ActivePlans).Error; err != nil {
		return nil, err
	}

	// 6. Last 10 payments (any status).
	var recent []struct {
		ID                 uint
		ZoneID             uint
		Phone              string
		Amount             float64
		Method             string
		Status             string
		StatusReason       *string
		MpesaReceiptNumber *string
		PackageName        *string
		CreatedAt          time.Time
	}
	if err := db.Raw(`SELECT p.id, p.zone_id, p.phone, p.amount, p.method, p.status, p.status_reason,
		p.mpesa_receipt_number, pk.name AS package_name, p.created_at
		FROM payments p LEFT JOIN packages pk ON pk.id = p.package_id
		WHERE p.zone_id IN ? ORDER BY p.id DESC LIMIT 10`, zoneIDs).Scan(&recent).Error; err != nil {
		return nil, err
	}
	for _, r := range recent {
		lp := LivePayment{
			ID: r.ID, ZoneID: r.ZoneID, ZoneName: zoneName[r.ZoneID], Phone: MaskPhone(r.Phone),
			Amount: round2(r.Amount), Method: r.Method, Status: r.Status, CreatedAt: r.CreatedAt,
			TimeEAT: formatEATStamp(r.CreatedAt, now),
		}
		if r.MpesaReceiptNumber != nil {
			lp.Receipt = *r.MpesaReceiptNumber
		}
		if r.PackageName != nil {
			lp.Package = *r.PackageName
		}
		if r.Status == "failed" {
			reason := ""
			if r.StatusReason != nil {
				reason = *r.StatusReason
			}
			lp.Reason = ClassifyFailureReason(reason)
			lp.ReasonLabel = failReasonLabels[lp.Reason]
		}
		out.RecentPayments = append(out.RecentPayments, lp)
	}

	// 7. Paid but not connected: money in, router change still owed after 3 min.
	var stuck []struct {
		ID                 uint
		PaymentID          uint
		ZoneID             uint
		Action             string
		Status             string
		Attempts           int
		LastError          string
		CreatedAt          time.Time
		Phone              string
		Amount             float64
		MpesaReceiptNumber *string
	}
	if err := db.Raw(`SELECT a.id, a.payment_id, a.zone_id, a.action, a.status, a.attempts, a.last_error, a.created_at,
		p.phone, p.amount, p.mpesa_receipt_number
		FROM pending_router_actions a JOIN payments p ON p.id = a.payment_id
		WHERE a.zone_id IN ? AND a.status IN ('pending', 'running', 'failed')
		  AND a.created_at <= ? AND a.created_at >= ? AND p.status = 'completed'
		ORDER BY a.id DESC LIMIT 50`, zoneIDs, dbT(now.Add(-PaidNotConnectedAfter)), dbT(now.Add(-paidNotConnectedLookback))).Scan(&stuck).Error; err != nil {
		return nil, err
	}
	for _, s := range stuck {
		ls := LiveStuck{
			ActionID: s.ID, PaymentID: s.PaymentID, ZoneID: s.ZoneID, ZoneName: zoneName[s.ZoneID],
			Action: s.Action, Status: s.Status, Attempts: s.Attempts, LastError: s.LastError,
			Phone: MaskPhone(s.Phone), Amount: round2(s.Amount), PaidAt: s.CreatedAt,
			PaidAtEAT: formatEATStamp(s.CreatedAt, now), WaitingSecs: int64(now.Sub(s.CreatedAt).Seconds()),
		}
		if s.MpesaReceiptNumber != nil {
			ls.Receipt = *s.MpesaReceiptNumber
		}
		if lz := zoneByID[s.ZoneID]; lz != nil && lz.Status != "online" {
			ls.ZoneOffline = true
		}
		out.PaidNotConnected = append(out.PaidNotConnected, ls)
	}

	// Given-up actions first (they need a person), then the longest waiting.
	sort.SliceStable(out.PaidNotConnected, func(i, j int) bool {
		a, b := out.PaidNotConnected[i], out.PaidNotConnected[j]
		if (a.Status == "failed") != (b.Status == "failed") {
			return a.Status == "failed"
		}
		return a.PaidAt.Before(b.PaidAt)
	})

	// Offline routers first, then by name.
	sort.SliceStable(out.Zones, func(i, j int) bool {
		return zoneRank(out.Zones[i].Status) < zoneRank(out.Zones[j].Status)
	})
	return out, nil
}

func zoneRank(s string) int {
	switch s {
	case "offline":
		return 0
	case "online":
		return 1
	}
	return 2
}

// formatEATStamp is "14:05" for today (EAT), "Yesterday 14:05", or "21 Oct 14:05".
func formatEATStamp(t, now time.Time) string {
	e := t.In(EAT)
	today := StartOfDayEAT(now)
	switch {
	case !e.Before(today):
		return e.Format("15:04")
	case !e.Before(today.Add(-24 * time.Hour)):
		return "Yesterday " + e.Format("15:04")
	}
	return e.Format("2 Jan 15:04")
}

// FormatDuration is "45 min", "1h 05m" or "2d 3h".
func FormatDuration(d time.Duration) string {
	if d < time.Minute {
		return "under 1 min"
	}
	m := int(d.Minutes())
	switch {
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 48*60:
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
	return fmt.Sprintf("%dd %dh", m/(24*60), (m%(24*60))/60)
}

// dbT puts a time in the zone the database connection uses (loc=Local), so
// every driver compares like with like.
func dbT(t time.Time) time.Time { return t.In(time.Local) }

// LiveOpsForScope is BuildLiveOps against the app database.
func LiveOpsForScope(scope LiveScope) (*LiveOps, error) {
	return BuildLiveOps(config.DB, scope, time.Now())
}
