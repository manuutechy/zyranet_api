package models

import "time"

// CreditLog records credit/debit adjustments to customer balances.
type CreditLog struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	CustomerID uint      `gorm:"not null" json:"customer_id"`
	Amount     float64   `gorm:"type:decimal(10,2);not null" json:"amount"`
	Type       string    `gorm:"size:10;not null" json:"type"` // credit | debit
	Note       *string   `gorm:"size:500" json:"note"`
	AddedBy    *uint     `json:"added_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	Customer *Customer `gorm:"foreignKey:CustomerID" json:"customer,omitempty"`
	Admin    *User     `gorm:"foreignKey:AddedBy" json:"admin,omitempty"`
}

func (CreditLog) TableName() string { return "credit_logs" }
