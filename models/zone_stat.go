package models

import "time"

// ZoneStat stores periodic router health snapshots.
type ZoneStat struct {
	ID               uint      `gorm:"primaryKey" json:"id"`
	ZoneID           uint      `gorm:"not null" json:"zone_id"`
	CPULoad          int       `gorm:"column:cpu_load" json:"cpu_load"`
	MemoryUsedMB     int       `gorm:"column:memory_used_mb" json:"memory_used_mb"`
	MemoryTotalMB    int       `gorm:"column:memory_total_mb" json:"memory_total_mb"`
	ConnectedClients int       `gorm:"column:connected_clients" json:"connected_clients"`
	RecordedAt       time.Time `gorm:"column:recorded_at" json:"recorded_at"`

	Zone *Zone `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
}

func (ZoneStat) TableName() string { return "zone_stats" }
