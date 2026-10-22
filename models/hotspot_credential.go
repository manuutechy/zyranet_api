package models

import "time"

// HotspotCredential is one device's own login on the ISP's routers: a random
// username/password bound to the device's MAC address, valid until the plan
// the customer paid for runs out. It replaces the shared `pkg-<id>` / `free`
// router users, which let anyone who knew the pattern browse for free.
//
// One row per (customer, MAC) inside an organization. Rows are never deleted:
// status moves active -> expired (plan over) or revoked (moved to another
// device / device limit), and a renewal from the same device re-activates the
// same row, so the device keeps its username and device tokens.
type HotspotCredential struct {
	ID             uint   `gorm:"primaryKey" json:"id"`
	OrganizationID uint   `gorm:"not null;index:idx_cred_org_status,priority:1;uniqueIndex:idx_cred_owner_mac,priority:1" json:"organization_id"`
	ZoneID         uint   `gorm:"not null;index" json:"zone_id"`
	CustomerID     uint   `gorm:"not null;index;uniqueIndex:idx_cred_owner_mac,priority:2" json:"customer_id"`
	PackageID      uint   `gorm:"not null" json:"package_id"`
	MacAddress     string `gorm:"size:17;not null;index;uniqueIndex:idx_cred_owner_mac,priority:3" json:"mac_address"` // AA:BB:CC:DD:EE:FF
	Username       string `gorm:"size:32;not null;uniqueIndex" json:"username"`
	// Password is what the portal submits to the router. It has to be stored
	// readable (the router needs it in clear), so it is never put in JSON.
	Password string `gorm:"size:64;not null" json:"-"`
	Kind     string `gorm:"size:10;not null;default:paid" json:"kind"` // paid | voucher | trial
	Status   string `gorm:"size:10;not null;default:active;index:idx_cred_org_status,priority:2" json:"status"`
	// ActivatedAt is when the router user was (re)created; the router's
	// limit-uptime is counted from here.
	ActivatedAt time.Time  `json:"activated_at"`
	ExpiresAt   time.Time  `gorm:"index" json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `gorm:"index" json:"updated_at"`
}

func (HotspotCredential) TableName() string { return "hotspot_credentials" }

// HotspotAccessToken is proof that a browser is the device that paid.
// Only a SHA-256 of the token is stored.
//
//	device — long-lived, kept by the portal in localStorage; lets the device
//	         fetch its router login again (and move it if its MAC changed).
//	ticket — one-time, 5 minutes; handed to the session that just paid /
//	         proved itself, exchanged once for the router login.
type HotspotAccessToken struct {
	ID           uint       `gorm:"primaryKey" json:"id"`
	CredentialID uint       `gorm:"not null;index" json:"credential_id"`
	Kind         string     `gorm:"size:10;not null" json:"kind"` // device | ticket
	TokenHash    string     `gorm:"size:64;not null;uniqueIndex" json:"-"`
	ExpiresAt    time.Time  `json:"expires_at"`
	UsedAt       *time.Time `json:"used_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

func (HotspotAccessToken) TableName() string { return "hotspot_access_tokens" }

// ZoneOutage records a stretch where a zone's router stopped checking in, and
// the time given back to customers for it.
type ZoneOutage struct {
	ID                   uint      `gorm:"primaryKey" json:"id"`
	OrganizationID       uint      `gorm:"not null;index" json:"organization_id"`
	ZoneID               uint      `gorm:"not null;index" json:"zone_id"`
	StartedAt            time.Time `json:"started_at"`
	EndedAt              time.Time `json:"ended_at"`
	DurationSeconds      int64     `json:"duration_seconds"`
	CompensatedSeconds   int64     `json:"compensated_seconds"` // after the cap
	CustomersCompensated int       `json:"customers_compensated"`
	CreatedAt            time.Time `json:"created_at"`
}

func (ZoneOutage) TableName() string { return "zone_outages" }

// FreeTrialClaim is one free-trial grant, so a device or phone gets one trial
// per cooldown period.
type FreeTrialClaim struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	OrganizationID uint      `gorm:"not null;index" json:"organization_id"`
	ZoneID         uint      `gorm:"not null" json:"zone_id"`
	PackageID      uint      `gorm:"not null" json:"package_id"`
	CustomerID     uint      `gorm:"not null" json:"customer_id"`
	MacAddress     string    `gorm:"size:17;index" json:"mac_address"`
	Phone          string    `gorm:"size:20;index" json:"phone"`
	ClaimedAt      time.Time `gorm:"index" json:"claimed_at"`
}

func (FreeTrialClaim) TableName() string { return "free_trial_claims" }
