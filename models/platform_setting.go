package models

import "time"

// PlatformSetting is a small key-value store for platform-wide (not
// per-org) SA configuration — e.g. the default commission percentage used
// to estimate Zyra Net's earnings on the Overview dashboard.
type PlatformSetting struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Key       string    `gorm:"size:255;uniqueIndex;not null" json:"key"`
	Value     *string   `gorm:"type:text" json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PlatformSetting) TableName() string { return "platform_settings" }
