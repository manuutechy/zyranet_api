package services

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// setupTestDB points config.DB at a fresh in-memory SQLite database with all
// tables migrated, and returns it. Each call gets an isolated database.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	err = db.AutoMigrate(
		&models.User{},
		&models.Zone{},
		&models.Package{},
		&models.Customer{},
		&models.Ticket{},
		&models.Payment{},
		&models.Session{},
		&models.Setting{},
		&models.Voucher{},
		&models.AuditLog{},
		&models.CreditLog{},
		&models.SmsLog{},
		&models.ZoneAlert{},
		&models.ZoneStat{},
	)
	if err != nil {
		t.Fatalf("failed to migrate test db: %v", err)
	}

	config.DB = db
	return db
}
