package services

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Smart speed figures. The router's heartbeat sends each lane's queue byte
// counter (&lanes=downRt,downQuick,downHeavy,upRt,upQuick,upHeavy); the
// server keeps what moved between readings. Nothing is estimated: no reading
// → no figure.

// laneQueueNames is the order of the counters in &lanes=.
var laneQueueNames = []string{"zq-down-rt", "zq-down-quick", "zq-down-heavy", "zq-up-rt", "zq-up-quick", "zq-up-heavy"}

// SmartLanes is what each lane is, in the words the admin shows.
var SmartLanes = []struct{ Key, Label, What string }{
	{"rt", "Games and calls", "Online games, voice and video calls, and the small lookups every app starts with. Always first, so they don't lag."},
	{"quick", "Browsing and chat", "Web pages, WhatsApp and other messages, social feeds: the first 2 MB of anything a phone opens."},
	{"heavy", "Big downloads and video", "Long videos, app updates and file downloads. They use what is left over, so they never slow the others."},
}

// laneHeartbeatParts is the heartbeat script fragment and query suffix that
// report the lane counters. Empty unless Smart speed is on for the zone. If
// the lanes aren't on the router (yet), nothing is sent.
func laneHeartbeatParts(zone *models.Zone) (script, query string) {
	if on, _ := SmartSpeedState(zone); !on {
		return "", ""
	}
	var names []string
	for _, n := range laneQueueNames {
		names = append(names, `"`+n+`"`)
	}
	script = fmt.Sprintf(`:local ql ""; :do { :foreach qn in={%s} do={ :set ql ($ql . [/queue tree get [find name=$qn] bytes] . ",") } } on-error={ :set ql "" }; `, strings.Join(names, ";"))
	return script, ` . "&lanes=" . $ql`
}

// ParseLaneCounters reads &lanes=; ok=false unless it is exactly six counters.
func ParseLaneCounters(raw string) (c [6]uint64, ok bool) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(raw), ","), ",")
	if len(parts) != 6 {
		return c, false
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return c, false
		}
		c[i] = v
	}
	return c, true
}

// MigrateSmartSpeed creates the lane readings table (additive).
func MigrateSmartSpeed(db *gorm.DB) error { return db.AutoMigrate(&models.ZoneLaneStat{}) }

var laneStatsPruned time.Time

// IngestLaneReport stores one heartbeat's lane counters. Never fails the
// heartbeat.
func IngestLaneReport(db *gorm.DB, zone *models.Zone, c [6]uint64, now time.Time) *models.ZoneLaneStat {
	row := models.ZoneLaneStat{ZoneID: zone.ID, RecordedAt: dbT(now),
		DownRt: c[0], DownQuick: c[1], DownHeavy: c[2], UpRt: c[3], UpQuick: c[4], UpHeavy: c[5]}
	var prevRows []models.ZoneLaneStat
	db.Where("zone_id = ?", zone.ID).Order("recorded_at DESC").Limit(1).Find(&prevRows)
	if len(prevRows) == 1 {
		p := prevRows[0]
		pc := [6]uint64{p.DownRt, p.DownQuick, p.DownHeavy, p.UpRt, p.UpQuick, p.UpHeavy}
		dt := now.Sub(p.RecordedAt)
		honest := dt >= lineRateMinGap && dt <= lineRateMaxGap
		for i := range c {
			if c[i] < pc[i] { // lanes rebuilt or router rebooted: skip one reading
				honest = false
			}
		}
		if honest {
			sec := dt.Seconds()
			d := make([]uint64, 6)
			for i := range c {
				d[i] = c[i] - pc[i]
			}
			row.Secs = &sec
			row.DeltaDownRt, row.DeltaDownQuick, row.DeltaDownHeavy = &d[0], &d[1], &d[2]
			row.DeltaUpRt, row.DeltaUpQuick, row.DeltaUpHeavy = &d[3], &d[4], &d[5]
		}
	}
	if err := db.Create(&row).Error; err != nil {
		log.Printf("[smart-speed] zone %d reading: %v", zone.ID, err)
		return nil
	}
	if now.Sub(laneStatsPruned) > time.Hour {
		laneStatsPruned = now
		db.Where("recorded_at < ?", dbT(now.Add(-lineStatsKeep))).Delete(&models.ZoneLaneStat{})
	}
	return &row
}

// ---- report ----

type SmartLaneNow struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	What     string   `json:"what"`
	DownMbps *float64 `json:"down_mbps"` // last reading, if under 5 min old
	UpMbps   *float64 `json:"up_mbps"`
	SharePct *float64 `json:"share_pct"` // of the download moving right now
	TodayMB  *float64 `json:"today_mb"`  // download + upload since 00:00 EAT
	TodayPct *float64 `json:"today_pct"` // share of today's data
}

type SmartPoint struct {
	At    string     `json:"at"`   // RFC3339, start of the bucket
	Down  []*float64 `json:"down"` // Mbps per lane, in SmartLanes order; nil = no reading
	Total *float64   `json:"total"`
}

type SmartZone struct {
	ZoneID         uint   `json:"zone_id"`
	Name           string `json:"name"`
	Online         bool   `json:"online"`
	LastSeenEAT    string `json:"last_seen_eat"`
	SmartSpeed     bool   `json:"smart_speed"`        // the switch
	Active         bool   `json:"smart_speed_active"` // switch on and line speed known
	Status         string `json:"status"`             // off | needs_speed | waiting | running
	StatusText     string `json:"status_text"`
	UplinkDownMbps int    `json:"uplink_down_mbps"`
	UplinkUpMbps   int    `json:"uplink_up_mbps"`
	// Running: the router sent lane figures in the last 5 minutes.
	Running    bool           `json:"running"`
	LastLaneAt *string        `json:"last_lane_at"`
	Lanes      []SmartLaneNow `json:"lanes"`
	Points     []SmartPoint   `json:"points"`
	BucketMin  int            `json:"bucket_minutes"`
}

type SmartSpeedReport struct {
	GeneratedEAT string      `json:"generated_at_eat"`
	Minutes      int         `json:"minutes"`
	Zones        []SmartZone `json:"zones"`
}

const smartFresh = 5 * time.Minute

// BuildSmartSpeed: zoneIDs nil = the whole ISP. minutes = chart window
// (60–1440); buckets keep the chart at ≤ 120 points.
func BuildSmartSpeed(db *gorm.DB, orgID uint, zoneIDs []uint, minutes int, now time.Time) (*SmartSpeedReport, error) {
	if minutes < 60 {
		minutes = 60
	}
	if minutes > 1440 {
		minutes = 1440
	}
	watch, err := BuildZonesWatchIn(db, orgID, zoneIDs, now)
	if err != nil {
		return nil, err
	}
	out := &SmartSpeedReport{GeneratedEAT: now.In(EAT).Format("15:04:05"), Minutes: minutes, Zones: []SmartZone{}}
	bucketMin := int(math.Ceil(float64(minutes) / 120))
	bd := time.Duration(bucketMin) * time.Minute
	end := now.Truncate(bd).Add(bd)
	start := end.Add(-time.Duration(minutes) * time.Minute)
	dayStart := StartOfDayEAT(now)
	from := start
	if dayStart.Before(from) {
		from = dayStart
	}

	for _, w := range watch.Zones {
		var zone models.Zone
		if err := db.First(&zone, w.ZoneID).Error; err != nil {
			continue
		}
		active, _ := SmartSpeedState(&zone)
		sz := SmartZone{ZoneID: zone.ID, Name: zone.Name, Online: w.Online, LastSeenEAT: w.LastSeenEAT,
			SmartSpeed: zone.SmartSpeed, Active: active, UplinkDownMbps: zone.UplinkDownMbps, UplinkUpMbps: zone.UplinkUpMbps,
			BucketMin: bucketMin, Points: []SmartPoint{}}

		var rows []models.ZoneLaneStat
		if err := db.Where("zone_id = ? AND recorded_at >= ? AND recorded_at < ?", zone.ID, dbT(from), dbT(end)).
			Order("recorded_at ASC").Find(&rows).Error; err != nil {
			return nil, err
		}

		// Now: the newest reading, if fresh and with deltas.
		var nowDown, nowUp [3]*float64
		if n := len(rows); n > 0 {
			last := rows[n-1]
			at := last.RecordedAt.UTC().Format(time.RFC3339)
			sz.LastLaneAt = &at
			if now.Sub(last.RecordedAt) <= smartFresh {
				sz.Running = true
				if last.Secs != nil && *last.Secs > 0 {
					for i, d := range []*uint64{last.DeltaDownRt, last.DeltaDownQuick, last.DeltaDownHeavy} {
						if d != nil {
							v := round2(float64(*d) * 8 / *last.Secs / 1e6)
							nowDown[i] = &v
						}
					}
					for i, d := range []*uint64{last.DeltaUpRt, last.DeltaUpQuick, last.DeltaUpHeavy} {
						if d != nil {
							v := round2(float64(*d) * 8 / *last.Secs / 1e6)
							nowUp[i] = &v
						}
					}
				}
			}
		}
		var nowTotal float64
		for _, v := range nowDown {
			if v != nil {
				nowTotal += *v
			}
		}

		// Today: bytes per lane since 00:00 EAT.
		var today [3]float64
		haveToday := false
		for _, r := range rows {
			if r.RecordedAt.Before(dayStart) || r.DeltaDownRt == nil {
				continue
			}
			haveToday = true
			today[0] += float64(*r.DeltaDownRt + *r.DeltaUpRt)
			today[1] += float64(*r.DeltaDownQuick + *r.DeltaUpQuick)
			today[2] += float64(*r.DeltaDownHeavy + *r.DeltaUpHeavy)
		}
		todayTotal := today[0] + today[1] + today[2]

		for i, l := range SmartLanes {
			ln := SmartLaneNow{Key: l.Key, Label: l.Label, What: l.What, DownMbps: nowDown[i], UpMbps: nowUp[i]}
			if nowDown[i] != nil && nowTotal > 0 {
				p := math.Round(*nowDown[i]/nowTotal*1000) / 10
				ln.SharePct = &p
			}
			if haveToday {
				mb := round2(today[i] / 1e6)
				ln.TodayMB = &mb
				if todayTotal > 0 {
					p := math.Round(today[i]/todayTotal*1000) / 10
					ln.TodayPct = &p
				}
			}
			sz.Lanes = append(sz.Lanes, ln)
		}

		// Chart: average download Mbps per lane per bucket.
		nb := int(end.Sub(start) / bd)
		sumBytes := make([][3]float64, nb)
		sumSecs := make([]float64, nb)
		for _, r := range rows {
			if r.RecordedAt.Before(start) || r.Secs == nil || r.DeltaDownRt == nil {
				continue
			}
			k := int(r.RecordedAt.Sub(start) / bd)
			if k < 0 || k >= nb {
				continue
			}
			sumBytes[k][0] += float64(*r.DeltaDownRt)
			sumBytes[k][1] += float64(*r.DeltaDownQuick)
			sumBytes[k][2] += float64(*r.DeltaDownHeavy)
			sumSecs[k] += *r.Secs
		}
		for k := 0; k < nb; k++ {
			pt := SmartPoint{At: start.Add(time.Duration(k) * bd).UTC().Format(time.RFC3339), Down: []*float64{nil, nil, nil}}
			if sumSecs[k] > 0 {
				var tot float64
				for i := 0; i < 3; i++ {
					v := round2(sumBytes[k][i] * 8 / sumSecs[k] / 1e6)
					pt.Down[i] = &v
					tot += v
				}
				tot = round2(tot)
				pt.Total = &tot
			}
			sz.Points = append(sz.Points, pt)
		}

		switch {
		case !zone.SmartSpeed:
			sz.Status, sz.StatusText = "off", "Switched off"
		case !active:
			sz.Status, sz.StatusText = "needs_speed", "Needs the line speed"
		case sz.Running:
			sz.Status, sz.StatusText = "running", "Running"
		case !w.Online:
			sz.Status, sz.StatusText = "waiting", "Waiting for the router to come online"
		default:
			sz.Status, sz.StatusText = "waiting", "Waiting for the router's first figures"
		}
		out.Zones = append(out.Zones, sz)
	}
	return out, nil
}
