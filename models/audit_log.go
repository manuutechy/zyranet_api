package models

import "time"

// AuditLog records admin actions for traceability.
type AuditLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    *uint     `json:"user_id"`
	Action    string    `gorm:"size:255;not null" json:"action"`
	Model     string    `gorm:"size:100;not null" json:"model"`
	ModelID   uint      `gorm:"not null" json:"model_id"`
	OldValues *string   `gorm:"type:text" json:"old_values"`
	NewValues *string   `gorm:"type:text" json:"new_values"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	User *User `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (AuditLog) TableName() string { return "audit_logs" }
