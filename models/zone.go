package models

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"gorm.io/gorm"
)

// Zone represents a geographic area with its own MikroTik router.
type Zone struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	Name           string     `gorm:"size:255;not null" json:"name"`
	Location       string     `gorm:"size:255;not null" json:"location"`
	Description    *string    `gorm:"type:text" json:"description"`
	RouterName     string     `gorm:"size:255;not null" json:"router_name"`
	RouterIP       string     `gorm:"size:45;not null" json:"router_ip"`
	ConnectionType string     `gorm:"size:10;default:api" json:"connection_type"` // api | rest
	RouterPort     int        `gorm:"default:8728" json:"router_port"`
	RouterUsername *string    `gorm:"size:255" json:"router_username"`
	RouterPassword *string    `gorm:"type:text" json:"router_password"`
	RouterUseSSL   bool       `gorm:"default:false" json:"router_use_ssl"`
	LanPorts       string     `gorm:"size:255;default:ether2,ether3,ether4" json:"lan_ports"`
	HotspotAddress string     `gorm:"size:45;default:10.5.50.1/24" json:"hotspot_address"`
	ManagerID      *uint      `json:"manager_id"`
	OrganizationID uint       `gorm:"not null;index" json:"organization_id"`
	Status         string     `gorm:"size:20;default:active" json:"status"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	LastStatus     string     `gorm:"size:20;default:unknown" json:"last_status"` // online | offline | unknown
	// ProvisionToken gates the unauthenticated /public/zones/* router-provisioning
	// endpoints (setup script, sync script, heartbeat). Those endpoints hand back
	// live customer/voucher credentials and accept router-identity updates, so a
	// guessable numeric zone ID alone must never be enough to reach them. Never
	// exposed in API JSON responses — see UI/admin surfaces that render the
	// provisioning one-liner for the one place it's read back out.
	ProvisionToken string `gorm:"size:64" json:"-"`
	// LegacyRequestAt is the last time this zone's router called a
	// /public/zones/* endpoint without a valid ProvisionToken — i.e. it still
	// runs a pre-token script and needs re-provisioning. Nil = never seen.
	LegacyRequestAt *time.Time `json:"legacy_request_at"`
	// AuthMode is how this zone's router authenticates customers: "api" (the
	// API pushes users/MACs to the router — the original model) or "radius"
	// (the router asks the RADIUS server; the API keeps the RADIUS tables in
	// step). RadiusSecret is the shared secret between this router and the
	// RADIUS server; never exposed in JSON.
	AuthMode     string `gorm:"size:10;default:api" json:"auth_mode"`
	RadiusSecret string `gorm:"size:64" json:"-"`
	// UplinkDownMbps/UplinkUpMbps are what the ISP really gets from upstream,
	// used only to generate the fair-queue tuning script.
	UplinkDownMbps int            `json:"uplink_down_mbps"`
	UplinkUpMbps   int            `json:"uplink_up_mbps"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Manager      *User         `gorm:"foreignKey:ManagerID" json:"manager,omitempty"`
	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Packages     []Package     `gorm:"foreignKey:ZoneID" json:"packages,omitempty"`
}

func (Zone) TableName() string { return "zones" }

// BeforeCreate generates the router-provisioning token.
func (z *Zone) BeforeCreate(tx *gorm.DB) (err error) {
	if z.ProvisionToken == "" {
		z.ProvisionToken, err = GenerateProvisionToken()
	}
	return err
}

// GenerateProvisionToken returns a fresh random router-provisioning token.
func GenerateProvisionToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
