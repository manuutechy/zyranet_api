package config

import (
	"fmt"
	"log"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB is the global GORM database instance.
var DB *gorm.DB

// ConnectDatabase initialises the MySQL connection pool using GORM.
func ConnectDatabase() {
	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		Config.DBUser,
		Config.DBPass,
		Config.DBHost,
		Config.DBPort,
		Config.DBName,
	)

	logLevel := logger.Silent
	if Config.AppEnv != "production" {
		logLevel = logger.Info
	}

	var err error
	DB, err = gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	})

	if err != nil {
		log.Fatalf("[database] Failed to connect: %v", err)
	}

	sqlDB, err := DB.DB()
	if err != nil {
		log.Fatalf("[database] Failed to get underlying sql.DB: %v", err)
	}

	// Connection pool settings — kept modest to fit Railway's free-tier MySQL
	// connection caps; this app's traffic doesn't need a large pool.
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetConnMaxLifetime(time.Hour)

	log.Println("[database] MySQL connection established")
}
