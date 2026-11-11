package services

import (
	"math"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// ZoneWatch is one router card on Network › Watch.
type ZoneWatch struct {
	ZoneID         uint    `json:"zone_id"`
	Name           string  `json:"name"`
	Location       string  `json:"location"`
	Online         bool    `json:"online"`
	LastSeenAt     *string `json:"last_seen_at"`  // RFC3339
	LastSeenEAT    string  `json:"last_seen_eat"` // "14:05", "Yesterday 22:10", "3 Nov 09:00"
	UplinkDownMbps int     `json:"uplink_down_mbps"`
	UplinkUpMbps   int     `json:"uplink_up_mbps"`
	OltIP          string  `json:"olt_ip"`
	// HasLineData: this router has sent line figures in the last 24 h (it runs
	// the newer heartbeat). False → "re-run the setup script".
	HasLineData   bool     `json:"has_line_data"`
	LastLineAt    *string  `json:"last_line_at"`
	DownMbps      *float64 `json:"down_mbps"` // now (last reading, if under 5 min old)
	UpMbps        *float64 `json:"up_mbps"`
	DownPct       *float64 `json:"down_pct"`
	UpPct         *float64 `json:"up_pct"`
	LinePct       *float64 `json:"line_pct"` // the busier direction
	PeakTodayPct  *float64 `json:"peak_today_pct"`
	PeakTodayAt   string   `json:"peak_today_at"` // HH:MM EAT
	PeakTodayMbps *float64 `json:"peak_today_mbps"`
	LineStatus    string   `json:"line_status"` // ok | full
	FullSince     *string  `json:"full_since"`
	PPPoE         *int     `json:"pppoe"`
	Hotspot       *int     `json:"hotspot"`
	CPU           *int     `json:"cpu"`
	MemPct        *int     `json:"mem_pct"`
	OltStatus     string   `json:"olt_status"` // none (no OLT set) | unknown | up | down
	OltMs         *float64 `json:"olt_ms"`
	OltDownSince  *string  `json:"olt_down_since"`
}

type ZonesWatchReport struct {
	Zones       []ZoneWatch `json:"zones"`
	FullPct     float64     `json:"full_pct"`
	ClearPct    float64     `json:"clear_pct"`
	GeneratedAt string      `json:"generated_at_eat"`
}

func rfc(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// BuildZonesWatch lists the ISP's active routers (one zone if zoneID set).
func BuildZonesWatch(db *gorm.DB, orgID uint, zoneID *uint, now time.Time) (*ZonesWatchReport, error) {
	q := db.Where("organization_id = ? AND status = ?", orgID, "active")
	if zoneID != nil {
		q = q.Where("id = ?", *zoneID)
	}
	var zones []models.Zone
	if err := q.Order("name ASC").Find(&zones).Error; err != nil {
		return nil, err
	}
	out := &ZonesWatchReport{Zones: []ZoneWatch{}, FullPct: LineFullPct, ClearPct: LineClearPct, GeneratedAt: now.In(EAT).Format("15:04")}
	dayStart := StartOfDayEAT(now)
	for i := range zones {
		z := &zones[i]
		w := ZoneWatch{
			ZoneID: z.ID, Name: z.Name, Location: z.Location,
			UplinkDownMbps: z.UplinkDownMbps, UplinkUpMbps: z.UplinkUpMbps,
			OltIP: zoneOltIP(z), LineStatus: "ok", OltStatus: "none",
		}
		if z.LastSeenAt != nil {
			w.Online = now.Sub(*z.LastSeenAt) <= RouterOfflineAfter
			w.LastSeenAt = rfc(z.LastSeenAt)
			w.LastSeenEAT = formatEATStamp(*z.LastSeenAt, now)
		}

		var lastRows []models.ZoneLineStat
		db.Where("zone_id = ? AND recorded_at >= ?", z.ID, dbT(now.Add(-24*time.Hour))).
			Order("recorded_at DESC").Limit(1).Find(&lastRows)
		if len(lastRows) == 1 {
			last := lastRows[0]
			w.HasLineData = true
			w.LastLineAt = rfc(&last.RecordedAt)
			w.PPPoE, w.Hotspot, w.CPU, w.MemPct = last.PPPoE, last.Hotspot, last.CPU, last.MemPct
			if now.Sub(last.RecordedAt) <= lineDataFresh {
				// "Now" = the newest reading that has a speed (the one just
				// after a reboot has none).
				var sps []models.ZoneLineStat
				db.Where("zone_id = ? AND recorded_at >= ? AND down_mbps IS NOT NULL", z.ID, dbT(now.Add(-lineDataFresh))).
					Order("recorded_at DESC").Limit(1).Find(&sps)
				if len(sps) == 1 {
					sp := sps[0]
					w.DownMbps, w.UpMbps, w.DownPct, w.UpPct = sp.DownMbps, sp.UpMbps, sp.DownPct, sp.UpPct
					w.LinePct = maxPct(sp.DownPct, sp.UpPct)
				}
				if w.OltIP != "" && last.OltOK != nil {
					w.OltMs = last.OltMs
				}
			}
			// Peak today (EAT).
			var today []models.ZoneLineStat
			db.Select("recorded_at", "down_mbps", "up_mbps", "down_pct", "up_pct").
				Where("zone_id = ? AND recorded_at >= ? AND down_mbps IS NOT NULL", z.ID, dbT(dayStart)).Find(&today)
			for _, r := range today {
				p := maxPct(r.DownPct, r.UpPct)
				mb := math.Max(deref(r.DownMbps), deref(r.UpMbps))
				better := false
				if p != nil {
					better = w.PeakTodayPct == nil || *p > *w.PeakTodayPct
				} else if w.PeakTodayPct == nil {
					better = w.PeakTodayMbps == nil || mb > *w.PeakTodayMbps
				}
				if better {
					if p != nil {
						v := *p
						w.PeakTodayPct = &v
					}
					m := mb
					w.PeakTodayMbps = &m
					w.PeakTodayAt = r.RecordedAt.In(EAT).Format("15:04")
				}
			}
		} else {
			// Older script: CPU/memory/hotspot still come from zone_stats.
			var zss []models.ZoneStat
			db.Where("zone_id = ?", z.ID).Order("recorded_at DESC").Limit(1).Find(&zss)
			if len(zss) == 1 && now.Sub(zss[0].RecordedAt) <= lineDataFresh {
				zs := zss[0]
				cpu, hs := zs.CPULoad, zs.ConnectedClients
				w.CPU, w.Hotspot = &cpu, &hs
				if zs.MemoryTotalMB > 0 {
					m := int(float64(zs.MemoryUsedMB)/float64(zs.MemoryTotalMB)*100 + 0.5)
					w.MemPct = &m
				}
			}
		}

		var sts []models.ZoneWatchState
		db.Where("zone_id = ?", z.ID).Limit(1).Find(&sts)
		if len(sts) == 1 {
			st := sts[0]
			w.LineStatus = st.LineStatus
			if st.LineStatus == "full" {
				w.FullSince = rfc(st.FullSince)
			}
			if w.OltIP != "" {
				w.OltStatus = st.OltStatus
				if st.OltStatus == "down" {
					w.OltDownSince = rfc(st.OltDownSince)
				}
			}
		} else if w.OltIP != "" {
			w.OltStatus = "unknown"
		}
		out.Zones = append(out.Zones, w)
	}
	return out, nil
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// WatchPoint is one chart bucket (max within the bucket; nil = no reading).
type WatchPoint struct {
	At       string   `json:"at"` // RFC3339 bucket start
	DownMbps *float64 `json:"down_mbps"`
	UpMbps   *float64 `json:"up_mbps"`
	DownPct  *float64 `json:"down_pct"`
	UpPct    *float64 `json:"up_pct"`
	PPPoE    *int     `json:"pppoe"`
	Hotspot  *int     `json:"hotspot"`
	OltOK    *bool    `json:"olt_ok"` // false if any ping in the bucket failed
	OltMs    *float64 `json:"olt_ms"`
}

type ZoneWatchSeries struct {
	ZoneID         uint         `json:"zone_id"`
	Name           string       `json:"name"`
	Hours          int          `json:"hours"`
	BucketMinutes  int          `json:"bucket_minutes"`
	UplinkDownMbps int          `json:"uplink_down_mbps"`
	UplinkUpMbps   int          `json:"uplink_up_mbps"`
	OltIP          string       `json:"olt_ip"`
	Readings       int          `json:"readings"`
	Points         []WatchPoint `json:"points"`
}

// BuildZoneWatchSeries buckets the last `hours` of readings (≤ 288 points).
func BuildZoneWatchSeries(db *gorm.DB, zone *models.Zone, hours int, now time.Time) (*ZoneWatchSeries, error) {
	bucket := int(math.Ceil(float64(hours*60) / 288))
	if bucket < 1 {
		bucket = 1
	}
	bd := time.Duration(bucket) * time.Minute
	end := now.Truncate(bd).Add(bd)
	start := end.Add(-time.Duration(hours) * time.Hour)
	var rows []models.ZoneLineStat
	if err := db.Where("zone_id = ? AND recorded_at >= ? AND recorded_at < ?", zone.ID, dbT(start), dbT(end)).
		Order("recorded_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	n := int(end.Sub(start) / bd)
	pts := make([]WatchPoint, n)
	msSum := make([]float64, n)
	msN := make([]int, n)
	for i := range pts {
		pts[i].At = start.Add(time.Duration(i) * bd).UTC().Format(time.RFC3339)
	}
	maxF := func(dst **float64, v *float64) {
		if v != nil && (*dst == nil || *v > **dst) {
			x := *v
			*dst = &x
		}
	}
	maxI := func(dst **int, v *int) {
		if v != nil && (*dst == nil || *v > **dst) {
			x := *v
			*dst = &x
		}
	}
	for _, r := range rows {
		i := int(r.RecordedAt.Sub(start) / bd)
		if i < 0 || i >= n {
			continue
		}
		p := &pts[i]
		maxF(&p.DownMbps, r.DownMbps)
		maxF(&p.UpMbps, r.UpMbps)
		maxF(&p.DownPct, r.DownPct)
		maxF(&p.UpPct, r.UpPct)
		maxI(&p.PPPoE, r.PPPoE)
		maxI(&p.Hotspot, r.Hotspot)
		if r.OltOK != nil {
			ok := *r.OltOK && (p.OltOK == nil || *p.OltOK)
			p.OltOK = &ok
			if r.OltMs != nil {
				msSum[i] += *r.OltMs
				msN[i]++
			}
		}
	}
	for i := range pts {
		if msN[i] > 0 {
			v := math.Round(msSum[i]/float64(msN[i])*10) / 10
			pts[i].OltMs = &v
		}
	}
	return &ZoneWatchSeries{
		ZoneID: zone.ID, Name: zone.Name, Hours: hours, BucketMinutes: bucket,
		UplinkDownMbps: zone.UplinkDownMbps, UplinkUpMbps: zone.UplinkUpMbps, OltIP: zoneOltIP(zone),
		Readings: len(rows), Points: pts,
	}, nil
}
