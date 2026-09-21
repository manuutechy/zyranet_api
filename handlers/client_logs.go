package handlers

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// Client logs for ISP staff: who was online, for how long and how much they
// used (sessions), which sign-ins were accepted or refused, and a per-customer
// timeline of everything that happened to an account.
//
// Sessions and sign-ins come from FreeRADIUS's accounting and post-auth tables
// (radacct / radpostauth). Those are attributed to a customer and an ISP only
// through radius_accounts, so an ISP can never see another ISP's traffic, and a
// customer's history stays visible after they expire.

// logZoneScope is the set of zones the caller may see: their organisation's,
// narrowed to one zone for a zone manager.
func logZoneScope(c *fiber.Ctx) ([]uint, error) {
	ids, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return nil, err
	}
	claims := middleware.GetClaims(c)
	if claims.Role == "zone_manager" && claims.ZoneID != nil {
		for _, id := range ids {
			if id == *claims.ZoneID {
				return []uint{id}, nil
			}
		}
		return []uint{}, nil
	}
	return ids, nil
}

type sessionRow struct {
	ID              int64      `json:"id"`
	Username        string     `json:"username"`
	CustomerID      uint       `json:"customer_id"`
	CustomerName    string     `json:"customer_name"`
	CustomerPhone   string     `json:"customer_phone"`
	ZoneID          uint       `json:"zone_id"`
	ZoneName        string     `json:"zone_name"`
	StartedAt       *time.Time `json:"started_at"`
	StoppedAt       *time.Time `json:"stopped_at"`
	DurationSeconds int64      `json:"duration_seconds"`
	BytesUp         int64      `json:"bytes_up"`   // sent by the customer
	BytesDown       int64      `json:"bytes_down"` // received by the customer
	IP              string     `json:"ip"`
	MAC             string     `json:"mac"`
	TerminateCause  string     `json:"terminate_cause"`
	Active          bool       `json:"active"`
}

// parseDay reads a YYYY-MM-DD query value; endOfDay pushes it to 23:59:59.
func parseDay(v string, endOfDay bool) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(v))
	if err != nil {
		return time.Time{}, false
	}
	if endOfDay {
		t = t.Add(24*time.Hour - time.Second)
	}
	return t, true
}

// ClientSessionsLog lists RADIUS sessions for the caller's zones.
// Query: zone_id, customer_id, q (name/phone/username/MAC/IP), status=active|ended,
// from, to (YYYY-MM-DD), page, per_page.
func ClientSessionsLog(c *fiber.Ctx) error {
	if !services.RadiusTablesExist() {
		return utils.SuccessResponse(c, fiber.Map{"radius_enabled": false, "sessions": []sessionRow{}}, "")
	}
	zones, err := logZoneScope(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve zones.", "", fiber.StatusInternalServerError)
	}
	page, perPage := utils.ParsePage(c)

	where := []string{"a.zone_id IN ?"}
	args := []interface{}{zones}
	if z := c.QueryInt("zone_id", 0); z > 0 {
		where = append(where, "a.zone_id = ?")
		args = append(args, z)
	}
	if cid := c.QueryInt("customer_id", 0); cid > 0 {
		where = append(where, "a.customer_id = ?")
		args = append(args, cid)
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		like := "%" + q + "%"
		where = append(where, "(ra.username LIKE ? OR c.name LIKE ? OR c.phone LIKE ? OR ra.framedipaddress LIKE ? OR ra.callingstationid LIKE ?)")
		args = append(args, like, like, like, like, like)
	}
	switch c.Query("status") {
	case "active":
		where = append(where, "ra.acctstoptime IS NULL")
	case "ended":
		where = append(where, "ra.acctstoptime IS NOT NULL")
	}
	if t, ok := parseDay(c.Query("from"), false); ok {
		where = append(where, "ra.acctstarttime >= ?")
		args = append(args, t)
	}
	if t, ok := parseDay(c.Query("to"), true); ok {
		where = append(where, "ra.acctstarttime <= ?")
		args = append(args, t)
	}
	from := ` FROM radacct ra
		JOIN radius_accounts a ON a.username = ra.username
		LEFT JOIN customers c ON c.id = a.customer_id
		LEFT JOIN zones z ON z.id = a.zone_id
		WHERE ` + strings.Join(where, " AND ")

	var total int64
	config.DB.Raw("SELECT COUNT(*)"+from, args...).Scan(&total)

	var totals struct{ Up, Down, Active int64 }
	config.DB.Raw("SELECT COALESCE(SUM(ra.acctinputoctets),0) AS up, COALESCE(SUM(ra.acctoutputoctets),0) AS down, "+
		"COALESCE(SUM(CASE WHEN ra.acctstoptime IS NULL THEN 1 ELSE 0 END),0) AS active"+from, args...).Scan(&totals)

	rows := []sessionRow{} // never null in the JSON, even with no matches
	pageArgs := append(append([]interface{}{}, args...), perPage, utils.Offset(page, perPage))
	config.DB.Raw(`SELECT ra.radacctid AS id, ra.username, a.customer_id, COALESCE(c.name, '') AS customer_name,
		COALESCE(c.phone, '') AS customer_phone, a.zone_id, COALESCE(z.name, '') AS zone_name,
		ra.acctstarttime AS started_at, ra.acctstoptime AS stopped_at,
		COALESCE(ra.acctsessiontime, 0) AS duration_seconds,
		COALESCE(ra.acctinputoctets, 0) AS bytes_up, COALESCE(ra.acctoutputoctets, 0) AS bytes_down,
		COALESCE(ra.framedipaddress, '') AS ip, COALESCE(ra.callingstationid, '') AS mac,
		COALESCE(ra.acctterminatecause, '') AS terminate_cause`+from+
		` ORDER BY ra.acctstarttime DESC LIMIT ? OFFSET ?`, pageArgs...).Scan(&rows)
	for i := range rows {
		rows[i].Active = rows[i].StoppedAt == nil
	}

	return c.JSON(fiber.Map{
		"success": true,
		"data": fiber.Map{
			"radius_enabled": true,
			"sessions":       rows,
			"summary":        fiber.Map{"active_now": totals.Active, "bytes_up": totals.Up, "bytes_down": totals.Down},
		},
		"meta": fiber.Map{"total": total, "page": page, "per_page": perPage},
	})
}

type signInRow struct {
	ID            int64     `json:"id"`
	Username      string    `json:"username"`
	CustomerID    uint      `json:"customer_id"`
	CustomerName  string    `json:"customer_name"`
	CustomerPhone string    `json:"customer_phone"`
	ZoneID        uint      `json:"zone_id"`
	ZoneName      string    `json:"zone_name"`
	At            time.Time `json:"at"`
	Accepted      bool      `json:"accepted"`
	Result        string    `json:"result"`
}

// ClientSignInsLog lists accepted and refused sign-in attempts for the caller's
// zones (e.g. "why can't this customer connect?" → refused, plan expired).
// The attempted password is deliberately never selected.
// Query: zone_id, customer_id, q, result=accepted|rejected, from, to, page, per_page.
func ClientSignInsLog(c *fiber.Ctx) error {
	if !services.RadiusTablesExist() || !config.DB.Migrator().HasTable("radpostauth") {
		return utils.SuccessResponse(c, fiber.Map{"radius_enabled": false, "sign_ins": []signInRow{}}, "")
	}
	zones, err := logZoneScope(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve zones.", "", fiber.StatusInternalServerError)
	}
	page, perPage := utils.ParsePage(c)

	where := []string{"a.zone_id IN ?"}
	args := []interface{}{zones}
	if z := c.QueryInt("zone_id", 0); z > 0 {
		where = append(where, "a.zone_id = ?")
		args = append(args, z)
	}
	if cid := c.QueryInt("customer_id", 0); cid > 0 {
		where = append(where, "a.customer_id = ?")
		args = append(args, cid)
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		like := "%" + q + "%"
		where = append(where, "(pa.username LIKE ? OR c.name LIKE ? OR c.phone LIKE ?)")
		args = append(args, like, like, like)
	}
	switch c.Query("result") {
	case "accepted":
		where = append(where, "pa.reply = 'Access-Accept'")
	case "rejected":
		where = append(where, "pa.reply <> 'Access-Accept'")
	}
	if t, ok := parseDay(c.Query("from"), false); ok {
		where = append(where, "pa.authdate >= ?")
		args = append(args, t)
	}
	if t, ok := parseDay(c.Query("to"), true); ok {
		where = append(where, "pa.authdate <= ?")
		args = append(args, t)
	}
	from := ` FROM radpostauth pa
		JOIN radius_accounts a ON a.username = pa.username
		LEFT JOIN customers c ON c.id = a.customer_id
		LEFT JOIN zones z ON z.id = a.zone_id
		WHERE ` + strings.Join(where, " AND ")

	var total int64
	config.DB.Raw("SELECT COUNT(*)"+from, args...).Scan(&total)
	rows := []signInRow{}
	pageArgs := append(append([]interface{}{}, args...), perPage, utils.Offset(page, perPage))
	config.DB.Raw(`SELECT pa.id, pa.username, a.customer_id, COALESCE(c.name, '') AS customer_name,
		COALESCE(c.phone, '') AS customer_phone, a.zone_id, COALESCE(z.name, '') AS zone_name,
		pa.authdate AS at, pa.reply AS result`+from+` ORDER BY pa.authdate DESC LIMIT ? OFFSET ?`, pageArgs...).Scan(&rows)
	for i := range rows {
		rows[i].Accepted = rows[i].Result == "Access-Accept"
	}
	return c.JSON(fiber.Map{
		"success": true,
		"data":    fiber.Map{"radius_enabled": true, "sign_ins": rows},
		"meta":    fiber.Map{"total": total, "page": page, "per_page": perPage},
	})
}

type activityItem struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // payment | credit | sms | session | sign_in | admin
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	Status string    `json:"status,omitempty"` // ok | failed | pending | info
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanDuration(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", sec)
}

func truncateText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// CustomerActivity is one customer's timeline: payments, credit changes, SMS
// sent, staff actions, and (when RADIUS is on) sessions and sign-in attempts.
func CustomerActivity(c *fiber.Ctx) error {
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to view this customer.", "", fiber.StatusForbidden)
	}
	claims := middleware.GetClaims(c)
	items := []activityItem{}

	var payments []models.Payment
	config.DB.Where("customer_id = ?", customer.ID).Order("created_at DESC").Limit(100).Find(&payments)
	for _, p := range payments {
		status := map[string]string{"completed": "ok", "failed": "failed"}[p.Status]
		if status == "" {
			status = "pending"
		}
		detail := p.Method
		if p.MpesaReceiptNumber != nil && *p.MpesaReceiptNumber != "" {
			detail += " · " + *p.MpesaReceiptNumber
		}
		if p.StatusReason != nil && *p.StatusReason != "" {
			detail += " · " + *p.StatusReason
		}
		items = append(items, activityItem{Time: p.CreatedAt, Kind: "payment", Title: fmt.Sprintf("Payment KES %.0f (%s)", p.Amount, p.Status), Detail: detail, Status: status})
	}

	var credits []models.CreditLog
	config.DB.Where("customer_id = ?", customer.ID).Order("created_at DESC").Limit(100).Find(&credits)
	for _, cl := range credits {
		note := ""
		if cl.Note != nil {
			note = *cl.Note
		}
		verb := "Credit added"
		if cl.Type == "debit" {
			verb = "Credit used"
		}
		items = append(items, activityItem{Time: cl.CreatedAt, Kind: "credit", Title: fmt.Sprintf("%s KES %.2f", verb, cl.Amount), Detail: note, Status: "info"})
	}

	// Only this ISP's own messages: rows from before the organisation was
	// recorded count only if they were sent after this customer existed.
	var sms []models.SmsLog
	config.DB.Where("phone = ? AND (organization_id = ? OR (organization_id IS NULL AND created_at >= ?))",
		utils.FormatPhone(customer.Phone), claims.OrganizationID, customer.CreatedAt).
		Order("created_at DESC").Limit(50).Find(&sms)
	for _, m := range sms {
		st := "ok"
		if m.Status != "sent" {
			st = "failed"
		}
		items = append(items, activityItem{Time: m.CreatedAt, Kind: "sms", Title: "SMS " + m.Status, Detail: truncateText(m.Message, 160), Status: st})
	}

	var audits []models.AuditLog
	config.DB.Preload("User").Where("model = ? AND model_id = ?", "Customer", customer.ID).Order("created_at DESC").Limit(50).Find(&audits)
	for _, a := range audits {
		who := "System"
		if a.User != nil {
			who = a.User.Name
		}
		items = append(items, activityItem{Time: a.CreatedAt, Kind: "admin", Title: a.Action, Detail: "by " + who, Status: "info"})
	}

	radius := services.RadiusTablesExist()
	if radius {
		var sess []sessionRow
		config.DB.Raw(`SELECT ra.radacctid AS id, ra.username, ra.acctstarttime AS started_at, ra.acctstoptime AS stopped_at,
			COALESCE(ra.acctsessiontime,0) AS duration_seconds, COALESCE(ra.acctinputoctets,0) AS bytes_up,
			COALESCE(ra.acctoutputoctets,0) AS bytes_down, COALESCE(ra.framedipaddress,'') AS ip,
			COALESCE(ra.callingstationid,'') AS mac, COALESCE(ra.acctterminatecause,'') AS terminate_cause
			FROM radacct ra JOIN radius_accounts a ON a.username = ra.username
			WHERE a.customer_id = ? ORDER BY ra.acctstarttime DESC LIMIT 50`, customer.ID).Scan(&sess)
		for _, s := range sess {
			if s.StartedAt == nil {
				continue
			}
			title := fmt.Sprintf("Online %s · ↑ %s ↓ %s", humanDuration(s.DurationSeconds), humanBytes(s.BytesUp), humanBytes(s.BytesDown))
			detail := strings.TrimSpace(s.IP + " " + s.MAC)
			if s.StoppedAt == nil {
				title = "Online now · ↑ " + humanBytes(s.BytesUp) + " ↓ " + humanBytes(s.BytesDown)
			} else if s.TerminateCause != "" {
				detail += " · ended: " + s.TerminateCause
			}
			items = append(items, activityItem{Time: *s.StartedAt, Kind: "session", Title: title, Detail: detail, Status: "ok"})
		}
		if config.DB.Migrator().HasTable("radpostauth") {
			var auths []signInRow
			config.DB.Raw(`SELECT pa.id, pa.username, pa.authdate AS at, pa.reply AS result
				FROM radpostauth pa JOIN radius_accounts a ON a.username = pa.username
				WHERE a.customer_id = ? ORDER BY pa.authdate DESC LIMIT 50`, customer.ID).Scan(&auths)
			for _, a := range auths {
				title, st := "Sign-in accepted", "ok"
				if a.Result != "Access-Accept" {
					title, st = "Sign-in refused", "failed"
				}
				items = append(items, activityItem{Time: a.At, Kind: "sign_in", Title: title, Detail: a.Username, Status: st})
			}
		}
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].Time.After(items[j].Time) })
	if len(items) > 200 {
		items = items[:200]
	}
	return utils.SuccessResponse(c, fiber.Map{"items": items, "radius_enabled": radius}, "")
}
