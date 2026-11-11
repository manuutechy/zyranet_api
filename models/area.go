package models

import (
	"time"

	"gorm.io/gorm"
)

// Area groups zones (router sites) of one ISP — an estate or a part of town.
// An area has its own price list (AreaPackage) that can be pushed to every
// zone in it, its own money report, and its own staff (area managers).
type Area struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	OrganizationID uint           `gorm:"not null;index" json:"organization_id"`
	Name           string         `gorm:"size:255;not null" json:"name"`
	Description    *string        `gorm:"type:text" json:"description"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

func (Area) TableName() string { return "areas" }

// AreaPackage is one plan on an area's price list: a template copied into
// every zone of the area as an ordinary Package (linked back by
// Package.AreaPackageID). The captive portal and payments only ever read the
// zone packages, never these.
type AreaPackage struct {
	ID                uint           `gorm:"primaryKey" json:"id"`
	AreaID            uint           `gorm:"not null;index" json:"area_id"`
	OrganizationID    uint           `gorm:"not null;index" json:"organization_id"`
	Name              string         `gorm:"size:255;not null" json:"name"`
	Type              string         `gorm:"size:20;not null" json:"type"`           // hotspot | pppoe
	Category          string         `gorm:"size:20;default:single" json:"category"` // single | multi
	DeviceLimit       int            `gorm:"default:1" json:"device_limit"`
	Price             float64        `gorm:"type:decimal(10,2);not null" json:"price"`
	TimeLimitMinutes  *int           `json:"time_limit_minutes"`
	DataLimitMB       *int           `json:"data_limit_mb"`
	FairUseKbps       *int           `json:"fair_use_kbps"`
	SpeedUploadKbps   int            `gorm:"not null" json:"speed_upload_kbps"`
	SpeedDownloadKbps int            `gorm:"not null" json:"speed_download_kbps"`
	BillingCycle      string         `gorm:"size:20;not null" json:"billing_cycle"` // hourly|daily|weekly|monthly
	Status            string         `gorm:"size:20;default:active" json:"status"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	DeletedAt         gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

func (AreaPackage) TableName() string { return "area_packages" }

// UserArea gives an area manager (users.role = area_manager) the zones of an
// area. One row per user per area.
type UserArea struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"not null;uniqueIndex:idx_user_area" json:"user_id"`
	AreaID    uint      `gorm:"not null;uniqueIndex:idx_user_area;index" json:"area_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (UserArea) TableName() string { return "user_areas" }
