package models

import "time"

// UnmatchedC2BPayment holds a C2B payment Safaricom confirmed on Zyra Net's
// shared paybill that couldn't be automatically matched to a customer (e.g.
// a mistyped account reference). Since multiple ISPs can share the one
// platform paybill, an unmatched payment's Organization/Zone genuinely
// can't be inferred automatically — it's queued here for a platform (SA)
// staff member to manually search and assign, rather than silently
// misattributing revenue to the wrong tenant (which is what defaulting to
// "the first zone" used to do).
type UnmatchedC2BPayment struct {
	ID                       uint       `gorm:"primaryKey" json:"id"`
	TransID                  string     `gorm:"size:255;uniqueIndex;not null" json:"trans_id"`
	Phone                    string     `gorm:"size:20" json:"phone"`
	Amount                   float64    `gorm:"type:decimal(10,2)" json:"amount"`
	BillRefNumber            string     `gorm:"size:100" json:"bill_ref_number"`
	BusinessShortCode        string     `gorm:"size:20" json:"business_short_code"`
	FirstName                string     `gorm:"size:100" json:"first_name"`
	LastName                 string     `gorm:"size:100" json:"last_name"`
	Status                   string     `gorm:"size:20;default:pending" json:"status"` // pending | resolved
	ResolvedZoneID           *uint      `json:"resolved_zone_id"`
	ResolvedCustomerID       *uint      `json:"resolved_customer_id"`
	ResolvedPaymentID        *uint      `json:"resolved_payment_id"`
	ResolvedByPlatformUserID *uint      `json:"resolved_by_platform_user_id"`
	ResolvedAt               *time.Time `json:"resolved_at"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

func (UnmatchedC2BPayment) TableName() string { return "unmatched_c2b_payments" }
