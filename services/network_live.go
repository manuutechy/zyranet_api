package services

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Network › Live: the whole network on one screen, refreshed every few
// seconds by the admin. Everything here is read from what routers actually
// reported (zone_line_stats, zone_stats, last_seen_at) and what customers
// actually hold (customers.status/expires_at). Nothing is estimated: no
// reading = null, never 0.

type LiveSpeed struct {
	DownMbps       *float64 `json:"down_mbps"` // sum of routers with a fresh reading
	UpMbps         *float64 `json:"up_mbps"`
	UplinkDownMbps int      `json:"uplink_down_mbps"` // sum of those routers' saved line speeds
	UplinkUpMbps   int      `json:"uplink_up_mbps"`
	DownPct        *float64 `json:"down_pct"`
	UpPct          *float64 `json:"up_pct"`
	Reporting      int      `json:"reporting"` // routers in the figure
	PeakTodayMbps  *float64 `json:"peak_today_mbps"`
	PeakTodayAt    string   `json:"peak_today_at"` // HH:MM EAT
}

type LiveCounts struct {
	RoutersTotal   int  `json:"routers_total"`
	RoutersOnline  int  `json:"routers_online"`
	PPPoE          *int `json:"pppoe"`   // home sessions on routers now
	Hotspot        *int `json:"hotspot"` // hotspot devices on routers now
	ActivePlans    int  `json:"active_plans"`
	HomesOn        int  `json:"homes_on"`
	HomesExpired   int  `json:"homes_expired"`
	HomesOff       int  `json:"homes_off"` // switched off by staff
	HotspotActive  int  `json:"hotspot_active"`
	ExpiringNextHr int  `json:"expiring_next_hour"`
}

// LiveMinute is one minute of the whole network (sum across routers).
type LiveMinute struct {
	At       string   `json:"at"`
	DownMbps *float64 `json:"down_mbps"`
	UpMbps   *float64 `json:"up_mbps"`
	Sessions *int     `json:"sessions"`
	Routers  int      `json:"routers"` // routers that reported in this minute
}

// LiveUptime is one router's last 24 h as 48 half-hour slots:
// 1 = it checked in, 0 = silent, -1 = before it was ever seen.
type LiveUptime struct {
	ZoneID uint     `json:"zone_id"`
	Name   string   `json:"name"`
	Online bool     `json:"online"`
	Slots  []int8   `json:"slots"`
	Pct    *float64 `json:"pct"` // share of known slots it was up
}

type LiveShare struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type NetworkLive struct {
	GeneratedAt   string       `json:"generated_at"`
	GeneratedEAT  string       `json:"generated_at_eat"`
	Speed         LiveSpeed    `json:"speed"`
	Counts        LiveCounts   `json:"counts"`
	Minutes       []LiveMinute `json:"minutes"`
	Uptime        []LiveUptime `json:"uptime"`
	SlotStart     string       `json:"slot_start"`
	ByZone        []LiveShare  `json:"by_zone"`    // active plans per router
	ByPackage     []LiveShare  `json:"by_package"` // active plans per package
	BySpeed       []LiveShare  `json:"by_speed"`   // active plans per download speed
	ByArea        []LiveShare  `json:"by_area"`    // active plans per area
	ByType        []LiveShare  `json:"by_type"`    // homes vs hotspot
	HomesByStatus []LiveShare  `json:"homes_by_status"`
	Zones         []ZoneWatch  `json:"zones"`
}

const (
	liveSlot     = 30 * time.Minute
	liveSlots    = 48
	liveMaxShare = 8 // longer lists fold the tail into "Others"
)

// BuildNetworkLive: zoneIDs nil = the whole ISP, else only those zones.
// minutes = how far back the speed series goes (15–360).
func BuildNetworkLive(db *gorm.DB, orgID uint, zoneIDs []uint, minutes int, now time.Time) (*NetworkLive, error) {
	if minutes < 15 {
		minutes = 15
	}
	if minutes > 360 {
		minutes = 360
	}
	watch, err := BuildZonesWatchIn(db, orgID, zoneIDs, now)
	if err != nil {
		return nil, err
	}
	out := &NetworkLive{
		GeneratedAt:  now.UTC().Format(time.RFC3339),
		GeneratedEAT: now.In(EAT).Format("15:04:05"),
		Zones:        watch.Zones,
		Minutes:      []LiveMinute{},
		Uptime:       []LiveUptime{},
	}
	ids := make([]uint, 0, len(watch.Zones))
	names := map[uint]string{}
	for _, z := range watch.Zones {
		ids = append(ids, z.ZoneID)
		names[z.ZoneID] = z.Name
	}

	// ---- Now: speed and sessions (fresh readings only) ----
	c := &out.Counts
	sp := &out.Speed
	c.RoutersTotal = len(watch.Zones)
	var down, up float64
	var pppoe, hotspot int
	havePPP, haveHS := false, false
	for _, z := range watch.Zones {
		if z.Online {
			c.RoutersOnline++
		}
		if z.DownMbps != nil && z.UpMbps != nil {
			sp.Reporting++
			down += *z.DownMbps
			up += *z.UpMbps
			sp.UplinkDownMbps += z.UplinkDownMbps
			sp.UplinkUpMbps += z.UplinkUpMbps
		}
		if !z.Online {
			continue // sessions from a dead router aren't "now"
		}
		if z.PPPoE != nil {
			pppoe += *z.PPPoE
			havePPP = true
		}
		if z.Hotspot != nil {
			hotspot += *z.Hotspot
			haveHS = true
		}
	}
	if sp.Reporting > 0 {
		d, u := round2(down), round2(up)
		sp.DownMbps, sp.UpMbps = &d, &u
		sp.DownPct = LinePct(d, sp.UplinkDownMbps)
		sp.UpPct = LinePct(u, sp.UplinkUpMbps)
	}
	if havePPP {
		c.PPPoE = &pppoe
	}
	if haveHS {
		c.Hotspot = &hotspot
	}

	if len(ids) == 0 {
		return out, nil
	}

	// ---- Per-minute series, summed across routers ----
	bd := time.Minute
	end := now.Truncate(bd).Add(bd)
	start := end.Add(-time.Duration(minutes) * time.Minute)
	dayStart := StartOfDayEAT(now)
	from := start
	if dayStart.Before(from) {
		from = dayStart // the same rows give today's peak
	}
	var rows []models.ZoneLineStat
	if err := db.Select("zone_id", "recorded_at", "down_mbps", "up_mbps", "pppoe", "hotspot").
		Where("zone_id IN ? AND recorded_at >= ? AND recorded_at < ?", ids, dbT(from), dbT(end)).
		Order("recorded_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	n := int(end.Sub(start) / bd)
	type cell struct {
		down, up *float64
		sess     *int
	}
	grid := make([]map[uint]cell, n)
	// Today's network peak: sum per minute across routers.
	dayDown := map[int64]float64{}
	for _, r := range rows {
		if !r.RecordedAt.Before(dayStart) && r.DownMbps != nil {
			k := r.RecordedAt.Truncate(time.Minute).Unix()
			dayDown[k] += *r.DownMbps
		}
		i := int(r.RecordedAt.Sub(start) / bd)
		if r.RecordedAt.Before(start) || i < 0 || i >= n {
			continue
		}
		if grid[i] == nil {
			grid[i] = map[uint]cell{}
		}
		cl := grid[i][r.ZoneID] // the latest reading in the minute wins
		if r.DownMbps != nil {
			cl.down, cl.up = r.DownMbps, r.UpMbps
		}
		if r.PPPoE != nil || r.Hotspot != nil {
			s := 0
			if r.PPPoE != nil {
				s += *r.PPPoE
			}
			if r.Hotspot != nil {
				s += *r.Hotspot
			}
			cl.sess = &s
		}
		grid[i][r.ZoneID] = cl
	}
	out.Minutes = make([]LiveMinute, n)
	for i := 0; i < n; i++ {
		m := LiveMinute{At: start.Add(time.Duration(i) * bd).UTC().Format(time.RFC3339)}
		var d, u float64
		var s int
		hasSpeed, hasSess := false, false
		for _, cl := range grid[i] {
			m.Routers++
			if cl.down != nil {
				d += *cl.down
				hasSpeed = true
			}
			if cl.up != nil {
				u += *cl.up
			}
			if cl.sess != nil {
				s += *cl.sess
				hasSess = true
			}
		}
		if hasSpeed {
			d, u = round2(d), round2(u)
			m.DownMbps, m.UpMbps = &d, &u
		}
		if hasSess {
			m.Sessions = &s
		}
		out.Minutes[i] = m
	}
	var peakK int64
	for k, v := range dayDown {
		if sp.PeakTodayMbps == nil || v > *sp.PeakTodayMbps {
			x := round2(v)
			sp.PeakTodayMbps, peakK = &x, k
		}
	}
	if sp.PeakTodayMbps != nil {
		sp.PeakTodayAt = time.Unix(peakK, 0).In(EAT).Format("15:04")
	}

	// ---- 24 h online strips ----
	slotEnd := now.Truncate(liveSlot).Add(liveSlot)
	slotStart := slotEnd.Add(-liveSlots * liveSlot)
	out.SlotStart = slotStart.UTC().Format(time.RFC3339)
	seen := map[uint][]bool{}
	for _, id := range ids {
		seen[id] = make([]bool, liveSlots)
	}
	mark := func(zone uint, at time.Time) {
		i := int(at.Sub(slotStart) / liveSlot)
		if s, ok := seen[zone]; ok && i >= 0 && i < liveSlots {
			s[i] = true
		}
	}
	type ping struct {
		ZoneID     uint
		RecordedAt time.Time
	}
	var pings, inWin []ping
	db.Model(&models.ZoneLineStat{}).Select("zone_id", "recorded_at").
		Where("zone_id IN ? AND recorded_at >= ?", ids, dbT(slotStart)).Scan(&pings)
	for _, p := range pings {
		mark(p.ZoneID, p.RecordedAt)
		inWin = append(inWin, p)
	}
	pings = pings[:0]
	db.Model(&models.ZoneStat{}).Select("zone_id", "recorded_at").
		Where("zone_id IN ? AND recorded_at >= ?", ids, dbT(slotStart)).Scan(&pings)
	for _, p := range pings {
		mark(p.ZoneID, p.RecordedAt)
		inWin = append(inWin, p)
	}
	// Slots before a router's first ever reading are "not set up yet",
	// not downtime. Routers with readings older than the window are known
	// for every slot; the others start at their first reading in it.
	first := map[uint]time.Time{}
	note := func(zone uint, at time.Time) {
		if t, ok := first[zone]; !ok || at.Before(t) {
			first[zone] = at
		}
	}
	for _, tbl := range []interface{}{&models.ZoneLineStat{}, &models.ZoneStat{}} {
		var before []uint
		db.Model(tbl).Where("zone_id IN ? AND recorded_at < ?", ids, dbT(slotStart)).Distinct().Pluck("zone_id", &before)
		for _, id := range before {
			note(id, slotStart.Add(-time.Second))
		}
	}
	for _, p := range inWin {
		note(p.ZoneID, p.RecordedAt)
	}
	for _, z := range watch.Zones {
		u := LiveUptime{ZoneID: z.ZoneID, Name: z.Name, Online: z.Online, Slots: make([]int8, liveSlots)}
		fs, everSeen := first[z.ZoneID]
		known, up := 0, 0
		for i := 0; i < liveSlots; i++ {
			slotT := slotStart.Add(time.Duration(i+1) * liveSlot)
			switch {
			case seen[z.ZoneID][i]:
				u.Slots[i] = 1
				known++
				up++
			case !everSeen || !slotT.After(fs):
				u.Slots[i] = -1
			default:
				known++
			}
		}
		// The current half-hour has only just started: don't call it a gap.
		if last := liveSlots - 1; u.Slots[last] == 0 && z.Online {
			u.Slots[last] = 1
			up++
		}
		if known > 0 {
			p := math.Round(float64(up)/float64(known)*1000) / 10
			u.Pct = &p
		}
		out.Uptime = append(out.Uptime, u)
	}

	// ---- Who holds what (customers) ----
	var custs []struct {
		ZoneID    uint
		PackageID uint
		Type      string
		Status    string
		ExpiresAt *time.Time
	}
	if err := db.Model(&models.Customer{}).Select("zone_id", "package_id", "type", "status", "expires_at").
		Where("zone_id IN ?", ids).Scan(&custs).Error; err != nil {
		return nil, err
	}
	var pkgs []models.Package
	db.Unscoped().Where("zone_id IN ?", ids).Find(&pkgs)
	pkgByID := map[uint]*models.Package{}
	for i := range pkgs {
		pkgByID[pkgs[i].ID] = &pkgs[i]
	}
	var zrows []models.Zone
	db.Select("id", "area_id").Where("id IN ?", ids).Find(&zrows)
	areaOf := map[uint]uint{}
	for _, z := range zrows {
		if z.AreaID != nil {
			areaOf[z.ID] = *z.AreaID
		}
	}
	areaName := map[uint]string{}
	if len(areaOf) > 0 {
		var areas []models.Area
		db.Where("organization_id = ?", orgID).Find(&areas)
		for _, a := range areas {
			areaName[a.ID] = a.Name
		}
	}

	byZone, byPkg, bySpeed, byArea := map[string]*LiveShare{}, map[string]*LiveShare{}, map[string]*LiveShare{}, map[string]*LiveShare{}
	add := func(m map[string]*LiveShare, key, label string) {
		if m[key] == nil {
			m[key] = &LiveShare{Key: key, Label: label}
		}
		m[key].Count++
	}
	nowDB, hourDB := dbT(now), dbT(now.Add(time.Hour))
	for _, cu := range custs {
		active := cu.Status == "active" && cu.ExpiresAt != nil && cu.ExpiresAt.After(nowDB)
		if cu.Type == "pppoe" {
			switch {
			case cu.Status == "suspended":
				c.HomesOff++
			case active:
				c.HomesOn++
			default:
				c.HomesExpired++
			}
		}
		if !active {
			continue
		}
		c.ActivePlans++
		if cu.Type != "pppoe" {
			c.HotspotActive++
		}
		if cu.ExpiresAt.Before(hourDB) {
			c.ExpiringNextHr++
		}
		add(byZone, uintKey(cu.ZoneID), names[cu.ZoneID])
		if p := pkgByID[cu.PackageID]; p != nil {
			add(byPkg, uintKey(p.ID), p.Name)
			add(bySpeed, uintKey(uint(p.SpeedDownloadKbps)), speedLabel(p.SpeedDownloadKbps))
		} else {
			add(byPkg, "0", "Plan removed")
			add(bySpeed, "removed", "Plan removed")
		}
		if a, ok := areaOf[cu.ZoneID]; ok {
			add(byArea, uintKey(a), areaName[a])
		} else {
			add(byArea, "0", "No area")
		}
	}
	out.ByZone = topShares(byZone, false)
	out.ByPackage = topShares(byPkg, false)
	out.BySpeed = topShares(bySpeed, true)
	out.ByArea = topShares(byArea, false)
	if len(byArea) == 1 && byArea["0"] != nil {
		out.ByArea = []LiveShare{} // no areas set up: nothing to show
	}
	out.ByType = []LiveShare{
		{Key: "pppoe", Label: "Homes", Count: c.HomesOn},
		{Key: "hotspot", Label: "Hotspot", Count: c.HotspotActive},
	}
	out.HomesByStatus = []LiveShare{
		{Key: "on", Label: "Paid up", Count: c.HomesOn},
		{Key: "expired", Label: "Ran out", Count: c.HomesExpired},
		{Key: "off", Label: "Switched off", Count: c.HomesOff},
	}
	return out, nil
}

func uintKey(v uint) string { return strconv.FormatUint(uint64(v), 10) }

func speedLabel(kbps int) string {
	if kbps <= 0 {
		return "No limit"
	}
	if kbps < 1024 {
		return strconv.Itoa(kbps) + " kbps"
	}
	return strconv.FormatFloat(math.Round(float64(kbps)/1024*10)/10, 'f', -1, 64) + " Mbps"
}

// topShares sorts biggest first (bySpeedKey: by speed ascending instead) and
// folds anything past liveMaxShare into "Others".
func topShares(m map[string]*LiveShare, bySpeedKey bool) []LiveShare {
	list := make([]LiveShare, 0, len(m))
	for _, s := range m {
		list = append(list, *s)
	}
	if bySpeedKey {
		// Slowest first; "Plan removed" (no speed known) last.
		rank := func(k string) int {
			if n, err := strconv.Atoi(k); err == nil {
				return n
			}
			return math.MaxInt32
		}
		sort.Slice(list, func(i, j int) bool { return rank(list[i].Key) < rank(list[j].Key) })
		return list
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].Label < list[j].Label
	})
	if len(list) > liveMaxShare {
		rest := LiveShare{Key: "others", Label: "Others"}
		for _, s := range list[liveMaxShare-1:] {
			rest.Count += s.Count
		}
		list = append(list[:liveMaxShare-1], rest)
	}
	return list
}
