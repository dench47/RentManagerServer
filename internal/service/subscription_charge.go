package service

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"gorm.io/gorm"
)

// SubscriptionChargeService — ежедневное списание за подписку.
//
// Модель «календарный день, день оплаты бесплатный»: списания строго
// в 0:00. Оплатил в 23:55 — остаток сегодняшнего дня бесплатно, первое
// списание в полночь. Активация (деньги + объекты) только запускает
// колесо, мгновенного списания нет.
//
// Постоплата с кредитом в один транш (как операторы связи):
//   - полночь, баланс >= 0 → списываем, даже если баланс уходит в минус
//     (не глубже −1 транша): 5 ₽ − 20 ₽ = −15 ₽, день оплачен;
//   - полночь, баланс уже отрицательный → СТОП: операция «Недостаточно
//     средств», объекты снимаются с публикации, пуш пользователю;
//   - пополнение при отрицательном балансе → долг гасится, объекты
//     возвращаются в публикацию сразу.
//
// Защита от «оплаты по необходимости»: списание привязано к объектам,
// а не к заходам в приложение; чтобы остановить списания — удали объект.
type SubscriptionChargeService struct {
	fcm *FCMService
	// Для тестов: шаг колеса из CHARGE_INTERVAL («1m»). Ночью 0:00 по
	// локальному времени сервера; тестовый интервал заменяет полуночь.
	testInterval time.Duration
}

func NewSubscriptionChargeService(fcm *FCMService) *SubscriptionChargeService {
	var test time.Duration
	if v := getEnvDuration("CHARGE_INTERVAL"); v > 0 {
		test = v
		log.Printf("charge: TEST interval %s (CHARGE_INTERVAL), полночь игнорируется", test)
	}
	return &SubscriptionChargeService{fcm: fcm, testInterval: test}
}

// Start запускает тикер: раз в минуту один индексированный запрос выбирает
// пользователей, у кого next_charge_at <= now, и делает шаг колеса.
// На 20k пользователей это ~14 строк в минуту — нагрузка копеечная.
func (s *SubscriptionChargeService) Start() {
	go func() {
		s.tick() // сразу при старте — подтянуть просроченных
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.tick()
		}
	}()
}

// nextMidnight — ближайшая полночь по локальному времени сервера
// (следующий календарный день); в тестовом режиме — +интервал.
func (s *SubscriptionChargeService) nextMidnight(from time.Time) time.Time {
	if s.testInterval > 0 {
		return from.Add(s.testInterval)
	}
	next := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	if !next.After(from) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func (s *SubscriptionChargeService) tick() {
	var users []model.User
	if err := database.DB.
		Where("next_charge_at IS NOT NULL AND next_charge_at <= ? AND deleted_at IS NULL", time.Now()).
		Find(&users).Error; err != nil {
		log.Printf("charge tick: select failed: %v", err)
		return
	}
	for i := range users {
		s.Step(&users[i])
	}
}

// Step — один шаг колеса в полночь: списание с кредитом в один транш,
// при исчерпании — блокировка функционала (объекты с публикации + пуш).
func (s *SubscriptionChargeService) Step(user *model.User) {
	objects := countObjects(user.ID)
	rate := rateFor(user)
	charge := float64(objects) * rate

	switch {
	case charge > 0 && user.Balance >= 0:
		// Постоплата: списываем, минус в один транш допустим
		if err := database.DB.Model(&model.User{}).Where("id = ?", user.ID).
			Update("balance", gorm.Expr("balance - ?", charge)).Error; err != nil {
			log.Printf("charge: balance update failed for %s: %v", user.ID, err)
			return
		}
		database.DB.Create(&model.SubscriptionOperation{
			UserID:   user.ID,
			Type:     "charge",
			Title:    "Ежедневное списание",
			Subtitle: strconv.FormatInt(objects, 10) + " объекта × " + formatRub(rate) + " ₽",
			Amount:   -charge,
			Status:   "done",
		})
		log.Printf("charge: user %s −%s ₽ (%d объектов × %s)", user.ID, formatRub(charge), objects, formatRub(rate))
		// Вернулись из блока (пополнение в минусе) — восстанавливаем публикацию
		if user.SubscriptionBlocked {
			s.unblock(user.ID)
		}

	case charge > 0 && user.Balance < 0:
		// Кредит в один транш исчерпан — функционал останавливается.
		// Операцию и пуш пишем один раз, при переходе в блок
		if !user.SubscriptionBlocked {
			database.DB.Create(&model.SubscriptionOperation{
				UserID:   user.ID,
				Type:     "charge",
				Title:    "Ежедневное списание",
				Subtitle: "Недостаточно средств",
				Amount:   -charge,
				Status:   "failed",
			})
			s.block(user.ID)
			log.Printf("charge: user %s BLOCKED (balance %.0f, charge %.0f)", user.ID, user.Balance, charge)
		}

	case charge == 0 && user.SubscriptionBlocked:
		// Объектов не осталось — блокировать/держать блок нечего
		s.unblock(user.ID)
	}

	// Колесо крутится всегда: заблокированный пользователь тоже проверяется
	// каждую полночь — пополнит, следующий тик спишет и разблокирует
	database.DB.Model(&model.User{}).Where("id = ?", user.ID).
		Update("next_charge_at", s.nextMidnight(time.Now()))
}

// ActivateIfDue запускает колесо, если пользователь ещё не в подписке,
// а деньги и объекты уже есть. Само списание — только в следующую полночь:
// остаток дня оплаты бесплатный.
func (s *SubscriptionChargeService) ActivateIfDue(userID string) {
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		return
	}
	if user.NextChargeAt != nil {
		return // колесо уже крутится
	}
	if countObjects(userID) == 0 || user.Balance <= 0 {
		return
	}
	next := s.nextMidnight(time.Now())
	database.DB.Model(&model.User{}).Where("id = ?", userID).
		Update("next_charge_at", next)
	log.Printf("charge: user %s activated, first charge at %s", userID, next.Format("02.01 15:04"))
}

// ResumeIfBlocked — мгновенное возобновление после пополнения в минусе:
// долг гасится списанием сразу, объекты возвращаются в публикацию.
// Вызывается из пополнения баланса.
func (s *SubscriptionChargeService) ResumeIfBlocked(userID string) {
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		return
	}
	if !user.SubscriptionBlocked || user.Balance < 0 {
		return // не в блоке или долга не закрыли — дождёмся полуночи
	}
	s.Step(&user)
}

// block — функционал недоступен: объекты с публикации, флаг, пуш
func (s *SubscriptionChargeService) block(userID string) {
	database.DB.Model(&model.User{}).Where("id = ?", userID).
		Update("subscription_blocked", true)
	database.DB.Model(&model.Property{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Update("is_published", false)
	if s.fcm != nil {
		go s.fcm.SendToUser(userID, map[string]string{
			"type":  "subscription_blocked",
			"title": "Недостаточно средств",
			"body":  "Баланс закончился — объекты недоступны. Пополните баланс, чтобы продолжить",
		}, "")
	}
}

// unblock — публикация возвращается, флаг снимается
func (s *SubscriptionChargeService) unblock(userID string) {
	database.DB.Model(&model.User{}).Where("id = ?", userID).
		Update("subscription_blocked", false)
	database.DB.Model(&model.Property{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Update("is_published", true)
	log.Printf("charge: user %s unblocked", userID)
}

func countObjects(userID string) int64 {
	var objects int64
	database.DB.Model(&model.Property{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Count(&objects)
	return objects
}

func rateFor(user *model.User) float64 {
	const baseRate = 10.0
	if user.AppliedPromoCode != "" {
		var pc model.PromoCode
		if err := database.DB.First(&pc, "code = ?", user.AppliedPromoCode).Error; err == nil {
			return pc.Rate
		}
	}
	return baseRate
}

// formatRub — «10», «8», «30» без десятичных хвостов
func formatRub(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// getEnvDuration — парсит «1m» / «30s» / «24h» из окружения
func getEnvDuration(key string) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}
