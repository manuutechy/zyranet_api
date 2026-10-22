package models

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Agent is a reseller (estate reseller, shop agent) who sells an ISP's
// vouchers for cash. The ISP issues vouchers to the agent, the agent sells
// them, and hands the cash over later; AgentLedgerEntry records every shilling
// of that in between.
//
// Money is kept in integer cents (int64) everywhere in the agent ledger, so
// sums and commissions are exact — no float rounding drift.
type Agent struct {
	ID             uint `gorm:"primaryKey" json:"id"`
	OrganizationID uint `gorm:"not null;index" json:"organization_id"`
	// UserID links the agent to a field_agent staff login, so the agent can
	// sign in and see their own statement. NULL for agents without a login.
	UserID *uint  `gorm:"uniqueIndex" json:"user_id"`
	Name   string `gorm:"size:255;not null" json:"name"`
	Phone  string `gorm:"size:20;not null" json:"phone"`
	// CommissionType is "percent" (CommissionValue in basis points, 1000 =
	// 10%) or "fixed" (CommissionValue in cents per voucher). The rate in
	// force is copied onto each issued voucher, so changing it never rewrites
	// what was already issued.
	CommissionType  string `gorm:"size:10;not null;default:percent" json:"commission_type"`
	CommissionValue int64  `gorm:"not null;default:0" json:"commission_value"`
	// CreditLimitCents is how much the agent may owe at once for vouchers
	// taken on credit. 0 = no credit: vouchers must come from float or be
	// paid for upfront.
	CreditLimitCents int64     `gorm:"not null;default:0" json:"credit_limit_cents"`
	Status           string    `gorm:"size:20;not null;default:active" json:"status"` // active | inactive
	CreatedBy        *uint     `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	Zones []Zone `gorm:"many2many:agent_zones;" json:"zones,omitempty"`
	User  *User  `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (Agent) TableName() string { return "agents" }

// AgentVoucherBatch is one issue of vouchers to an agent.
type AgentVoucherBatch struct {
	ID             uint `gorm:"primaryKey" json:"id"`
	OrganizationID uint `gorm:"not null;index" json:"organization_id"`
	AgentID        uint `gorm:"not null;index" json:"agent_id"`
	ZoneID         uint `gorm:"not null" json:"zone_id"`
	PackageID      uint `gorm:"not null" json:"package_id"`
	Quantity       int  `gorm:"not null" json:"quantity"`
	// Per-voucher face value and commission at the time of issue.
	FaceValueCents  int64 `gorm:"not null" json:"face_value_cents"`
	CommissionCents int64 `gorm:"not null" json:"commission_cents"`
	// Funding is how the agent pays: "credit" (owes it), "float" (drawn from
	// money already handed over) or "upfront" (paid now, recorded as a
	// handover in the same transaction).
	Funding   string    `gorm:"size:10;not null" json:"funding"`
	CreatedBy uint      `gorm:"not null" json:"created_by"`
	CreatedAt time.Time `json:"created_at"`

	Package *Package `gorm:"foreignKey:PackageID" json:"package,omitempty"`
	Zone    *Zone    `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
}

func (AgentVoucherBatch) TableName() string { return "agent_voucher_batches" }

// AgentVoucher is a voucher held by an agent: issued -> sold (redeemed by a
// customer) or issued -> returned (voided and reversed on the ledger).
type AgentVoucher struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	OrganizationID  uint       `gorm:"not null;index" json:"organization_id"`
	AgentID         uint       `gorm:"not null;index:idx_agent_vouchers_agent_status" json:"agent_id"`
	BatchID         uint       `gorm:"not null;index" json:"batch_id"`
	VoucherID       uint       `gorm:"not null;uniqueIndex" json:"voucher_id"`
	FaceValueCents  int64      `gorm:"not null" json:"face_value_cents"`
	CommissionCents int64      `gorm:"not null" json:"commission_cents"`
	Status          string     `gorm:"size:10;not null;default:issued;index:idx_agent_vouchers_agent_status" json:"status"` // issued | sold | returned
	SoldAt          *time.Time `gorm:"index" json:"sold_at"`
	ReturnedAt      *time.Time `json:"returned_at"`
	CreatedAt       time.Time  `json:"created_at"`

	Voucher *Voucher `gorm:"foreignKey:VoucherID" json:"voucher,omitempty"`
}

func (AgentVoucher) TableName() string { return "agent_vouchers" }

// Agent ledger entry types. AmountCents is signed from the ISP's side: a
// positive amount increases what the agent owes, a negative one reduces it.
// A negative balance is prepaid float the agent holds with the ISP.
const (
	AgentEntryIssue              = "issue"               // + face value of vouchers issued
	AgentEntryCommission         = "commission"          // - commission on vouchers issued
	AgentEntryReturn             = "return"              // - face value of unsold vouchers returned
	AgentEntryCommissionReversal = "commission_reversal" // + commission on returned vouchers
	AgentEntryHandover           = "handover"            // - cash / M-Pesa received from the agent
	AgentEntryAdjustment         = "adjustment"          // ± owner correction, with a note
	AgentEntryReversal           = "reversal"            // the exact negative of an earlier entry
)

// ErrAppendOnly is returned by any attempt to change or delete a ledger
// entry. Corrections are new, reversing entries.
var ErrAppendOnly = errors.New("agent ledger entries are append-only")

// AgentLedgerEntry is one line of an agent's money ledger. Entries are never
// updated or deleted (enforced by the hooks below); every balance is the sum
// of entries.
type AgentLedgerEntry struct {
	ID             uint   `gorm:"primaryKey" json:"id"`
	OrganizationID uint   `gorm:"not null;index" json:"organization_id"`
	AgentID        uint   `gorm:"not null;index:idx_agent_ledger_agent_created" json:"agent_id"`
	Type           string `gorm:"size:24;not null" json:"type"`
	AmountCents    int64  `gorm:"not null" json:"amount_cents"`
	BatchID        *uint  `gorm:"index" json:"batch_id"`
	VoucherCount   int    `gorm:"not null;default:0" json:"voucher_count"`
	// Method and Reference describe a handover: "cash" or "mpesa" and the
	// M-Pesa confirmation code.
	Method    string `gorm:"size:10" json:"method"`
	Reference string `gorm:"size:64" json:"reference"`
	Note      string `gorm:"size:500" json:"note"`
	// ReversesID points at the entry this one reverses. Unique, so an entry
	// can be reversed at most once even under concurrent requests.
	ReversesID *uint     `gorm:"uniqueIndex" json:"reverses_id"`
	CreatedBy  uint      `gorm:"not null" json:"created_by"`
	CreatedAt  time.Time `gorm:"index:idx_agent_ledger_agent_created" json:"created_at"`

	Creator *User `gorm:"foreignKey:CreatedBy" json:"creator,omitempty"`
}

func (AgentLedgerEntry) TableName() string { return "agent_ledger_entries" }

// BeforeUpdate keeps the ledger append-only.
func (AgentLedgerEntry) BeforeUpdate(*gorm.DB) error { return ErrAppendOnly }

// BeforeDelete keeps the ledger append-only.
func (AgentLedgerEntry) BeforeDelete(*gorm.DB) error { return ErrAppendOnly }

// AgentIdempotencyKey remembers the result of each money-moving agent request
// (issue, return, handover, adjustment, reversal) under the client's
// Idempotency-Key, so a double-click or retried request returns the first
// result instead of recording it twice. Unique per organization.
type AgentIdempotencyKey struct {
	ID             uint   `gorm:"primaryKey" json:"id"`
	OrganizationID uint   `gorm:"not null;uniqueIndex:idx_agent_idem_org_key" json:"organization_id"`
	Key            string `gorm:"column:idem_key;size:64;not null;uniqueIndex:idx_agent_idem_org_key" json:"key"`
	AgentID        uint   `gorm:"not null" json:"agent_id"`
	Operation      string `gorm:"size:24;not null" json:"operation"`
	// RequestHash fingerprints the request body: the same key sent with a
	// different request is refused rather than silently replayed.
	RequestHash string `gorm:"size:64;not null" json:"request_hash"`
	// ResultID is the batch (issue), the handover/adjustment/reversal entry,
	// or the return entry the request produced.
	ResultID  uint      `gorm:"not null" json:"result_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (AgentIdempotencyKey) TableName() string { return "agent_idempotency_keys" }
