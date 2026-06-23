package models

import (
	"time"

	"gorm.io/gorm"
)

// Zone represents a geographic area with its own MikroTik router.
type Zone struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	Name           string         `gorm:"size:255;not null" json:"name"`
	Location       string         `gorm:"size:255;not null" json:"location"`
	Description    *string        `gorm:"type:text" json:"description"`
	RouterName     string         `gorm:"size:255;not null" json:"router_name"`
	RouterIP       string         `gorm:"size:45;not null" json:"router_ip"`
	ConnectionType string         `gorm:"size:10;default:api" json:"connection_type"` // api | rest
	RouterPort     int            `gorm:"default:8728" json:"router_port"`
	RouterUsername *string        `gorm:"size:255" json:"router_username"`
	RouterPassword *string        `gorm:"type:text" json:"router_password"`
	RouterUseSSL   bool           `gorm:"default:false" json:"router_use_ssl"`
	LanPorts       string         `gorm:"size:255;default:ether2,ether3,ether4" json:"lan_ports"`
	HotspotAddress string         `gorm:"size:45;default:10.5.50.1/24" json:"hotspot_address"`
	ManagerID      *uint          `json:"manager_id"`
	Status         string         `gorm:"size:20;default:active" json:"status"`
	LastSeenAt     *time.Time     `json:"last_seen_at"`
	LastStatus     string         `gorm:"size:20;default:unknown" json:"last_status"` // online | offline | unknown
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Manager  *User     `gorm:"foreignKey:ManagerID" json:"manager,omitempty"`
	Packages []Package `gorm:"foreignKey:ZoneID" json:"packages,omitempty"`
}

func (Zone) TableName() string { return "zones" }
