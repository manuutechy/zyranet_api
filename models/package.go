package models

import (
	"time"

	"gorm.io/gorm"
)

// Package represents an internet service plan (hotspot or PPPoE).
// Named ISPPackage to avoid collision with Go's built-in "package" keyword.
type Package struct {
	ID                 uint           `gorm:"primaryKey" json:"id"`
	ZoneID             uint           `gorm:"not null" json:"zone_id"`
	Name               string         `gorm:"size:255;not null" json:"name"`
	Type               string         `gorm:"size:20;not null" json:"type"` // hotspot | pppoe
	Price              float64        `gorm:"type:decimal(10,2);not null" json:"price"`
	TimeLimitMinutes   *int           `json:"time_limit_minutes"`
	DataLimitMB        *int           `json:"data_limit_mb"`
	SpeedUploadKbps    int            `gorm:"not null" json:"speed_upload_kbps"`
	SpeedDownloadKbps  int            `gorm:"not null" json:"speed_download_kbps"`
	BillingCycle       string         `gorm:"size:20;not null" json:"billing_cycle"` // hourly|daily|weekly|monthly
	Status             string         `gorm:"size:20;default:active" json:"status"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	DeletedAt          gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Zone *Zone `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
}

func (Package) TableName() string { return "packages" }
