package models

import "time"

// SmsLog records all SMS messages sent through the system.
type SmsLog struct {
	ID               uint      `gorm:"primaryKey" json:"id"`
	Phone            string    `gorm:"size:20;not null" json:"phone"`
	Message          string    `gorm:"type:text;not null" json:"message"`
	Status           string    `gorm:"size:20;not null" json:"status"` // sent | failed
	ProviderResponse *string   `gorm:"type:text" json:"provider_response"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (SmsLog) TableName() string { return "sms_logs" }
