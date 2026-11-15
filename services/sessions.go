package services

import (
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Internet sessions are NOT kept in our own `sessions` table. Nothing ever
// wrote to it (only the demo seeder did), so every "sessions" figure built on
// it was always 0. The real records are:
//
//   - radacct (FreeRADIUS accounting, joined to radius_accounts for the
//     customer and zone) for zones in RADIUS mode — one row per session with
//     start, stop, bytes, IP and MAC;
//   - the router heartbeat's connected-clients count (zone_stats) for any
//     zone, including API-mode zones that have no per-session record.
//
// The helpers below read those. The old table is left in the database
// (nothing is dropped) but is no longer read or written.

// radacctReady: FreeRADIUS's accounting table exists on this server.
func radacctReady(db *gorm.DB) bool {
	return RadiusTablesExist() && db.Migrator().HasTable("radacct")
}

// OpenRadiusSessions counts sessions still open in radacct per zone,
// optionally only for customers of one type (hotspot | pppoe).
func OpenRadiusSessions(db *gorm.DB, zoneIDs []uint, customerType string) map[uint]int64 {
	out := map[uint]int64{}
	if len(zoneIDs) == 0 || !radacctReady(db) {
		return out
	}
	q := db.Table("radacct AS ra").
		Joins("JOIN radius_accounts a ON a.username = ra.username").
		Where("ra.acctstoptime IS NULL AND a.zone_id IN ?", zoneIDs)
	if customerType != "" {
		q = q.Joins("JOIN customers c ON c.id = a.customer_id").Where("c.type = ?", customerType)
	}
	var rows []struct {
		ZoneID uint
		N      int64
	}
	q.Select("a.zone_id AS zone_id, COUNT(*) AS n").Group("a.zone_id").Scan(&rows)
	for _, r := range rows {
		out[r.ZoneID] = r.N
	}
	return out
}

// RadiusSessionsStarted counts radacct sessions that started in [from, to)
// in the given zones, optionally for one customer type.
func RadiusSessionsStarted(db *gorm.DB, zoneIDs []uint, customerType string, from, to time.Time) int64 {
	if len(zoneIDs) == 0 || !radacctReady(db) {
		return 0
	}
	q := db.Table("radacct AS ra").
		Joins("JOIN radius_accounts a ON a.username = ra.username").
		Where("ra.acctstarttime >= ? AND ra.acctstarttime < ? AND a.zone_id IN ?", from, to, zoneIDs)
	if customerType != "" {
		q = q.Joins("JOIN customers c ON c.id = a.customer_id").Where("c.type = ?", customerType)
	}
	var n int64
	q.Count(&n)
	return n
}

// HeartbeatClients is the connected-clients count from a zone's latest
// router check-in, if that check-in is recent enough to mean "now".
func HeartbeatClients(db *gorm.DB, zoneID uint, fresh time.Duration) (int64, bool) {
	var st models.ZoneStat
	if err := db.Where("zone_id = ? AND recorded_at >= ?", zoneID, time.Now().Add(-fresh)).
		Order("recorded_at DESC").First(&st).Error; err != nil {
		return 0, false
	}
	return int64(st.ConnectedClients), true
}

// OnlineNow is the best real "devices online" figure for a zone: the larger
// of open RADIUS sessions and the last fresh heartbeat count.
func OnlineNow(db *gorm.DB, zoneID uint) int64 {
	n := OpenRadiusSessions(db, []uint{zoneID}, "")[zoneID]
	if hb, ok := HeartbeatClients(db, zoneID, 10*time.Minute); ok && hb > n {
		n = hb
	}
	return n
}

// CustomerSession is one internet session of a customer, from radacct. Its
// JSON matches the old models.Session so the admin's list keeps working.
type CustomerSession struct {
	ID              uint       `json:"id"`
	CustomerID      uint       `json:"customer_id"`
	ZoneID          uint       `json:"zone_id"`
	MacAddress      *string    `json:"mac_address"`
	IPAddress       *string    `json:"ip_address"`
	StartedAt       *time.Time `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	DataUsedMB      int64      `json:"data_used_mb"`
	DurationMinutes int64      `json:"duration_minutes"`
	Source          string     `json:"source"` // radius
}

// CustomerSessions lists a customer's most recent sessions (newest first).
// Empty when the customer's zone isn't in RADIUS mode: API-mode routers keep
// no per-session record we can read.
func CustomerSessions(db *gorm.DB, customerID uint, limit int) []CustomerSession {
	out := []CustomerSession{}
	if !radacctReady(db) {
		return out
	}
	var rows []struct {
		ID         uint
		CustomerID uint
		ZoneID     uint
		Mac        *string
		IP         *string
		StartedAt  *time.Time
		StoppedAt  *time.Time
		Seconds    int64
		Octets     int64
	}
	db.Raw(`SELECT ra.radacctid AS id, a.customer_id, a.zone_id, ra.callingstationid AS mac, ra.framedipaddress AS ip,
		ra.acctstarttime AS started_at, ra.acctstoptime AS stopped_at,
		COALESCE(ra.acctsessiontime, 0) AS seconds,
		COALESCE(ra.acctinputoctets, 0) + COALESCE(ra.acctoutputoctets, 0) AS octets
		FROM radacct ra JOIN radius_accounts a ON a.username = ra.username
		WHERE a.customer_id = ? ORDER BY ra.radacctid DESC LIMIT ?`, customerID, limit).Scan(&rows)
	for _, r := range rows {
		out = append(out, CustomerSession{
			ID: r.ID, CustomerID: r.CustomerID, ZoneID: r.ZoneID, MacAddress: r.Mac, IPAddress: r.IP,
			StartedAt: r.StartedAt, EndedAt: r.StoppedAt,
			DataUsedMB: r.Octets / (1024 * 1024), DurationMinutes: r.Seconds / 60, Source: "radius",
		})
	}
	return out
}
