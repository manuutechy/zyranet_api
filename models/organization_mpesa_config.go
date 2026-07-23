package models

import "time"

// OrganizationMpesaConfig lets an ISP tenant either keep using Zyra Net's
// shared Daraja app ("platform" mode — the default, and what an org has if
// no row exists at all) or register its own Daraja credentials ("own"
// mode) so its customers' M-Pesa payments settle directly to the ISP.
//
// Only ever read server-side when resolving credentials for an outgoing
// Daraja request (see services/mpesa.go resolveMpesaCreds) — the platform's
// own shared credentials are never included in any API response, so an ISP
// on "platform" mode has no way to see or exfiltrate them via this table.
type OrganizationMpesaConfig struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	OrganizationID uint      `gorm:"not null;uniqueIndex" json:"organization_id"`
	Mode           string    `gorm:"size:20;default:platform" json:"mode"` // platform | own
	ConsumerKey    string    `gorm:"size:255" json:"consumer_key"`
	ConsumerSecret string    `gorm:"size:255" json:"-"`
	Shortcode      string    `gorm:"size:20" json:"shortcode"`
	Passkey        string    `gorm:"size:255" json:"-"`
	CallbackURL    string    `gorm:"size:255" json:"callback_url"`
	Env            string    `gorm:"size:20;default:sandbox" json:"env"`
	BillingType    string    `gorm:"size:20;default:paybill" json:"billing_type"` // paybill | till | bank
	TillNumber     string    `gorm:"size:20" json:"till_number"`
	PaybillNumber  string    `gorm:"size:20" json:"paybill_number"`
	PaybillAccount string    `gorm:"size:50" json:"paybill_account"`
	BankName       string    `gorm:"size:100" json:"bank_name"`
	BankAccount    string    `gorm:"size:50" json:"bank_account"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (OrganizationMpesaConfig) TableName() string { return "organization_mpesa_configs" }
