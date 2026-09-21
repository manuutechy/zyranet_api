package models

import "time"

// Payout is one transfer of an ISP's share out of Zyra Net's shared shortcode.
// It claims a set of platform-collected Payments (Payment.PayoutID) so each
// shilling is paid out exactly once; the destination is snapshotted so a later
// settings change can't redirect a payout already being processed.
//
// Status flow:
//
//	pending    → claimed and priced, waiting for staff to send it
//	processing → B2B request accepted by Safaricom, awaiting the result callback
//	completed  → paid (Reference holds the M-Pesa receipt, or the manual reference)
//	failed     → Safaricom reported failure, or staff declared it failed; the
//	             payments are released back to the ledger
//	cancelled  → cancelled while pending; payments released
type Payout struct {
	ID             uint   `gorm:"primaryKey" json:"id"`
	OrganizationID uint   `gorm:"not null;index" json:"organization_id"`
	Status         string `gorm:"size:20;not null;default:pending;index" json:"status"`
	Method         string `gorm:"size:10" json:"method"` // "" | b2b | manual

	GrossAmount       float64 `gorm:"type:decimal(12,2)" json:"gross_amount"`
	CommissionPercent float64 `gorm:"type:decimal(5,2)" json:"commission_percent"`
	CommissionAmount  float64 `gorm:"type:decimal(12,2)" json:"commission_amount"`
	NetAmount         float64 `gorm:"type:decimal(12,2)" json:"net_amount"`
	// PayoutAmount is the whole-shilling amount actually sent (Daraja B2B takes
	// integers): the net amount rounded down. The sub-shilling remainder stays
	// with the platform.
	PayoutAmount int64 `json:"payout_amount"`
	PaymentCount int   `json:"payment_count"`

	DestType    string `gorm:"size:20" json:"dest_type"` // till | paybill
	DestTill    string `gorm:"size:20" json:"dest_till"`
	DestPaybill string `gorm:"size:20" json:"dest_paybill"`
	DestAccount string `gorm:"size:50" json:"dest_account"`

	Reference                string `gorm:"size:100;index" json:"reference"`
	OriginatorConversationID string `gorm:"size:100;index" json:"originator_conversation_id"`
	ConversationID           string `gorm:"size:100" json:"conversation_id"`
	ResultDesc               string `gorm:"size:255" json:"result_desc"`
	FailureReason            string `gorm:"size:500" json:"failure_reason"`

	CreatedByPlatformUserID uint       `json:"created_by_platform_user_id"`
	SentByPlatformUserID    *uint      `json:"sent_by_platform_user_id"`
	CompletedAt             *time.Time `json:"completed_at"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`

	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
}

func (Payout) TableName() string { return "payouts" }
