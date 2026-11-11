package models

import "time"

// GrowthPlan is one ISP's growth plan numbers (Reports › Growth). Its own
// table, one row per ISP: the old global `settings` table is shared by every
// ISP (any ISP owner can read and write all of it), so per-ISP business plans
// don't belong there. No row = the owner's defaults.
type GrowthPlan struct {
	ID                   uint      `gorm:"primaryKey" json:"-"`
	OrganizationID       uint      `gorm:"not null;uniqueIndex" json:"-"`
	StartDate            string    `gorm:"size:10" json:"plan_start_date"` // YYYY-MM-DD (EAT), "" = not set
	StartPeople          int       `gorm:"not null;default:200" json:"plan_start_people"`
	PricePerDay          float64   `gorm:"type:decimal(12,2);not null;default:30" json:"plan_price_per_day"`
	FeePercent           float64   `gorm:"type:decimal(6,3);not null;default:11.5" json:"plan_fee_percent"`
	FixedMonthly         float64   `gorm:"type:decimal(14,2);not null;default:25000" json:"plan_fixed_monthly"`
	CostPerPersonMonthly float64   `gorm:"type:decimal(12,2);not null;default:60" json:"plan_cost_per_person_monthly"`
	ReinvestPercent      float64   `gorm:"type:decimal(6,3);not null;default:90" json:"plan_reinvest_percent"`
	CostPerNewPerson     float64   `gorm:"type:decimal(12,2);not null;default:1000" json:"plan_cost_per_new_person"`
	MarketSize           int       `gorm:"not null;default:10000" json:"plan_market_size"`
	UpdatedBy            *uint     `json:"-"`
	CreatedAt            time.Time `json:"-"`
	UpdatedAt            time.Time `json:"-"`
}

func (GrowthPlan) TableName() string { return "growth_plans" }

// ZoneLineStat is one router heartbeat's line reading (Network › Watch):
// the raw WAN byte counters the router sent, the speed worked out from the
// previous reading, sessions and the OLT ping. Fields are nil when the
// router didn't send them (older heartbeat script) or no speed could be
// worked out (first reading, counter reset, long gap).
type ZoneLineStat struct {
	ID         uint      `gorm:"primaryKey" json:"-"`
	ZoneID     uint      `gorm:"not null;index:idx_zone_line_stats_zone_at,priority:1" json:"zone_id"`
	RecordedAt time.Time `gorm:"not null;index:idx_zone_line_stats_zone_at,priority:2;index" json:"recorded_at"`
	RxBytes    *uint64   `json:"-"` // WAN counter: bytes in (download)
	TxBytes    *uint64   `json:"-"` // WAN counter: bytes out (upload)
	DownMbps   *float64  `json:"down_mbps"`
	UpMbps     *float64  `json:"up_mbps"`
	DownPct    *float64  `json:"down_pct"` // of zone.uplink_down_mbps at the time
	UpPct      *float64  `json:"up_pct"`
	PPPoE      *int      `gorm:"column:pppoe" json:"pppoe"`
	Hotspot    *int      `json:"hotspot"`
	OltOK      *bool     `json:"olt_ok"`
	OltMs      *float64  `json:"olt_ms"`
	CPU        *int      `json:"cpu"`
	MemPct     *int      `json:"mem_pct"`
}

func (ZoneLineStat) TableName() string { return "zone_line_stats" }

// ZoneWatchState is what the line/OLT alerting last knew about one zone.
type ZoneWatchState struct {
	ZoneID         uint       `gorm:"primaryKey;autoIncrement:false" json:"zone_id"`
	OrganizationID uint       `gorm:"not null;index" json:"organization_id"`
	LineStatus     string     `gorm:"size:10;not null;default:ok" json:"line_status"` // ok | full
	HighSince      *time.Time `json:"high_since"`                                     // first reading of the current ≥80% run
	LastSampleAt   *time.Time `json:"last_sample_at"`
	FullSince      *time.Time `json:"full_since"`
	LineSms        string     `gorm:"size:10;not null;default:ok" json:"line_sms"` // what the owner was last texted
	LastFullSmsAt  *time.Time `json:"last_full_sms_at"`
	OltStatus      string     `gorm:"size:10;not null;default:unknown" json:"olt_status"` // unknown | up | down
	OltFails       int        `gorm:"not null;default:0" json:"olt_fails"`
	OltDownSince   *time.Time `json:"olt_down_since"`
	OltSms         string     `gorm:"size:10;not null;default:up" json:"olt_sms"`
	LastOltSmsAt   *time.Time `json:"last_olt_sms_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (ZoneWatchState) TableName() string { return "zone_watch_states" }
