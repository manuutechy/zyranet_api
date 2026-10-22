package models

import "time"

// OpsAlertSetting is an ISP's choice of who gets a text when one of its
// routers goes down or comes back. No row (or SmsEnabled=false, or no
// phones) means no SMS at all — alerts still show inside the admin.
type OpsAlertSetting struct {
	ID             uint `gorm:"primaryKey" json:"id"`
	OrganizationID uint `gorm:"not null;uniqueIndex" json:"organization_id"`
	SmsEnabled     bool `gorm:"not null;default:false" json:"sms_enabled"`
	// Phones is a comma-separated list of normalised numbers (2547XXXXXXXX).
	Phones    string    `gorm:"size:255" json:"phones"`
	UpdatedBy *uint     `json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OpsAlertSetting) TableName() string { return "ops_alert_settings" }

// OpsZoneAlertState is what the owner-alert job last knew about one zone's
// router, so every online/offline transition is alerted exactly once (also
// across API restarts) and a flapping router can't text every few minutes.
type OpsZoneAlertState struct {
	ZoneID         uint   `gorm:"primaryKey;autoIncrement:false" json:"zone_id"`
	OrganizationID uint   `gorm:"not null;index" json:"organization_id"`
	Status         string `gorm:"size:10;not null" json:"status"` // online | offline (as last seen by the job)
	// OfflineSince is the router's last check-in before the current outage.
	OfflineSince *time.Time `json:"offline_since"`
	// SmsStatus is what the owner was last told by SMS (online | offline), so
	// a "back online" text only follows a "down" text.
	SmsStatus        string     `gorm:"size:10;not null;default:online" json:"sms_status"`
	LastOfflineSmsAt *time.Time `json:"last_offline_sms_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (OpsZoneAlertState) TableName() string { return "ops_zone_alert_states" }

// OpsAlert is one owner alert: a router went offline, came back, or the
// 06:00 summary of what happened during quiet hours. It is what the admin
// banner shows; Delivery says whether/why an SMS went out.
type OpsAlert struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	OrganizationID uint       `gorm:"not null;index:idx_ops_alerts_org_created,priority:1" json:"organization_id"`
	ZoneID         uint       `gorm:"index" json:"zone_id"` // 0 for a summary
	ZoneName       string     `gorm:"size:255" json:"zone_name"`
	Kind           string     `gorm:"size:20;not null" json:"kind"` // offline | recovered | summary
	Message        string     `gorm:"type:text;not null" json:"message"`
	OfflineSince   *time.Time `json:"offline_since"`
	RecoveredAt    *time.Time `json:"recovered_at"`
	OutageSeconds  int64      `json:"outage_seconds"`
	// CustomersCompensated comes from zone_outages; nil when no outage record
	// exists (outages under 10 minutes aren't compensated).
	CustomersCompensated *int `json:"customers_compensated"`
	// Delivery: sms_sent | sms_failed | sms_off | no_phones | quiet_hours
	// (queued for the 06:00 summary) | in_summary | flap_hold (router keeps
	// dropping; at most one "down" SMS per 2 h) | not_needed.
	Delivery       string     `gorm:"size:20" json:"delivery"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
	AcknowledgedBy *uint      `json:"acknowledged_by"`
	CreatedAt      time.Time  `gorm:"index:idx_ops_alerts_org_created,priority:2" json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (OpsAlert) TableName() string { return "ops_alerts" }
