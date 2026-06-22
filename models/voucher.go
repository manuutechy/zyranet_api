package models

import "time"

// Voucher represents a hotspot internet access code.
type Voucher struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	Code       string     `gorm:"size:20;uniqueIndex;not null" json:"code"`
	ZoneID     uint       `gorm:"not null" json:"zone_id"`
	PackageID  uint       `gorm:"not null" json:"package_id"`
	Type       string     `gorm:"size:20;default:single_use" json:"type"` // single_use | multi_use
	UsageLimit *int       `gorm:"default:1" json:"usage_limit"`
	UsageCount int        `gorm:"default:0" json:"usage_count"`
	Status     string     `gorm:"size:20;default:unused" json:"status"` // unused|active|expired|depleted
	UsedBy     *uint      `json:"used_by"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	Zone     *Zone     `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
	Package  *Package  `gorm:"foreignKey:PackageID" json:"package,omitempty"`
	Customer *Customer `gorm:"foreignKey:UsedBy" json:"customer,omitempty"`
}

func (Voucher) TableName() string { return "vouchers" }
