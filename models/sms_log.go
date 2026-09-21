package models

import "time"

// SmsLog records all SMS messages sent through the system.
type SmsLog struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// OrganizationID is the ISP whose message this was (0/NULL for platform
	// messages and rows from before it was recorded). It lets a client's
	// activity timeline show only that ISP's own messages to a shared phone.
	OrganizationID   *uint     `gorm:"index" json:"organization_id"`
	Phone            string    `gorm:"size:20;not null" json:"phone"`
	Message          string    `gorm:"type:text;not null" json:"message"`
	Status           string    `gorm:"size:20;not null" json:"status"` // sent | failed
	ProviderResponse *string   `gorm:"type:text" json:"provider_response"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (SmsLog) TableName() string { return "sms_logs" }
