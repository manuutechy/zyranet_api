package models

import (
	"time"

	"gorm.io/gorm"
)

// Customer represents a PPPoE or Hotspot internet subscriber.
type Customer struct {
	ID            uint           `gorm:"primaryKey" json:"id"`
	Name          string         `gorm:"size:255;not null" json:"name"`
	Phone         string         `gorm:"size:20;not null" json:"phone"`
	Email         *string        `gorm:"size:255" json:"email"`
	ZoneID        uint           `gorm:"not null" json:"zone_id"`
	PackageID     uint           `gorm:"not null" json:"package_id"`
	Type          string         `gorm:"size:20;not null" json:"type"` // hotspot | pppoe
	PPPoEUsername *string        `gorm:"size:255;column:pppoe_username" json:"pppoe_username"`
	PPPoEPassword *string        `gorm:"size:255;column:pppoe_password" json:"pppoe_password"`
	Status        string         `gorm:"size:20;default:active" json:"status"` // active|suspended|expired
	ExpiresAt     *time.Time     `json:"expires_at"`
	CreditBalance float64        `gorm:"type:decimal(10,2);default:0" json:"credit_balance"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	DeletedAt     gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Zone    *Zone    `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
	Package *Package `gorm:"foreignKey:PackageID" json:"package,omitempty"`
}

func (Customer) TableName() string { return "customers" }
