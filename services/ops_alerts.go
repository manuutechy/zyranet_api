package services

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
)

// Owner router alerts.
//
// Rules:
//   - A router is offline when it hasn't checked in for RouterOfflineAfter
//     (5 min) — the same test checkout uses. Zones that never checked in,
//     disabled zones and deleted zones are never alerted.
//   - Every offline/online transition is recorded once (ops_alerts) and shown
//     in the admin banner.
//   - SMS only if the ISP switched it on and saved at least one phone.
//   - At most one "router down" SMS per zone per 2 hours (a flapping router).
//     If it is still down when the 2 hours are up, one more "down" SMS goes.
//     A "back online" SMS is only sent after a "down" SMS, so the two pair up.
//   - Quiet hours 23:00–06:00 EAT: nothing is sent; the night's events go out
//     as one summary SMS at 06:00.
const (
	OpsFlapWindow     = 2 * time.Hour
	opsQuietStartHour = 23
	opsQuietEndHour   = 6
	opsMaxAlertPhones = 3
	opsSmsMaxChars    = 459 // 3 SMS segments
	opsStaleOutage    = 24 * time.Hour
)

// AlertSMSSender sends one owner alert SMS. Tests use a fake.
type AlertSMSSender interface {
	SendAlert(organizationID uint, phone, message string) error
}

type smsAlertSender struct{ sms *SmsService }

func (s smsAlertSender) SendAlert(orgID uint, phone, message string) error {
	_, err := s.sms.Send(orgID, phone, message)
	return err
}

// NewSmsAlertSender sends owner alerts through the ISP's SMS gateway.
func NewSmsAlertSender(sms *SmsService) AlertSMSSender { return smsAlertSender{sms: sms} }

// OpsAlertService watches router health and alerts ISP owners.
type OpsAlertService struct {
	DB       *gorm.DB
	Sender   AlertSMSSender
	Now      func() time.Time
	stopChan chan struct{}
}

func NewOpsAlertService(db *gorm.DB, sender AlertSMSSender) *OpsAlertService {
	return &OpsAlertService{DB: db, Sender: sender, Now: time.Now, stopChan: make(chan struct{})}
}

// MigrateOpsTables creates the owner-alert tables and the payments index the
// live dashboard reads by (additive only).
func MigrateOpsTables(db *gorm.DB) error {
	if err := db.AutoMigrate(&models.OpsAlertSetting{}, &models.OpsZoneAlertState{}, &models.OpsAlert{}); err != nil {
		return err
	}
	if !db.Migrator().HasIndex(&models.Payment{}, "idx_payments_zone_created") {
		if err := db.Exec("CREATE INDEX idx_payments_zone_created ON payments (zone_id, created_at)").Error; err != nil {
			return err
		}
	}
	return nil
}

// Start migrates the tables and runs the check every minute.
func (s *OpsAlertService) Start() {
	if err := MigrateOpsTables(s.DB); err != nil {
		log.Printf("[ops-alerts] migration failed, owner alerts disabled: %v", err)
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	log.Println("[ops-alerts] Owner router alerts started (60s interval).")
	go func() {
		for {
			select {
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[ops-alerts] sweep panic: %v", r)
						}
					}()
					s.Sweep()
				}()
			case <-s.stopChan:
				ticker.Stop()
				return
			}
		}
	}()
}

func (s *OpsAlertService) Stop() { close(s.stopChan) }

// InQuietHours: 23:00–06:00 EAT.
func InQuietHours(t time.Time) bool {
	h := t.In(EAT).Hour()
	return h >= opsQuietStartHour || h < opsQuietEndHour
}

var kePhoneRe = regexp.MustCompile(`^254(7|1)\d{8}$`)

// NormalizeAlertPhones validates and normalises a list of owner phones.
func NormalizeAlertPhones(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		p := utils.FormatPhone(raw)
		if !kePhoneRe.MatchString(p) {
			return nil, fmt.Errorf("%q is not a valid Kenyan mobile number", raw)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) > opsMaxAlertPhones {
		return nil, fmt.Errorf("at most %d alert phones", opsMaxAlertPhones)
	}
	return out, nil
}

// SplitAlertPhones turns the stored list into numbers.
func SplitAlertPhones(stored string) []string {
	out := []string{}
	for _, p := range strings.Split(stored, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type opsOrgCfg struct {
	enabled bool
	phones  []string
	reason  string // delivery value when not sending: sms_off | no_phones
}

func (s *OpsAlertService) orgConfigs() map[uint]opsOrgCfg {
	var rows []models.OpsAlertSetting
	s.DB.Find(&rows)
	out := map[uint]opsOrgCfg{}
	for _, r := range rows {
		phones := SplitAlertPhones(r.Phones)
		c := opsOrgCfg{phones: phones}
		switch {
		case !r.SmsEnabled:
			c.reason = "sms_off"
		case len(phones) == 0:
			c.reason = "no_phones"
		default:
			c.enabled = true
		}
		out[r.OrganizationID] = c
	}
	return out
}

func (c opsOrgCfg) offReason() string {
	if c.reason == "" {
		return "sms_off"
	}
	return c.reason
}

// Sweep is one pass: record transitions, send the 06:00 summary, then send
// any per-zone SMS that is due.
func (s *OpsAlertService) Sweep() {
	now := dbT(s.Now())

	var zones []models.Zone
	if err := s.DB.Select("id", "name", "organization_id", "status", "last_seen_at").
		Where("status = ? AND last_seen_at IS NOT NULL", "active").Find(&zones).Error; err != nil {
		log.Printf("[ops-alerts] zones: %v", err)
		return
	}
	var stateRows []models.OpsZoneAlertState
	s.DB.Find(&stateRows)
	states := map[uint]*models.OpsZoneAlertState{}
	for i := range stateRows {
		states[stateRows[i].ZoneID] = &stateRows[i]
	}
	cfgs := s.orgConfigs()
	quiet := InQuietHours(now)

	// 1. Transitions.
	newAlert := map[uint]*models.OpsAlert{}
	for i := range zones {
		z := &zones[i]
		cur := "online"
		if now.Sub(*z.LastSeenAt) > RouterOfflineAfter {
			cur = "offline"
		}
		st := states[z.ID]
		if st == nil {
			// First sight (e.g. right after deploy): remember, don't alert.
			st = &models.OpsZoneAlertState{ZoneID: z.ID, OrganizationID: z.OrganizationID, Status: cur, SmsStatus: cur, UpdatedAt: now}
			if cur == "offline" {
				t := *z.LastSeenAt
				st.OfflineSince = &t
			}
			if err := s.DB.Create(st).Error; err != nil {
				log.Printf("[ops-alerts] state for zone %d: %v", z.ID, err)
				continue
			}
			states[z.ID] = st
			continue
		}
		if st.Status == cur {
			continue
		}
		updates := map[string]interface{}{"status": cur, "updated_at": now}
		var since *time.Time
		if cur == "offline" {
			t := *z.LastSeenAt
			since = &t
			updates["offline_since"] = since
		}
		// Conditional on the status we read, so a transition is claimed once.
		res := s.DB.Model(&models.OpsZoneAlertState{}).Where("zone_id = ? AND status = ?", z.ID, st.Status).Updates(updates)
		if res.Error != nil || res.RowsAffected != 1 {
			continue
		}
		prevSince := st.OfflineSince
		st.Status = cur
		if since != nil {
			st.OfflineSince = since
		}
		var a *models.OpsAlert
		if cur == "offline" {
			a = &models.OpsAlert{
				OrganizationID: z.OrganizationID, ZoneID: z.ID, ZoneName: z.Name, Kind: "offline",
				OfflineSince: since, OutageSeconds: int64(now.Sub(*since).Seconds()),
				Message: fmt.Sprintf("%s router is OFFLINE since %s (no check-in for %s). Customers there can't pay for WiFi until it's back.",
					z.Name, formatEATStamp(*since, now), FormatDuration(now.Sub(*since))),
			}
		} else {
			a = s.recoveredAlert(z, prevSince, now)
		}
		a.CreatedAt, a.UpdatedAt = now, now
		cfg, ok := cfgs[z.OrganizationID]
		switch {
		case !ok || !cfg.enabled:
			a.Delivery = cfg.offReason()
		case quiet:
			a.Delivery = "quiet_hours"
		}
		if err := s.DB.Create(a).Error; err != nil {
			log.Printf("[ops-alerts] alert for zone %d: %v", z.ID, err)
			continue
		}
		newAlert[z.ID] = a
		log.Printf("[ops-alerts] zone %d (%s) %s", z.ID, z.Name, strings.ToUpper(cur))
	}

	zoneByID := map[uint]*models.Zone{}
	for i := range zones {
		zoneByID[zones[i].ID] = &zones[i]
	}

	if quiet {
		return
	}

	// 2. Morning summary of what was held back overnight.
	for orgID, cfg := range cfgs {
		if cfg.enabled {
			s.sendMorningSummary(orgID, cfg, zones, states, now)
		}
	}

	// 3. Per-zone SMS that are due.
	for i := range zones {
		z := &zones[i]
		st := states[z.ID]
		if st == nil {
			continue
		}
		cfg := cfgs[z.OrganizationID]
		a := newAlert[z.ID]
		switch {
		case st.Status == "offline" && st.SmsStatus != "offline":
			if !cfg.enabled {
				continue
			}
			if st.OfflineSince != nil && now.Sub(*st.OfflineSince) > opsStaleOutage {
				// Down for over a day without the owner being texted (alerts
				// were off): don't announce an old outage out of the blue.
				s.DB.Model(&models.OpsZoneAlertState{}).Where("zone_id = ?", z.ID).Update("sms_status", "offline")
				st.SmsStatus = "offline"
				continue
			}
			if st.LastOfflineSmsAt != nil && now.Sub(*st.LastOfflineSmsAt) < OpsFlapWindow {
				if a != nil && a.Delivery == "" {
					s.setDelivery(a, "flap_hold")
				}
				continue
			}
			msg := s.offlineSMS(z, st, now)
			delivery := s.sendToOwners(z.OrganizationID, cfg.phones, msg)
			s.DB.Model(&models.OpsZoneAlertState{}).Where("zone_id = ?", z.ID).
				Updates(map[string]interface{}{"sms_status": "offline", "last_offline_sms_at": now})
			st.SmsStatus, st.LastOfflineSmsAt = "offline", &now
			if a == nil {
				// Held back earlier (flapping): the latest open "down" alert gets the result.
				a = s.latestAlert(z.ID, "offline")
			}
			if a != nil {
				s.setDelivery(a, delivery)
			}
		case st.Status == "online" && st.SmsStatus == "offline":
			if cfg.enabled {
				msg := s.backOnlineFallback(z, now)
				if a != nil && a.Message != "" {
					msg = "Zyra Net: " + a.Message
				}
				delivery := s.sendToOwners(z.OrganizationID, cfg.phones, msg)
				if a != nil {
					s.setDelivery(a, delivery)
				}
			}
			s.DB.Model(&models.OpsZoneAlertState{}).Where("zone_id = ?", z.ID).Update("sms_status", "online")
			st.SmsStatus = "online"
		case a != nil && a.Delivery == "":
			// e.g. back online after a "down" that was never texted (flap hold).
			s.setDelivery(a, "not_needed")
		}
	}
}

func (s *OpsAlertService) backOnlineFallback(z *models.Zone, now time.Time) string {
	return fmt.Sprintf("Zyra Net: %s router is back ONLINE (%s).", z.Name, now.In(EAT).Format("15:04"))
}

// recoveredAlert builds the "back online" alert, with the outage length and
// compensation from zone_outages when the heartbeat recorded one.
func (s *OpsAlertService) recoveredAlert(z *models.Zone, since *time.Time, now time.Time) *models.OpsAlert {
	a := &models.OpsAlert{OrganizationID: z.OrganizationID, ZoneID: z.ID, ZoneName: z.Name, Kind: "recovered", OfflineSince: since}
	back := now
	approx := true
	if since != nil {
		var outages []models.ZoneOutage
		s.DB.Where("zone_id = ? AND started_at >= ?", z.ID, since.Add(-2*time.Second)).Order("id ASC").Find(&outages)
		if len(outages) > 0 {
			n := 0
			for _, o := range outages {
				n += o.CustomersCompensated
			}
			a.CustomersCompensated = &n
			back = outages[len(outages)-1].EndedAt
			approx = false
		}
		a.OutageSeconds = int64(back.Sub(*since).Seconds())
	}
	a.RecoveredAt = &back
	var b strings.Builder
	fmt.Fprintf(&b, "%s router is back ONLINE (%s).", z.Name, back.In(EAT).Format("15:04"))
	if since != nil {
		about := ""
		if approx {
			about = "about "
		}
		fmt.Fprintf(&b, " Down %s%s, from %s.", about, FormatDuration(back.Sub(*since)), formatEATStamp(*since, now))
	}
	if a.CustomersCompensated != nil {
		switch *a.CustomersCompensated {
		case 0:
			b.WriteString(" No customer had time running, nothing to give back.")
		case 1:
			b.WriteString(" 1 customer got the lost time back.")
		default:
			fmt.Fprintf(&b, " %d customers got the lost time back.", *a.CustomersCompensated)
		}
	}
	a.Message = b.String()
	return a
}

func (s *OpsAlertService) offlineSMS(z *models.Zone, st *models.OpsZoneAlertState, now time.Time) string {
	since := now
	if st.OfflineSince != nil {
		since = *st.OfflineSince
	}
	msg := fmt.Sprintf("Zyra Net: %s router is OFFLINE since %s (%s). Customers there can't pay for WiFi until it's back.",
		z.Name, formatEATStamp(since, now), FormatDuration(now.Sub(since)))
	var drops int64
	s.DB.Model(&models.OpsAlert{}).Where("zone_id = ? AND kind = ? AND created_at >= ?", z.ID, "offline", now.Add(-OpsFlapWindow)).Count(&drops)
	if drops > 1 {
		msg += fmt.Sprintf(" It dropped %d times in the last 2 hours.", drops)
	}
	return msg
}

func (s *OpsAlertService) latestAlert(zoneID uint, kind string) *models.OpsAlert {
	var a models.OpsAlert
	if err := s.DB.Where("zone_id = ? AND kind = ?", zoneID, kind).Order("id DESC").First(&a).Error; err != nil {
		return nil
	}
	return &a
}

func (s *OpsAlertService) setDelivery(a *models.OpsAlert, d string) {
	a.Delivery = d
	s.DB.Model(&models.OpsAlert{}).Where("id = ?", a.ID).Update("delivery", d)
}

// sendToOwners texts every owner phone; sms_sent if at least one went out.
func (s *OpsAlertService) sendToOwners(orgID uint, phones []string, msg string) string {
	if s.Sender == nil {
		return "sms_failed"
	}
	if len(msg) > opsSmsMaxChars {
		msg = msg[:opsSmsMaxChars-3] + "..."
	}
	sent := 0
	for _, p := range phones {
		if err := s.Sender.SendAlert(orgID, p, msg); err != nil {
			log.Printf("[ops-alerts] SMS to owner of org %d failed: %v", orgID, err)
			continue
		}
		sent++
	}
	if sent == 0 {
		return "sms_failed"
	}
	return "sms_sent"
}

// sendMorningSummary sends one SMS covering alerts held back during quiet
// hours, then brings every zone's "what the owner was told" up to date.
func (s *OpsAlertService) sendMorningSummary(orgID uint, cfg opsOrgCfg, zones []models.Zone, states map[uint]*models.OpsZoneAlertState, now time.Time) {
	var queued []models.OpsAlert
	s.DB.Where("organization_id = ? AND delivery = ? AND created_at >= ?", orgID, "quiet_hours", now.Add(-12*time.Hour)).
		Order("id ASC").Find(&queued)
	if len(queued) == 0 {
		return
	}
	live := map[uint]*models.Zone{}
	for i := range zones {
		if zones[i].OrganizationID == orgID {
			live[zones[i].ID] = &zones[i]
		}
	}
	type zoneNight struct {
		name     string
		downs    int
		downFor  time.Duration
		stillOff *time.Time
		comp     int
	}
	byZone := map[uint]*zoneNight{}
	var order []uint
	var ids, dropped []uint
	for _, a := range queued {
		z := live[a.ZoneID]
		if z == nil {
			dropped = append(dropped, a.ID) // zone deleted/disabled since: never alert
			continue
		}
		ids = append(ids, a.ID)
		zn := byZone[a.ZoneID]
		if zn == nil {
			zn = &zoneNight{name: z.Name}
			byZone[a.ZoneID] = zn
			order = append(order, a.ZoneID)
		}
		switch a.Kind {
		case "offline":
			zn.downs++
		case "recovered":
			zn.downFor += time.Duration(a.OutageSeconds) * time.Second
			if a.CustomersCompensated != nil {
				zn.comp += *a.CustomersCompensated
			}
		}
	}
	if len(dropped) > 0 {
		s.DB.Model(&models.OpsAlert{}).Where("id IN ?", dropped).Update("delivery", "not_needed")
	}
	if len(order) == 0 {
		return
	}
	for _, zid := range order {
		if st := states[zid]; st != nil && st.Status == "offline" {
			byZone[zid].stillOff = st.OfflineSince
		}
	}
	sort.SliceStable(order, func(i, j int) bool { // still-down routers first
		return byZone[order[i]].stillOff != nil && byZone[order[j]].stillOff == nil
	})
	var parts []string
	for _, zid := range order {
		zn := byZone[zid]
		var p string
		if zn.stillOff != nil {
			p = fmt.Sprintf("%s STILL OFFLINE since %s", zn.name, formatEATStamp(*zn.stillOff, now))
		} else {
			p = fmt.Sprintf("%s went down %dx, back now", zn.name, maxInt(zn.downs, 1))
			if zn.downFor > 0 {
				p += fmt.Sprintf(" (down %s in total", FormatDuration(zn.downFor))
				if zn.comp > 0 {
					p += fmt.Sprintf(", %d customers got time back", zn.comp)
				}
				p += ")"
			}
		}
		parts = append(parts, p)
	}
	msg := "Zyra Net overnight (23:00-06:00): " + strings.Join(parts, "; ") + "."
	delivery := s.sendToOwners(orgID, cfg.phones, msg)

	sum := models.OpsAlert{OrganizationID: orgID, Kind: "summary", Message: strings.TrimPrefix(msg, "Zyra Net "), Delivery: delivery, CreatedAt: now, UpdatedAt: now}
	s.DB.Create(&sum)
	s.DB.Model(&models.OpsAlert{}).Where("id IN ?", ids).Update("delivery", "in_summary")

	// The summary told the owner the current state of every zone in it.
	for _, zid := range order {
		st := states[zid]
		if st == nil {
			continue
		}
		upd := map[string]interface{}{"sms_status": st.Status}
		st.SmsStatus = st.Status
		if st.Status == "offline" {
			upd["last_offline_sms_at"] = now
			st.LastOfflineSmsAt = &now
		}
		s.DB.Model(&models.OpsZoneAlertState{}).Where("zone_id = ?", zid).Updates(upd)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// OpsAlertsForBanner returns the org's un-dismissed alerts from the last
// 24 h (newest first), optionally for one zone (zone managers).
func OpsAlertsForBanner(orgID uint, zoneID *uint, now time.Time) ([]models.OpsAlert, error) {
	q := config.DB.Where("organization_id = ? AND acknowledged_at IS NULL AND created_at >= ?", orgID, now.Add(-24*time.Hour))
	if zoneID != nil {
		q = q.Where("zone_id = ?", *zoneID)
	}
	var out []models.OpsAlert
	err := q.Order("id DESC").Limit(20).Find(&out).Error
	return out, err
}
