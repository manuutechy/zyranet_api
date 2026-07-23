package models

import "time"

// Setting is a key-value store for dynamic system configuration. It
// remains deployment-global for now — Phase 4 of the multi-tenant plan
// introduces a dedicated OrganizationMpesaConfig table for the one thing
// that actually needs per-tenant values today; per-org branding/settings
// is deferred until that's needed, to avoid threading OrganizationID
// through the many GetSetting() call sites across the codebase.
type Setting struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Key       string    `gorm:"size:255;uniqueIndex;not null" json:"key"`
	Value     *string   `gorm:"type:text" json:"value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Setting) TableName() string { return "settings" }
