package models

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// Zone represents a geographic area with its own MikroTik router.
type Zone struct {
	ID             uint    `gorm:"primaryKey" json:"id"`
	Name           string  `gorm:"size:255;not null" json:"name"`
	Location       string  `gorm:"size:255;not null" json:"location"`
	Description    *string `gorm:"type:text" json:"description"`
	RouterName     string  `gorm:"size:255;not null" json:"router_name"`
	RouterIP       string  `gorm:"size:45;not null" json:"router_ip"`
	ConnectionType string  `gorm:"size:10;default:api" json:"connection_type"` // api | rest
	RouterPort     int     `gorm:"default:8728" json:"router_port"`
	RouterUsername *string `gorm:"size:255" json:"router_username"`
	RouterPassword *string `gorm:"type:text" json:"router_password"`
	RouterUseSSL   bool    `gorm:"default:false" json:"router_use_ssl"`
	LanPorts       string  `gorm:"size:255;default:ether2,ether3,ether4" json:"lan_ports"`
	// WanPort is the router port that goes to the internet (uplink).
	WanPort string `gorm:"size:32;default:ether1" json:"wan_port"`
	// ServiceMode is what the router does for customers:
	//   hotspot — WiFi captive portal (customers pay/log in on the portal)
	//   pppoe   — billing only: PPPoE accounts, no captive portal
	//   both    — hotspot and PPPoE on the same router
	ServiceMode    string     `gorm:"size:10;default:both" json:"service_mode"`
	HotspotAddress string     `gorm:"size:45;default:10.5.50.1/24" json:"hotspot_address"`
	ManagerID      *uint      `json:"manager_id"`
	OrganizationID uint       `gorm:"not null;index" json:"organization_id"`
	Status         string     `gorm:"size:20;default:active" json:"status"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	LastStatus     string     `gorm:"size:20;default:unknown" json:"last_status"` // online | offline | unknown

	// AreaID is the estate/area this zone belongs to (nil = no area).
	AreaID *uint `gorm:"index" json:"area_id"`

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
	// PublicIP is the address the router's last authenticated heartbeat came
	// from. For WireGuard-managed routers RouterIP stays the tunnel address
	// (that is what the API dials); PublicIP shows where the site really is.
	PublicIP string `gorm:"size:45" json:"public_ip"`
	// LastSyncAt is when the router last fetched its sync script. Logins
	// created after it reach the router on the next sync.
	LastSyncAt *time.Time `json:"last_sync_at"`
	// VerifyTLS makes the router's /tool fetch check the API's HTTPS
	// certificate (check-certificate=yes). Off by default: it needs a router
	// with a CA store that trusts our chain, so switch it on per zone once
	// tested on that router — a router that can't verify stops syncing.
	VerifyTLS bool `gorm:"default:false" json:"verify_tls"`
	// AuthMode is how this zone's router authenticates customers: "api" (the
	// API pushes users/MACs to the router — the original model) or "radius"
	// (the router asks the RADIUS server; the API keeps the RADIUS tables in
	// step). RadiusSecret is the shared secret between this router and the
	// RADIUS server; never exposed in JSON.
	AuthMode     string `gorm:"size:10;default:api" json:"auth_mode"`
	RadiusSecret string `gorm:"size:64" json:"-"`
	// UplinkDownMbps/UplinkUpMbps are what the ISP really gets from upstream,
	// used only to generate the fair-queue tuning script.
	UplinkDownMbps int `json:"uplink_down_mbps"`
	UplinkUpMbps   int `json:"uplink_up_mbps"`
	// SmartSpeed puts traffic lanes on the router (games/calls first, big
	// downloads last). Needs the uplink speeds above; on by default.
	SmartSpeed bool           `gorm:"default:true" json:"smart_speed"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	// OltIP is the fibre OLT the router pings each heartbeat (Network › Watch).
	OltIP *string `gorm:"size:45" json:"olt_ip"`

	// TunnelIP is the router's address on the Zyra Net WireGuard tunnel
	// (e.g. 10.200.0.10). Set = every sync makes sure the router has the
	// tunnel and the server's API login, so routers behind NAT can be read
	// live. Nil = no tunnel (the router is reached at RouterIP, if at all).
	TunnelIP *string `gorm:"size:45" json:"tunnel_ip"`
	// TunnelPubKey is the router's WireGuard public key as it last reported
	// it; TunnelPeerAt is when that key was added to the hub. Both set = the
	// tunnel is wired at both ends.
	TunnelPubKey *string    `gorm:"size:64" json:"-"`
	TunnelPeerAt *time.Time `json:"tunnel_peer_at"`

	// brief: loaded through ZoneBrief (see AfterFind); not a column.
	brief bool

	Manager      *User         `gorm:"foreignKey:ManagerID" json:"manager,omitempty"`
	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Packages     []Package     `gorm:"foreignKey:ZoneID" json:"packages,omitempty"`
}

func (Zone) TableName() string { return "zones" }

// MarshalJSON never sends the router admin password back out: every staff
// role (and every payment row that preloads its Zone) used to receive it.
// The admin only ever writes it; has_router_password says whether one is set.
// Request bodies still decode router_password normally.
func (z Zone) MarshalJSON() ([]byte, error) {
	if z.brief {
		return json.Marshal(zoneBriefJSON{ID: z.ID, Name: z.Name, Location: z.Location, Status: z.Status,
			LastStatus: z.LastStatus, LastSeenAt: z.LastSeenAt, OrganizationID: z.OrganizationID, AreaID: z.AreaID})
	}
	type zoneAlias Zone
	out := struct {
		zoneAlias
		RouterPassword    *string `json:"router_password"`
		HasRouterPassword bool    `json:"has_router_password"`
	}{zoneAlias: zoneAlias(z), HasRouterPassword: z.RouterPassword != nil && *z.RouterPassword != ""}
	return json.Marshal(out)
}

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

// zoneBriefJSON is all a zone nested in a list row (customer, payment,
// voucher, ticket, user...) carries: which WiFi it is and whether it is up.
// Router address, port, login user, public IP, tunnel and line settings are
// for the zone pages only — list rows went to every staff role.
type zoneBriefJSON struct {
	ID             uint       `json:"id"`
	Name           string     `json:"name"`
	Location       string     `json:"location"`
	Status         string     `json:"status"`
	LastStatus     string     `json:"last_status"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	OrganizationID uint       `json:"organization_id"`
	AreaID         *uint      `json:"area_id"`
}

var zoneBriefColumns = []string{"id", "name", "location", "status", "last_status", "last_seen_at", "organization_id", "area_id", "deleted_at"}

// ZoneBrief is a Preload condition: Preload("Zone", models.ZoneBrief) loads
// only the brief columns, and such a zone marshals as zoneBriefJSON.
func ZoneBrief(db *gorm.DB) *gorm.DB { return db.Select(zoneBriefColumns) }

// AfterFind marks zones loaded through ZoneBrief so MarshalJSON keeps them
// brief (never a full zone with blank router fields).
func (z *Zone) AfterFind(tx *gorm.DB) error {
	sel := tx.Statement.Selects
	if len(sel) == len(zoneBriefColumns) && sel[0] == "id" && sel[1] == "name" && sel[len(sel)-1] == "deleted_at" {
		z.brief = true
	}
	return nil
}
