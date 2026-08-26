package models

import (
	"time"

	"gorm.io/gorm"
)

// CustomerDevice represents a physical device (MAC address) linked to a customer account.
type CustomerDevice struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	CustomerID uint           `gorm:"index;not null" json:"customer_id"`
	MacAddress string         `gorm:"size:45;index;not null" json:"mac_address"`
	IPAddress  *string        `gorm:"size:45" json:"ip_address"`
	DeviceName *string        `gorm:"size:100" json:"device_name"`
	LastSeenAt time.Time      `json:"last_seen_at"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Customer *Customer `gorm:"foreignKey:CustomerID" json:"customer,omitempty"`
}

func (CustomerDevice) TableName() string { return "customer_devices" }
