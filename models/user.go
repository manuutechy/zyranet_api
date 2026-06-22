package models

import (
	"time"

	"gorm.io/gorm"
)

// User represents an admin staff member.
type User struct {
	ID              uint           `gorm:"primaryKey" json:"id"`
	Name            string         `gorm:"size:255;not null" json:"name"`
	Email           string         `gorm:"size:255;uniqueIndex;not null" json:"email"`
	EmailVerifiedAt *time.Time     `json:"email_verified_at,omitempty"`
	Password        string         `gorm:"size:255;not null" json:"-"`
	Phone           *string        `gorm:"size:20" json:"phone"`
	Role            string         `gorm:"size:50;default:field_agent" json:"role"`
	ZoneID          *uint          `json:"zone_id"`
	Status          string         `gorm:"size:20;default:active" json:"status"`
	RememberToken   *string        `gorm:"size:100" json:"-"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	DeletedAt       gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Zone *Zone `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
}

func (User) TableName() string { return "users" }
