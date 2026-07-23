package models

import (
	"time"

	"gorm.io/gorm"
)

// Organization represents one ISP tenant on the platform. Every User and
// Zone belongs to exactly one Organization; all other zone-scoped resources
// (customers, packages, payments, vouchers, etc.) derive their tenant
// through Zone.OrganizationID rather than storing it themselves.
type Organization struct {
	ID           uint    `gorm:"primaryKey" json:"id"`
	Name         string  `gorm:"size:255;not null" json:"name"`
	Slug         string  `gorm:"size:100;uniqueIndex;not null" json:"slug"`
	ContactEmail string  `gorm:"size:255" json:"contact_email"`
	ContactPhone *string `gorm:"size:20" json:"contact_phone"`
	Status       string  `gorm:"size:20;default:active" json:"status"` // active | suspended | trial
	// BillingRatePerCustomer is what Zyra Net charges this ISP per billable
	// (active) customer per invoicing period. Zero means billing hasn't been
	// configured for this tenant yet — the invoice generator skips it.
	BillingRatePerCustomer float64 `gorm:"default:0" json:"billing_rate_per_customer"`

	// Settlement destination for an ISP on "platform" Daraja mode: when its
	// customers pay into Zyra Net's shared till/paybill, this is where Zyra
	// Net sends that ISP's share. Only two destination shapes exist —
	// SettlementType is "till" (SettlementTillNumber only) or "paybill"
	// (SettlementPaybillNumber + SettlementAccountNumber); a bank account is
	// entered the same way a paybill is (the bank's own paybill number plus
	// the customer's account number at that bank), so it isn't a third type.
	// Irrelevant for ISPs on "own" Daraja mode, since their customers' money
	// never passes through Zyra Net in the first place.
	SettlementType          string `gorm:"size:20;default:paybill" json:"settlement_type"` // till | paybill
	SettlementTillNumber    string `gorm:"size:20" json:"settlement_till_number"`
	SettlementPaybillNumber string `gorm:"size:20" json:"settlement_paybill_number"`
	SettlementAccountNumber string `gorm:"size:50" json:"settlement_account_number"`

	// Captive portal branding — what a connecting hotspot customer sees at
	// captive.zyranet.co.ke for this ISP's zones. CaptivePortalTheme selects
	// the page layout (see the `customer` app's theme registry); the rest
	// override the portal's default copy/colors. Blank fields fall back to
	// sensible defaults (Name, the platform default color, etc.) at read time
	// rather than being duplicated here.
	CaptivePortalTheme        string `gorm:"size:30;default:classic" json:"captive_portal_theme"` // classic | split
	CaptivePortalCompanyName  string `gorm:"size:255" json:"captive_portal_company_name"`
	CaptivePortalLogo         string `gorm:"size:255" json:"captive_portal_logo"`
	CaptivePortalPrimaryColor string `gorm:"size:20" json:"captive_portal_primary_color"`
	CaptivePortalTagline      string `gorm:"size:255" json:"captive_portal_tagline"`
	CaptivePortalSupportPhone string `gorm:"size:20" json:"captive_portal_support_phone"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

func (Organization) TableName() string { return "organizations" }
