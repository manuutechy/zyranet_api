package services

import (
	"fmt"
	"log"
	"math"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Network › Watch: is a router's internet line (or its OLT) in trouble?
//
// Every heartbeat (1/min) a router on the current script sends its WAN port's
// byte counters, active PPPoE and hotspot sessions, and — when the zone has an
// OLT address — a 3-packet ping to the OLT. The server turns counter deltas
// into Mbps, compares them with the zone's uplink speed, and alerts:
//   - line FULL: 80%+ of the uplink (down or up) on every reading for 15 min;
//     cleared when it falls back under 70% (hysteresis: no flapping between).
//   - OLT DOWN: 3 heartbeats in a row with no ping reply; back when it answers.
//
// Alerts go into ops_alerts (the admin banner). SMS follows the router-alert
// rules: only if the ISP switched SMS on and saved phones, never 23:00–06:00
// EAT (those stay in the admin only and are not put in the 06:00 summary),
// at most one "full"/"down" SMS per zone per 2 hours, and a "back to normal"
// SMS only after a "full"/"down" SMS.
const (
	LineFullPct       = 80.0
	LineClearPct      = 70.0
	LineFullAfter     = 15 * time.Minute
	lineRunGap        = 5 * time.Minute // a longer silence breaks a "sustained" run
	OltDownAfterFails = 3

	lineRateMinGap = 20 * time.Second
	lineRateMaxGap = 10 * time.Minute
	lineMaxMbps    = 100000.0 // above 100 Gbit/s the reading is nonsense
	lineStatsKeep  = 35 * 24 * time.Hour
	// lineDataFresh: a reading older than this isn't "now".
	lineDataFresh = 5 * time.Minute
)

// LineCounters is one reading of a WAN port's byte counters.
type LineCounters struct {
	At     time.Time
	Rx, Tx uint64
}

// LineRate works out download/upload Mbps between two readings. ok=false
// when no honest figure exists: readings too close or too far apart (a gap
// would average a busy hour into a quiet one), or a counter went backwards
// (router rebooted or the 32/64-bit counter wrapped — one reading is skipped
// rather than guessing which).
func LineRate(prev, cur LineCounters) (downMbps, upMbps float64, ok bool) {
	dt := cur.At.Sub(prev.At)
	if dt < lineRateMinGap || dt > lineRateMaxGap {
		return 0, 0, false
	}
	if cur.Rx < prev.Rx || cur.Tx < prev.Tx {
		return 0, 0, false
	}
	sec := dt.Seconds()
	downMbps = float64(cur.Rx-prev.Rx) * 8 / sec / 1e6
	upMbps = float64(cur.Tx-prev.Tx) * 8 / sec / 1e6
	if downMbps > lineMaxMbps || upMbps > lineMaxMbps {
		return 0, 0, false
	}
	return math.Round(downMbps*100) / 100, math.Round(upMbps*100) / 100, true
}

// LinePct is mbps as a % of the uplink; nil when the uplink isn't set.
func LinePct(mbps float64, uplink int) *float64 {
	if uplink <= 0 {
		return nil
	}
	v := math.Round(mbps/float64(uplink)*1000) / 10
	return &v
}

// Watch events.
const (
	WatchLineFull = "line_full"
	WatchLineOK   = "line_ok"
	WatchOltDown  = "olt_down"
	WatchOltUp    = "olt_up"
)

// WatchInput is one heartbeat as the alerting sees it.
type WatchInput struct {
	At      time.Time
	LinePct *float64 // the busier direction, % of uplink; nil = no figure
	OltOK   *bool    // nil = no OLT ping in this heartbeat
}

// EvaluateWatch is the pure alert state machine (see the rules at the top).
func EvaluateWatch(st models.ZoneWatchState, in WatchInput) (models.ZoneWatchState, []string) {
	var events []string
	if st.LineStatus == "" {
		st.LineStatus = "ok"
	}
	if st.OltStatus == "" {
		st.OltStatus = "unknown"
	}
	if in.LinePct != nil {
		pct := *in.LinePct
		broken := st.LastSampleAt == nil || in.At.Sub(*st.LastSampleAt) > lineRunGap
		if pct >= LineFullPct {
			if st.HighSince == nil || broken {
				t := in.At
				st.HighSince = &t
			}
			if st.LineStatus != "full" && in.At.Sub(*st.HighSince) >= LineFullAfter {
				st.LineStatus = "full"
				t := *st.HighSince
				st.FullSince = &t
				events = append(events, WatchLineFull)
			}
		} else {
			st.HighSince = nil
			if st.LineStatus == "full" && pct < LineClearPct {
				st.LineStatus = "ok"
				events = append(events, WatchLineOK)
			}
		}
		t := in.At
		st.LastSampleAt = &t
	}
	if in.OltOK != nil {
		if *in.OltOK {
			if st.OltStatus == "down" {
				events = append(events, WatchOltUp)
			}
			st.OltStatus, st.OltFails = "up", 0
		} else {
			st.OltFails++
			if st.OltFails == 1 {
				t := in.At
				st.OltDownSince = &t
			}
			if st.OltFails >= OltDownAfterFails && st.OltStatus != "down" {
				st.OltStatus = "down"
				events = append(events, WatchOltDown)
			}
		}
	}
	return st, events
}

// LineReport is the watch part of one heartbeat (nil = not sent).
type LineReport struct {
	RxBytes, TxBytes *uint64
	PPPoE, Hotspot   *int
	OltOK            *bool
	OltMs            *float64
	CPU, MemPct      *int
}

// HasWatchData: the router runs the new heartbeat (sent any watch field).
func (r LineReport) HasWatchData() bool {
	return r.RxBytes != nil || r.TxBytes != nil || r.PPPoE != nil || r.OltOK != nil
}

// Test hooks: who sends the SMS, and how (tests run it inline).
var (
	NetWatchSender AlertSMSSender
	netWatchGo     = func(fn func()) { go fn() }
	netWatchMu     sync.Mutex
	netWatchPruned time.Time
)

func netWatchSender() AlertSMSSender {
	if NetWatchSender == nil {
		NetWatchSender = NewSmsAlertSender(NewSmsService())
	}
	return NetWatchSender
}

// MigrateNetWatch creates the watch tables (additive).
func MigrateNetWatch(db *gorm.DB) error {
	return db.AutoMigrate(&models.ZoneLineStat{}, &models.ZoneWatchState{})
}

// IngestLineReport stores one heartbeat's line reading and runs the alerts.
// Never fails the heartbeat: problems are logged.
func IngestLineReport(db *gorm.DB, zone *models.Zone, r LineReport, now time.Time) *models.ZoneLineStat {
	netWatchMu.Lock()
	defer netWatchMu.Unlock()

	row := models.ZoneLineStat{
		ZoneID: zone.ID, RecordedAt: dbT(now),
		RxBytes: r.RxBytes, TxBytes: r.TxBytes,
		PPPoE: r.PPPoE, Hotspot: r.Hotspot, OltOK: r.OltOK, OltMs: r.OltMs,
		CPU: r.CPU, MemPct: r.MemPct,
	}
	if r.OltOK != nil && !*r.OltOK {
		row.OltMs = nil
	}
	if r.RxBytes != nil && r.TxBytes != nil {
		var prevRows []models.ZoneLineStat
		db.Where("zone_id = ? AND rx_bytes IS NOT NULL AND tx_bytes IS NOT NULL", zone.ID).
			Order("recorded_at DESC").Limit(1).Find(&prevRows)
		if len(prevRows) == 1 {
			prev := prevRows[0]
			down, up, ok := LineRate(
				LineCounters{At: prev.RecordedAt, Rx: *prev.RxBytes, Tx: *prev.TxBytes},
				LineCounters{At: now, Rx: *r.RxBytes, Tx: *r.TxBytes})
			if ok {
				row.DownMbps, row.UpMbps = &down, &up
				row.DownPct = LinePct(down, zone.UplinkDownMbps)
				row.UpPct = LinePct(up, zone.UplinkUpMbps)
			}
		}
	}
	if err := db.Create(&row).Error; err != nil {
		log.Printf("[net-watch] zone %d reading: %v", zone.ID, err)
		return nil
	}

	var sts []models.ZoneWatchState
	db.Where("zone_id = ?", zone.ID).Limit(1).Find(&sts)
	var st models.ZoneWatchState
	if len(sts) == 1 {
		st = sts[0]
	} else {
		st = models.ZoneWatchState{ZoneID: zone.ID, OrganizationID: zone.OrganizationID, LineStatus: "ok", LineSms: "ok", OltStatus: "unknown", OltSms: "up"}
	}
	busier := maxPct(row.DownPct, row.UpPct)
	var oltIn *bool
	if zoneOltIP(zone) != "" {
		oltIn = r.OltOK
	}
	next, events := EvaluateWatch(st, WatchInput{At: dbT(now), LinePct: busier, OltOK: oltIn})
	next.OrganizationID = zone.OrganizationID
	for _, ev := range events {
		netWatchAlert(db, zone, &next, ev, &row, now)
	}
	next.UpdatedAt = dbT(now)
	if err := db.Save(&next).Error; err != nil {
		log.Printf("[net-watch] zone %d state: %v", zone.ID, err)
	}

	if now.Sub(netWatchPruned) > time.Hour {
		netWatchPruned = now
		db.Where("recorded_at < ?", dbT(now.Add(-lineStatsKeep))).Delete(&models.ZoneLineStat{})
	}
	return &row
}

func maxPct(a, b *float64) *float64 {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a >= *b:
		return a
	}
	return b
}

// netWatchAlert records one watch alert and texts the owner when due.
func netWatchAlert(db *gorm.DB, zone *models.Zone, st *models.ZoneWatchState, ev string, row *models.ZoneLineStat, now time.Time) {
	if zone.Status != "" && zone.Status != "active" {
		return
	}
	a := &models.OpsAlert{OrganizationID: zone.OrganizationID, ZoneID: zone.ID, ZoneName: zone.Name, Kind: ev, CreatedAt: dbT(now), UpdatedAt: dbT(now)}
	f1 := func(p *float64) string {
		if p == nil {
			return "?"
		}
		return fmt.Sprintf("%.0f", *p)
	}
	since := func(t *time.Time) string {
		if t == nil {
			return "a while"
		}
		return FormatDuration(now.Sub(*t))
	}
	switch ev {
	case WatchLineFull:
		pct := maxPct(row.DownPct, row.UpPct)
		a.Message = fmt.Sprintf("%s internet line is FULL: %s%% used for %s (%s of %d Mbps down, %s of %d up). Customers there will find it slow.",
			zone.Name, f1(pct), since(st.FullSince), f1(row.DownMbps), zone.UplinkDownMbps, f1(row.UpMbps), zone.UplinkUpMbps)
	case WatchLineOK:
		a.Message = fmt.Sprintf("%s internet line is back to normal (%s%% used). It was full for %s.",
			zone.Name, f1(maxPct(row.DownPct, row.UpPct)), since(st.FullSince))
	case WatchOltDown:
		a.Message = fmt.Sprintf("%s OLT (%s) is not answering: %d checks in a row since %s. Fibre homes there may have no internet.",
			zone.Name, zoneOltIP(zone), st.OltFails, formatEATStamp(derefT(st.OltDownSince, now), now))
		a.OfflineSince = st.OltDownSince
	case WatchOltUp:
		ms := ""
		if row.OltMs != nil {
			ms = fmt.Sprintf(", %.0f ms", *row.OltMs)
		}
		a.Message = fmt.Sprintf("%s OLT is answering again%s. It was down about %s.", zone.Name, ms, since(st.OltDownSince))
		st.OltDownSince = nil
	}

	cfgs := (&OpsAlertService{DB: db}).orgConfigs()
	cfg, ok := cfgs[zone.OrganizationID]
	send := false
	switch {
	case !ok || !cfg.enabled:
		a.Delivery = cfg.offReason()
	case InQuietHours(now):
		a.Delivery = "night_no_sms"
	default:
		switch ev {
		case WatchLineFull:
			if st.LastFullSmsAt != nil && now.Sub(*st.LastFullSmsAt) < OpsFlapWindow {
				a.Delivery = "flap_hold"
			} else {
				send = true
				t := dbT(now)
				st.LineSms, st.LastFullSmsAt = "full", &t
			}
		case WatchLineOK:
			if st.LineSms == "full" {
				send = true
				st.LineSms = "ok"
			} else {
				a.Delivery = "not_needed"
			}
		case WatchOltDown:
			if st.LastOltSmsAt != nil && now.Sub(*st.LastOltSmsAt) < OpsFlapWindow {
				a.Delivery = "flap_hold"
			} else {
				send = true
				t := dbT(now)
				st.OltSms, st.LastOltSmsAt = "down", &t
			}
		case WatchOltUp:
			if st.OltSms == "down" {
				send = true
				st.OltSms = "up"
			} else {
				a.Delivery = "not_needed"
			}
		}
	}
	if send {
		a.Delivery = "sending"
	}
	if err := db.Create(a).Error; err != nil {
		log.Printf("[net-watch] alert for zone %d: %v", zone.ID, err)
		return
	}
	log.Printf("[net-watch] zone %d (%s) %s", zone.ID, zone.Name, ev)
	if send {
		phones, id, orgID, msg := cfg.phones, a.ID, zone.OrganizationID, "Zyra Net: "+a.Message
		netWatchGo(func() {
			svc := &OpsAlertService{DB: db, Sender: netWatchSender()}
			d := svc.sendToOwners(orgID, phones, msg)
			db.Model(&models.OpsAlert{}).Where("id = ?", id).Update("delivery", d)
		})
	}
}

func derefT(t *time.Time, def time.Time) time.Time {
	if t == nil {
		return def
	}
	return *t
}

// ---- Heartbeat script ----

var wanPortRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)

// zoneOltIP is the zone's OLT address when it is a valid IP, else "".
func zoneOltIP(zone *models.Zone) string {
	if zone.OltIP == nil {
		return ""
	}
	ip := net.ParseIP(strings.TrimSpace(*zone.OltIP))
	if ip == nil {
		return ""
	}
	return ip.String()
}

// NormalizeOltIP validates an OLT address typed by staff ("" clears it).
func NormalizeOltIP(v string) (*string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	ip := net.ParseIP(v)
	if ip == nil {
		return nil, fmt.Errorf("%q is not an IP address (e.g. 192.168.100.1)", v)
	}
	s := ip.String()
	return &s, nil
}

// watchHeartbeatParts is the RouterOS (unescaped) that reads the line
// figures, and the URL query it appends. The WAN port name and OLT address
// are validated so nothing a user typed can end the string or add commands.
func watchHeartbeatParts(zone *models.Zone) (script, query string) {
	var sb strings.Builder
	var q strings.Builder
	if wan := strings.TrimSpace(zone.WanPort); wanPortRe.MatchString(wan) {
		fmt.Fprintf(&sb, `:local wr ""; :local wt ""; :do { :set wr [/interface get [find name="%s"] rx-byte]; :set wt [/interface get [find name="%s"] tx-byte] } on-error={}; `, wan, wan)
		q.WriteString(` . "&wanrx=" . $wr . "&wantx=" . $wt`)
	}
	sb.WriteString(`:local pp [:len [/ppp active find]]; `)
	q.WriteString(` . "&pppoe=" . $pp`)
	if ip := zoneOltIP(zone); ip != "" {
		fmt.Fprintf(&sb, `:local olr 0; :local olm 0; :do { /tool flood-ping %s count=3 do={ :if ($sent = 3) do={ :set olr $received; :set olm $"avg-rtt" } } } on-error={}; `, ip)
		q.WriteString(` . "&olt=" . $olr . "&oltms=" . $olm`)
	}
	return sb.String(), q.String()
}
