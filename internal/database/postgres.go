package database

import (
	"log"
	"os"
	"rentmanager-server/internal/config"
	"rentmanager-server/internal/model"
	"time"

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

	// Настройка пула соединений: переживает рестарт БД и не держит мёртвые коннекты.
	sqlDB, err := DB.DB()
	if err != nil {
		log.Fatalf("Failed to get sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	// AutoMigrate all models
	err = DB.AutoMigrate(
		&model.User{},
		&model.TrustedDevice{},
		&model.TelegramBinding{},
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

	// Фоновая очистка протухших refresh-токенов, чтобы таблица не росла.
	go startRefreshTokenCleanup()

	// Очистка trust-записей устройств, чьи пользователи удалены
	// (сироты могли остаться от удалений до внедрения Device Trust).
	go startTrustedDevicesCleanup()
}

// startTrustedDevicesCleanup периодически удаляет доверенные устройства
// несуществующих пользователей.
func startTrustedDevicesCleanup() {
	clean := func() {
		res := DB.Exec("DELETE FROM trusted_devices WHERE user_id NOT IN (SELECT id FROM users)")
		if res.Error != nil {
			log.Printf("trusted devices cleanup failed: %v", res.Error)
		} else if res.RowsAffected > 0 {
			log.Printf("trusted devices cleanup: removed %d orphaned records", res.RowsAffected)
		}
	}

	clean() // сразу при старте
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		clean()
	}
}

// startRefreshTokenCleanup периодически удаляет истёкшие refresh-токены.
func startRefreshTokenCleanup() {
	clean := func() {
		res := DB.Exec("DELETE FROM refresh_tokens WHERE expires_at <= ?", time.Now().UnixMilli())
		if res.Error != nil {
			log.Printf("refresh token cleanup failed: %v", res.Error)
		} else if res.RowsAffected > 0 {
			log.Printf("refresh token cleanup: removed %d expired tokens", res.RowsAffected)
		}
	}

	clean() // сразу при старте
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		clean()
	}
}
