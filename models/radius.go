package models

import "time"

// RadiusAccount maps a RADIUS username to the customer and zone it belongs to.
// FreeRADIUS owns the radcheck/radreply/radacct/radpostauth tables; this is
// ours, and is what lets session and sign-in logs be attributed to a customer
// and scoped to an ISP.
//
// Rows are kept after a customer expires (Active=false) so their history stays
// attributable; only the RADIUS-side credentials are removed.
type RadiusAccount struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Username   string    `gorm:"size:64;uniqueIndex;not null" json:"username"` // hotspot: MAC (AA:BB:..), pppoe: pppoe username
	CustomerID uint      `gorm:"index" json:"customer_id"`
	ZoneID     uint      `gorm:"index" json:"zone_id"`
	Kind       string    `gorm:"size:12" json:"kind"` // hotspot | pppoe
	Active     bool      `gorm:"index" json:"active"`
	AttrHash   string    `gorm:"size:64" json:"-"` // fingerprint of what was last written, to skip no-op syncs
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (RadiusAccount) TableName() string { return "radius_accounts" }
