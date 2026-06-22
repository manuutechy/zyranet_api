package models

import "time"

// ZoneAlert records router connectivity events.
type ZoneAlert struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	ZoneID     uint       `gorm:"not null" json:"zone_id"`
	Type       string     `gorm:"size:20;not null" json:"type"` // offline | online | error
	Message    string     `gorm:"type:text;not null" json:"message"`
	ResolvedAt *time.Time `json:"resolved_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	Zone *Zone `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
}

func (ZoneAlert) TableName() string { return "zone_alerts" }
