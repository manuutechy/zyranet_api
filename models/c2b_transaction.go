package models

import "time"

// C2BTransaction is the idempotency ledger for paybill (C2B) confirmations.
// Every confirmation Safaricom delivers gets exactly one row, keyed by its
// TransID (unique), before anything is credited — so a re-delivered or forged
// duplicate can never credit twice, even when two deliveries race.
//
// Status flow:
//
//	received  → row claimed, being processed
//	verifying → the confirmation carried no valid callback token, so a Daraja
//	            Transaction Status query was sent; the result callback decides
//	credited  → money applied (to a customer, or queued as an unmatched payment
//	            that Safaricom has vouched for)
//	parked    → could not be verified (or Safaricom's answer didn't match); held
//	            in unmatched_c2b_payments for a person to check, nothing credited
type C2BTransaction struct {
	ID                uint    `gorm:"primaryKey" json:"id"`
	TransID           string  `gorm:"size:64;uniqueIndex;not null" json:"trans_id"`
	TransTime         string  `gorm:"size:20" json:"trans_time"`
	Amount            float64 `gorm:"type:decimal(10,2)" json:"amount"`
	BusinessShortCode string  `gorm:"size:20" json:"business_short_code"`
	BillRefNumber     string  `gorm:"size:100" json:"bill_ref_number"`
	MSISDN            string  `gorm:"size:100" json:"msisdn"`
	FirstName         string  `gorm:"size:100" json:"first_name"`
	MiddleName        string  `gorm:"size:100" json:"middle_name"`
	LastName          string  `gorm:"size:100" json:"last_name"`
	Status            string  `gorm:"size:20;not null;default:received;index" json:"status"`
	// Verification is how the confirmation was proven genuine: "token" (the
	// URL carried MPESA_CALLBACK_SECRET), "status_query" (Safaricom confirmed it
	// via Transaction Status) or "" (not proven).
	Verification         string    `gorm:"size:20" json:"verification"`
	VerifyConversationID string    `gorm:"size:100;index" json:"verify_conversation_id"`
	VerifyNote           string    `gorm:"size:500" json:"verify_note"`
	PaymentID            *uint     `json:"payment_id"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func (C2BTransaction) TableName() string { return "c2b_transactions" }

// PendingRouterAction is a router change owed to a paying customer (whitelist
// their device, load their voucher, re-enable their PPPoE account). It is
// persisted before the first attempt and retried with backoff by a background
// job, so an unreachable router or an API restart can't silently lose it.
type PendingRouterAction struct {
	ID         uint   `gorm:"primaryKey" json:"id"`
	ZoneID     uint   `gorm:"not null;index" json:"zone_id"`
	PaymentID  *uint  `gorm:"index" json:"payment_id"`
	Action     string `gorm:"size:30;not null" json:"action"` // whitelist_mac | push_voucher | reactivate_customer
	CustomerID *uint  `json:"customer_id"`
	VoucherID  *uint  `json:"voucher_id"`
	PackageID  *uint  `json:"package_id"`
	MacAddress string `gorm:"size:45" json:"mac_address"`
	// Status: pending (waiting for NextAttemptAt) | running | done | failed (gave up).
	Status        string    `gorm:"size:20;not null;default:pending;index" json:"status"`
	Attempts      int       `gorm:"not null;default:0" json:"attempts"`
	NextAttemptAt time.Time `gorm:"index" json:"next_attempt_at"`
	LastError     string    `gorm:"size:500" json:"last_error"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (PendingRouterAction) TableName() string { return "pending_router_actions" }
