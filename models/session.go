package models

import "time"

// Session is DEPRECATED: nothing in the API writes or reads this table any
// more (it was only ever filled by the demo seeder, so it was always empty).
// Real sessions are RADIUS accounting rows (radacct) and router check-ins —
// see services/sessions.go. Kept so existing databases and cmd/seed still
// build; the table can be dropped by hand once nobody needs the seed data.
type Session struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	CustomerID      uint       `gorm:"not null" json:"customer_id"`
	ZoneID          uint       `gorm:"not null" json:"zone_id"`
	MacAddress      *string    `gorm:"size:20;column:mac_address" json:"mac_address"`
	IPAddress       *string    `gorm:"size:45;column:ip_address" json:"ip_address"`
	StartedAt       time.Time  `gorm:"not null" json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	DataUsedMB      int        `gorm:"default:0;column:data_used_mb" json:"data_used_mb"`
	DurationMinutes int        `gorm:"default:0;column:duration_minutes" json:"duration_minutes"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	Customer *Customer `gorm:"foreignKey:CustomerID" json:"customer,omitempty"`
	Zone     *Zone     `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
}

func (Session) TableName() string { return "sessions" }
