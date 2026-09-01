package database

import (
	"log"
	"os"
	"rentmanager-server/internal/config"
	"rentmanager-server/internal/model"
	"time"

	"github.com/google/uuid"
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
		&model.MeterReading{},
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

	// До починки upsert каждое сохранение графика создавало новую строку:
	// остаётся одна (самая свежая по updated_at) на объект.
	consolidatePaymentSchedules()

	// Бэксайд-миграция: объектам, созданным до автосоздания графиков,
	// недостающий график создаётся по типу аренды (посуточно → переменный,
	// длительно → постоянный). Существующие графики не трогаются.
	backfillPaymentSchedules()

	// Фоновая очистка протухших refresh-токенов, чтобы таблица не росла.
	go startRefreshTokenCleanup()

	// Очистка trust-записей устройств, чьи пользователи удалены
	// (сироты могли остаться от удалений до внедрения Device Trust).
	go startTrustedDevicesCleanup()
}

// consolidatePaymentSchedules убирает дубли графиков (одна запись на объект):
// остаётся самая свежая по updated_at — именно её обновлял upsert.
func consolidatePaymentSchedules() {
	var groups []struct {
		UserID     string
		PropertyID string
	}
	if err := DB.Model(&model.PaymentSchedule{}).
		Select("user_id, property_id").
		Group("user_id, property_id").
		Having("count(*) > 1").
		Scan(&groups).Error; err != nil {
		log.Printf("payment schedule consolidation failed: %v", err)
		return
	}
	removed := 0
	for _, g := range groups {
		var schedules []model.PaymentSchedule
		if err := DB.Where("user_id = ? AND property_id = ?", g.UserID, g.PropertyID).
			Order("updated_at desc").Find(&schedules).Error; err != nil || len(schedules) < 2 {
			continue
		}
		for _, s := range schedules[1:] {
			if err := DB.Unscoped().Delete(&model.PaymentSchedule{}, s.ID).Error; err == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("payment schedule consolidation: removed %d duplicates", removed)
	}
}

// backfillPaymentSchedules создаёт графики платежей старым объектам,
// у которых графика ещё нет. Тип — по типу аренды объекта:
// «посуточно» → manual (переменный), иначе → auto (постоянный).
func backfillPaymentSchedules() {
	var properties []model.Property
	if err := DB.Find(&properties).Error; err != nil {
		log.Printf("payment schedule backfill failed to list properties: %v", err)
		return
	}
	created := 0
	for _, p := range properties {
		var count int64
		if err := DB.Model(&model.PaymentSchedule{}).
			Where("property_id = ?", p.ID).Count(&count).Error; err != nil {
			log.Printf("payment schedule backfill check failed for property %s: %v", p.ID, err)
			continue
		}
		if count > 0 {
			continue
		}
		scheduleType := "auto"
		if p.RentType != nil && *p.RentType == "посуточно" {
			scheduleType = "manual"
		}
		schedule := model.PaymentSchedule{
			BaseModel:  model.BaseModel{ID: uuid.New().String()},
			PropertyID: p.ID,
			UserID:     p.UserID,
			Type:       scheduleType,
		}
		if err := DB.Create(&schedule).Error; err != nil {
			log.Printf("payment schedule backfill create failed for property %s: %v", p.ID, err)
			continue
		}
		created++
	}
	if created > 0 {
		log.Printf("payment schedule backfill: created %d schedules", created)
	}
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
