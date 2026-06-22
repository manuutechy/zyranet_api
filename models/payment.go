package models

import "time"

// Payment records an M-Pesa or manual payment transaction.
type Payment struct {
	ID                  uint      `gorm:"primaryKey" json:"id"`
	CustomerID          *uint     `json:"customer_id"`
	VoucherID           *uint     `json:"voucher_id"`
	ZoneID              uint      `gorm:"not null" json:"zone_id"`
	PackageID           *uint     `json:"package_id"`
	Phone               string    `gorm:"size:20;not null" json:"phone"`
	Amount              float64   `gorm:"type:decimal(10,2);not null" json:"amount"`
	Currency            string    `gorm:"size:5;default:KES" json:"currency"`
	Method              string    `gorm:"size:20;not null" json:"method"` // mpesa | manual
	MpesaTransactionID  *string   `gorm:"size:255;column:mpesa_transaction_id" json:"mpesa_transaction_id"`
	MpesaReceiptNumber  *string   `gorm:"size:255;column:mpesa_receipt_number" json:"mpesa_receipt_number"`
	Status              string    `gorm:"size:20;default:pending" json:"status"` // pending|completed|failed
	StatusReason        *string   `gorm:"size:500;column:status_reason" json:"status_reason"`
	MacAddress          string    `gorm:"size:20;column:mac_address" json:"mac_address"`
	IpAddress           string    `gorm:"size:45;column:ip_address" json:"ip_address"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`

	Customer *Customer `gorm:"foreignKey:CustomerID" json:"customer,omitempty"`
	Voucher  *Voucher  `gorm:"foreignKey:VoucherID" json:"voucher,omitempty"`
	Zone     *Zone     `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
	Package  *Package  `gorm:"foreignKey:PackageID" json:"package,omitempty"`
}

func (Payment) TableName() string { return "payments" }
