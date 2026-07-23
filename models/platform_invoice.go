package models

import "time"

// PlatformInvoice is what Zyra Net bills an ISP tenant for platform usage
// over a period — distinct from Payment, which is the ISP's own customers
// paying the ISP.
type PlatformInvoice struct {
	ID                  uint       `gorm:"primaryKey" json:"id"`
	OrganizationID      uint       `gorm:"not null;index" json:"organization_id"`
	PeriodStart         time.Time  `json:"period_start"`
	PeriodEnd           time.Time  `json:"period_end"`
	ActiveCustomerCount int64      `json:"active_customer_count"`
	RatePerCustomer     float64    `json:"rate_per_customer"`
	Total               float64    `json:"total"`
	Status              string     `gorm:"size:20;default:draft" json:"status"` // draft | issued | paid | overdue | void
	IssuedAt            *time.Time `json:"issued_at"`
	DueAt               *time.Time `json:"due_at"`
	PaidAt              *time.Time `json:"paid_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`

	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
}

func (PlatformInvoice) TableName() string { return "platform_invoices" }
