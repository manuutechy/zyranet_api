package models

import (
	"time"

	"gorm.io/gorm"
)

// Package represents an internet service plan (hotspot or PPPoE).
// Named ISPPackage to avoid collision with Go's built-in "package" keyword.
type Package struct {
	ID                    uint    `gorm:"primaryKey" json:"id"`
	ZoneID                uint    `gorm:"not null" json:"zone_id"`
	Name                  string  `gorm:"size:255;not null" json:"name"`
	Type                  string  `gorm:"size:20;not null" json:"type"`           // hotspot | pppoe
	Category              string  `gorm:"size:20;default:single" json:"category"` // single | multi
	DeviceLimit           int     `gorm:"default:1" json:"device_limit"`
	Price                 float64 `gorm:"type:decimal(10,2);not null" json:"price"`
	IsFreeTier            bool    `gorm:"default:false" json:"is_free_tier"`
	FreeTierCooldownHours int     `gorm:"default:24" json:"free_tier_cooldown_hours"`
	FreeTierStartTime     *string `gorm:"size:10" json:"free_tier_start_time"`       // e.g. "14:00" in 24h format
	FreeTierEndTime       *string `gorm:"size:10" json:"free_tier_end_time"`         // e.g. "18:00" in 24h format
	FreeTierDays          string  `gorm:"size:20;default:all" json:"free_tier_days"` // all | weekdays | weekends
	TimeLimitMinutes      *int    `json:"time_limit_minutes"`
	DataLimitMB           *int    `json:"data_limit_mb"`
	// FairUseKbps: with a data cap, the speed (both ways) after the cap is used
	// up, until the plan ends. Nil/0 = cut off at the cap. RADIUS zones only.
	FairUseKbps       *int   `json:"fair_use_kbps"`
	SpeedUploadKbps   int    `gorm:"not null" json:"speed_upload_kbps"`
	SpeedDownloadKbps int    `gorm:"not null" json:"speed_download_kbps"`
	BillingCycle      string `gorm:"size:20;not null" json:"billing_cycle"` // hourly|daily|weekly|monthly
	Status            string `gorm:"size:20;default:active" json:"status"`
	// AreaPackageID links this plan to its area price-list template; nil for
	// a zone's own plans. AreaOverride = this zone changed the plan itself,
	// so "apply area prices" leaves it alone.
	AreaPackageID *uint          `gorm:"index" json:"area_package_id"`
	AreaOverride  bool           `gorm:"default:false" json:"area_override"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	DeletedAt     gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Zone *Zone `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
}

func (Package) TableName() string { return "packages" }
