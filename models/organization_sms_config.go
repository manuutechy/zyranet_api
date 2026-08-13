package models

import "time"

// OrganizationSmsConfig lets an ISP tenant either keep using Zyra Net's
// shared Hostpinnacle SMS gateway ("platform" mode — the default, and what
// an org has if no row exists at all) or register its own Hostpinnacle
// credentials ("own" mode) so its outbound SMS (OTPs, vouchers, payment
// receipts) is sent/billed through its own account.
//
// Only ever read server-side when resolving credentials for an outgoing
// SMS send (see services/sms.go resolveSmsCreds) — the platform's own
// shared credentials are never included in any API response, so an ISP on
// "platform" mode has no way to see or exfiltrate them via this table.
// Mirrors OrganizationMpesaConfig's platform/own pattern exactly.
//
// Provider is currently always "hostpinnacle": that's the only SMS gateway
// this codebase actually knows how to call (see services/sms.go Send).
// SMS_PROVIDER/sms_provider settings have long gestured at an
// "africastalking" option, but no Africa's Talking client, credential
// keys, or sending logic exist anywhere in the codebase — it was dead
// scaffolding. The field is kept (rather than dropped) so a second
// provider can be added later without another migration, but
// OrganizationSmsUpdate rejects any value other than "hostpinnacle" today
// so an admin can never pick a mode that silently does nothing.
type OrganizationSmsConfig struct {
	ID                   uint      `gorm:"primaryKey" json:"id"`
	OrganizationID       uint      `gorm:"not null;uniqueIndex" json:"organization_id"`
	Mode                 string    `gorm:"size:20;default:platform" json:"mode"` // platform | own
	Provider             string    `gorm:"size:20;default:hostpinnacle" json:"provider"`
	HostpinnacleBaseURL  string    `gorm:"size:255" json:"hostpinnacle_base_url"`
	HostpinnacleAPIKey   string    `gorm:"size:255" json:"-"`
	HostpinnacleUsername string    `gorm:"size:255" json:"hostpinnacle_username"`
	HostpinnacleSenderID string    `gorm:"size:50" json:"hostpinnacle_sender_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func (OrganizationSmsConfig) TableName() string { return "organization_sms_configs" }
