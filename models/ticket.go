package models

import (
	"time"

	"gorm.io/gorm"
)

// Ticket represents a customer support request.
type Ticket struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	OrganizationID uint           `gorm:"index;default:1" json:"organization_id"`
	ZoneID         *uint          `gorm:"index" json:"zone_id"`
	CustomerID     *uint          `json:"customer_id"`
	Name           string         `gorm:"size:255;not null" json:"name"`
	Phone          string         `gorm:"size:20;not null" json:"phone"`
	Subject        string         `gorm:"size:255;not null" json:"subject"`
	Message        string         `gorm:"type:text;not null" json:"message"`
	AttachmentURL  string         `gorm:"size:500" json:"attachment_url"`
	Source         string         `gorm:"size:50;default:captive_portal" json:"source"` // captive_portal|dashboard|admin
	InternalNotes  string         `gorm:"type:text;column:internal_notes" json:"internal_notes"`
	Status         string         `gorm:"size:30;default:pending" json:"status"` // pending|open|in_progress|resolved|closed
	Priority       string         `gorm:"size:20;default:medium" json:"priority"` // low|medium|high
	AssignedTo     *uint          `json:"assigned_to"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Customer     *Customer     `gorm:"foreignKey:CustomerID" json:"customer,omitempty"`
	AssignedUser *User         `gorm:"foreignKey:AssignedTo" json:"assigned_user,omitempty"`
	Zone         *Zone         `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
}

func (Ticket) TableName() string { return "tickets" }
