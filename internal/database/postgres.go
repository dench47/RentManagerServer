package database

import (
	"log"
	"rentmanager-server/internal/config"
	"rentmanager-server/internal/model"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func InitPostgres(cfg config.DBConfig) {
	var err error
	DB, err = gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
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
		&model.Chat{},
		&model.Message{},
		&model.RefreshToken{},
	)
	if err != nil {
		log.Fatalf("Failed to automigrate: %v", err)
	}

	log.Println("PostgreSQL connected and migrated")
}
