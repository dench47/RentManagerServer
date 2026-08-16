package database

import (
	"log"
	"os"
	"rentmanager-server/internal/config"
	"rentmanager-server/internal/model"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func InitPostgres(cfg config.DBConfig) {
	var logLevel = logger.Silent
	if os.Getenv("DB_LOG_QUERIES") == "true" {
		logLevel = logger.Info
	}

	var err error
	DB, err = gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
	})
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}

	// AutoMigrate all models
	err = DB.AutoMigrate(
		&model.User{},
		&model.Property{},
		&model.Photo{},
		&model.Tenant{},
		&model.Meter{},
		&model.Payment{},
		&model.PaymentSchedule{},
		&model.Booking{},
		&model.Chat{},
		&model.Message{},
		&model.RefreshToken{},
	)
	if err != nil {
		log.Fatalf("Failed to automigrate: %v", err)
	}

	log.Println("PostgreSQL connected and migrated")
}
