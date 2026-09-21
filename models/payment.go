package models

import "time"

// Payment records an M-Pesa or manual payment transaction.
type Payment struct {
	ID                 uint    `gorm:"primaryKey" json:"id"`
	CustomerID         *uint   `gorm:"index" json:"customer_id"`
	VoucherID          *uint   `json:"voucher_id"`
	ZoneID             uint    `gorm:"not null;index" json:"zone_id"`
	PackageID          *uint   `json:"package_id"`
	Phone              string  `gorm:"size:20;not null" json:"phone"`
	Amount             float64 `gorm:"type:decimal(10,2);not null" json:"amount"`
	Currency           string  `gorm:"size:5;default:KES" json:"currency"`
	Method             string  `gorm:"size:20;not null" json:"method"` // mpesa | manual
	MpesaTransactionID *string `gorm:"size:255;column:mpesa_transaction_id;index" json:"mpesa_transaction_id"`
	MpesaReceiptNumber *string `gorm:"size:255;column:mpesa_receipt_number;index" json:"mpesa_receipt_number"`
	Status             string  `gorm:"size:20;default:pending" json:"status"` // pending|completed|failed
	StatusReason       *string `gorm:"size:500;column:status_reason" json:"status_reason"`
	MacAddress         string  `gorm:"size:20;column:mac_address" json:"mac_address"`
	IpAddress          string  `gorm:"size:45;column:ip_address" json:"ip_address"`
	// CollectedVia records where the money landed, decided when the payment
	// completes: "platform" (Zyra Net's shared shortcode — the ISP is owed it),
	// "own" (the ISP's own Daraja app — already theirs) or "" (manual, mock or
	// sandbox — not real settled money, never part of a payout).
	CollectedVia string `gorm:"size:10;index" json:"collected_via"`
	// PayoutID is the payout that has claimed this payment. NULL = not yet paid
	// out. A payment can be claimed by at most one payout at a time.
	PayoutID *uint `gorm:"index" json:"payout_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Customer *Customer `gorm:"foreignKey:CustomerID" json:"customer,omitempty"`
	Voucher  *Voucher  `gorm:"foreignKey:VoucherID" json:"voucher,omitempty"`
	Zone     *Zone     `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
	Package  *Package  `gorm:"foreignKey:PackageID" json:"package,omitempty"`
}

func (Payment) TableName() string { return "payments" }
