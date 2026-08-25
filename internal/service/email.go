package service

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/smtp"
	"time"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"golang.org/x/net/context"
)

type EmailService struct {
	cfg config.EmailConfig
}

func NewEmailService(cfg config.EmailConfig) *EmailService {
	if cfg.SMTPUser == "" {
		log.Println("EmailService: SMTP_USER not set — skipped")
		return nil
	}
	return &EmailService{cfg: cfg}
}

func (s *EmailService) IsEnabled() bool { return s != nil && s.cfg.SMTPUser != "" }

func (s *EmailService) GenerateCode() string {
	const length = 6
	result := make([]byte, length)
	for i := range result {
		n, _ := rand.Int(rand.Reader, big.NewInt(10))
		result[i] = byte('0' + n.Int64())
	}
	return string(result)
}

const (
	emailCodeTTL        = 5 * time.Minute
	emailMaxCodesPerDay = 5
)

// SendVerificationCode генерирует код, сохраняет в Redis, отправляет на почту.
func (s *EmailService) SendVerificationCode(ctx context.Context, userID, email, phone string) (int, error) {
	if email == "" {
		return 0, fmt.Errorf("email not set")
	}

	dateKey := "email_limit:" + userID + ":" + time.Now().Format("20060102")
	count, _ := database.RDB.Get(ctx, dateKey).Int64()
	if count >= emailMaxCodesPerDay {
		return 0, fmt.Errorf("daily limit reached")
	}

	code := s.GenerateCode()
	codeKey := "email_code:" + phone
	database.RDB.Set(ctx, codeKey, code, emailCodeTTL)

	count++
	now := time.Now()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(25 * time.Hour)
	database.RDB.Set(ctx, dateKey, count, endOfDay.Sub(now))

	remaining := int(emailMaxCodesPerDay - count)

	go func() {
		if err := s.sendEmail(email, code); err != nil {
			log.Printf("Email: failed to send to %s: %v", email, err)
		}
	}()

	return remaining, nil
}

// VerifyCode проверяет код и помечает email как verified.
func (s *EmailService) VerifyCode(ctx context.Context, userID, phone, code string) error {
	codeKey := "email_code:" + phone
	stored, err := database.RDB.Get(ctx, codeKey).Result()
	if err != nil || stored == "" {
		return fmt.Errorf("code expired or not found")
	}
	if stored != code {
		return fmt.Errorf("invalid code")
	}
	database.RDB.Del(ctx, codeKey)

	database.DB.Model(&model.User{}).Where("id = ?", userID).Update("email_verified", true)
	return nil
}

func (s *EmailService) sendEmail(to, code string) error {
	subject := "Код подтверждения RentManager"
	body := fmt.Sprintf("Ваш код для подтверждения почты: %s\n\nКод действителен 5 минут.", code)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		s.cfg.FromAddress, to, subject, body)

	addr := fmt.Sprintf("%s:%s", s.cfg.SMTPHost, s.cfg.SMTPPort)
	auth := smtp.PlainAuth("", s.cfg.SMTPUser, s.cfg.SMTPPassword, s.cfg.SMTPHost)

	return smtp.SendMail(addr, auth, s.cfg.FromAddress, []string{to}, []byte(msg))
}
