package models

import (
	"time"

	"gorm.io/gorm"
)

// PlatformUser is a Zyra Net staff account for the Super Admin (SA)
// platform layer. It is intentionally a separate table/model from User
// (ISP admin staff) so a platform credential can never be reachable
// through the per-ISP admin login flow, and vice versa.
type PlatformUser struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Name      string         `gorm:"size:255;not null" json:"name"`
	Email     string         `gorm:"size:255;uniqueIndex;not null" json:"email"`
	Password  string         `gorm:"size:255;not null" json:"-"`
	Status    string         `gorm:"size:20;default:active" json:"status"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`
}

func (PlatformUser) TableName() string { return "platform_users" }
