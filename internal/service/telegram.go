package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"github.com/google/uuid"
)

// TelegramUpdate — входящий апдейт от Telegram (webhook)
type TelegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *TelegramMessage `json:"message"`
}

type TelegramMessage struct {
	MessageID int64         `json:"message_id"`
	Chat      TelegramChat  `json:"chat"`
	Text      string        `json:"text"`
	Date      int64         `json:"date"`
	From      *TelegramUser `json:"from"`
}

type TelegramChat struct {
	ID int64 `json:"id"`
}

type TelegramUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// TelegramService — работа с Telegram Bot API
type TelegramService struct {
	botToken    string
	botUsername string
	httpClient  *http.Client
}

func NewTelegramService(cfg config.TelegramConfig) *TelegramService {
	if cfg.BotToken == "" {
		log.Println("TelegramService: BOT_TOKEN not set — skipped")
		return nil
	}
	return &TelegramService{
		botToken:    cfg.BotToken,
		botUsername: cfg.BotUsername,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// GenerateCode — 8-значный код (цифры)
func (s *TelegramService) GenerateCode() string {
	const length = 8
	result := make([]byte, length)
	for i := range result {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		result[i] = byte('0' + n.Int64())
	}
	return string(result)
}

// BotToken возвращает токен бота
func (s *TelegramService) BotToken() string { return s.botToken }

// BotLink возвращает ссылку на бота с токеном
func (s *TelegramService) BotLink(token string) string {
	return fmt.Sprintf("https://t.me/%s?start=%s", s.botUsername, token)
}

// IsEnabled возвращает true, если токен бота задан
func (s *TelegramService) IsEnabled() bool { return s.botToken != "" }

// ParseUpdate парсит тело запроса (формат Telegram Update)
func ParseUpdate(body []byte) (TelegramUpdate, error) {
	var update TelegramUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		return TelegramUpdate{}, err
	}
	return update, nil
}

// ===== Webhook: обработка входящего сообщения =====

// HandleUpdate обрабатывает одно обновление от Telegram.
func (s *TelegramService) HandleUpdate(ctx context.Context, update TelegramUpdate) {
	if update.Message == nil {
		return
	}
	msg := update.Message
	chatID := msg.Chat.ID
	username := ""
	firstName := "Unknown"
	var telegramUserID int64
	if msg.From != nil {
		username = msg.From.Username
		firstName = msg.From.FirstName
		telegramUserID = msg.From.ID
	}

	log.Printf("Telegram: chat_id=%d user=%d username=%s text=%q",
		chatID, telegramUserID, username, msg.Text)

	if len(msg.Text) >= 7 && msg.Text[:7] == "/start " {
		s.handleStartToken(ctx, chatID, telegramUserID, username, firstName, msg.Text[7:])
		return
	}
	if msg.Text == "/start" {
		s.sendMessage(ctx, chatID, "Привет! Этот бот для привязки Telegram к RentManager. Нажмите «Привязать» в приложении, чтобы получить персональную ссылку.")
		return
	}
	s.sendMessage(ctx, chatID, "Этот бот отправляет коды входа в RentManager. Для привязки используйте приложение.")
}

func (s *TelegramService) handleStartToken(ctx context.Context, chatID, telegramUserID int64, username, firstName, token string) {
	key := "tg_link:" + token
	phone, err := database.RDB.Get(ctx, key).Result()
	if err != nil || phone == "" {
		s.sendMessage(ctx, chatID, "Токен недействителен или истёк. Запросите новую ссылку в приложении RentManager.")
		return
	}

	if s.bindUser(chatID, telegramUserID, username, firstName, phone) {
		s.sendMessage(ctx, chatID, "✅ Telegram привязан к номеру "+phone+"!\nТеперь вы будете получать коды входа в RentManager через этого бота.")
	} else {
		s.sendMessage(ctx, chatID, "Ошибка: пользователь с таким номером не найден. Сначала зарегистрируйтесь в приложении RentManager.")
	}
	database.RDB.Del(ctx, key)
}

func (s *TelegramService) bindUser(chatID, telegramUserID int64, username, firstName, phone string) bool {
	var user model.User
	if err := database.DB.Where("phone = ?", phone).First(&user).Error; err != nil {
		return false
	}

	binding := model.TelegramBinding{
		ID:             uuid.New().String(),
		UserID:         user.ID,
		ChatID:         chatID,
		TelegramUserID: telegramUserID,
		Username:       username,
		FirstName:      firstName,
		LinkedAt:       time.Now(),
	}

	result := database.DB.Where("user_id = ?", user.ID).Assign(binding).FirstOrCreate(&binding)
	if result.Error != nil {
		log.Printf("Telegram: bindUser error: %v", result.Error)
		return false
	}
	return true
}

// sendMessage отправляет сообщение через Bot API
func (s *TelegramService) sendMessage(ctx context.Context, chatID int64, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", s.botToken)
	body := map[string]interface{}{"chat_id": chatID, "text": text}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("Telegram: sendMessage error: %v", err)
		return
	}
	resp.Body.Close()
}

// ===== Генерация и отправка кода входа (on-demand, TTL 5 мин, лимит 5/сутки) =====

const (
	codeTTL        = 10 * time.Minute
	maxCodesPerDay = 5
)

// SendLoginCode генерирует код (8 цифр), сохраняет в Redis с TTL 5 мин,
// отправляет ботом. Контролирует лимит: не более 5 кодов в сутки.
func (s *TelegramService) SendLoginCode(ctx context.Context, userID, phone string) (int, error) {
	var binding model.TelegramBinding
	if err := database.DB.Where("user_id = ?", userID).First(&binding).Error; err != nil {
		return 0, fmt.Errorf("telegram not linked")
	}

	now := time.Now()
	dateKey := "tg_limit:" + userID + ":" + now.Format("20060102")
	failedKey := "tg_failed:" + phone
	_, failed := database.RDB.Get(ctx, failedKey).Result()

	count, _ := database.RDB.Get(ctx, dateKey).Int64()

	// Счётчик попыток растёт только при повторном запросе кода после неудачного ввода.
	if failed == nil && count >= maxCodesPerDay {
		return 0, fmt.Errorf("daily limit reached")
	}

	code := s.GenerateCode()
	codeKey := "tg_code:" + phone
	database.RDB.Set(ctx, codeKey, code, codeTTL)

	if failed == nil {
		count++
		endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(25 * time.Hour)
		database.RDB.Set(ctx, dateKey, count, endOfDay.Sub(now))
	}

	remaining := int(maxCodesPerDay - count)
	msg := fmt.Sprintf("Код для входа: %s\nОсталось попыток сегодня: %d", code, remaining)
	s.sendMessage(ctx, binding.ChatID, msg)

	return remaining, nil
}

// VerifyLoginCode проверяет код, возвращает userID при успехе.
func (s *TelegramService) VerifyLoginCode(ctx context.Context, phone, code string) (string, error) {
	codeKey := "tg_code:" + phone
	failedKey := "tg_failed:" + phone

	stored, err := database.RDB.Get(ctx, codeKey).Result()
	if err != nil || stored == "" {
		return "", fmt.Errorf("code expired or not found")
	}
	if stored != code {
		now := time.Now()
		endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(25 * time.Hour)
		database.RDB.Set(ctx, failedKey, "1", endOfDay.Sub(now))
		return "", fmt.Errorf("invalid code")
	}
	database.RDB.Del(ctx, codeKey)

	var user model.User
	if err := database.DB.Where("phone = ?", phone).First(&user).Error; err != nil {
		return "", fmt.Errorf("user not found")
	}

	database.RDB.Del(ctx, failedKey)
	dateKey := "tg_limit:" + user.ID + ":" + time.Now().Format("20060102")
	database.RDB.Del(ctx, dateKey)

	return user.ID, nil
}

// GenerateLinkToken создаёт одноразовый токен привязки (Redis, TTL 10 мин).
func (s *TelegramService) GenerateLinkToken(ctx context.Context, phone string) (string, error) {
	token := uuid.New().String()
	database.RDB.Set(ctx, "tg_link:"+token, phone, 10*time.Minute)
	log.Printf("Telegram: link token for phone=%s", phone)
	return token, nil
}

// GetBinding возвращает привязку пользователя.
func (s *TelegramService) GetBinding(userID string) *model.TelegramBinding {
	var binding model.TelegramBinding
	if err := database.DB.Where("user_id = ?", userID).First(&binding).Error; err != nil {
		return nil
	}
	return &binding
}

// Unlink отвязывает Telegram от пользователя.
func (s *TelegramService) Unlink(userID string) error {
	res := database.DB.Where("user_id = ?", userID).Delete(&model.TelegramBinding{})
	if res.RowsAffected == 0 {
		return fmt.Errorf("no binding found")
	}
	return nil
}

// HasBinding проверяет, привязан ли Telegram.
func (s *TelegramService) HasBinding(userID string) bool {
	var count int64
	database.DB.Model(&model.TelegramBinding{}).Where("user_id = ?", userID).Count(&count)
	return count > 0
}

// UnlinkByUserID удаляет привязку (при удалении аккаунта).
func (s *TelegramService) UnlinkByUserID(userID string) {
	database.DB.Where("user_id = ?", userID).Delete(&model.TelegramBinding{})
}
