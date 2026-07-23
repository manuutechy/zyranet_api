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
	BillingRatePerCustomer float64        `gorm:"default:0" json:"billing_rate_per_customer"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
	DeletedAt              gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

func (Organization) TableName() string { return "organizations" }
