package models

import "time"

// OtpCode stores persistent verification codes across server restarts.
type OtpCode struct {
	Phone     string    `gorm:"primaryKey;size:20" json:"phone"`
	OTP       string    `gorm:"size:10;not null" json:"otp"`
	ExpiresAt time.Time `gorm:"not null" json:"expires_at"`
	Attempts  int       `gorm:"default:0" json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OtpCode) TableName() string { return "otp_codes" }
