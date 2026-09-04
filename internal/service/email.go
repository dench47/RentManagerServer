package service

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log"
	"math/big"
	"net/smtp"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"
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

// SendLoginCode генерирует код входа, сохраняет в Redis и отправляет на подтверждённую почту.
// Используется при входе с нового устройства через email (аналог Telegram-входа).
func (s *EmailService) SendLoginCode(ctx context.Context, phone string) (int, error) {
	var user model.User
	if err := database.DB.Where("phone = ?", phone).First(&user).Error; err != nil {
		return 0, fmt.Errorf("user not found")
	}
	if user.Email == "" || !user.EmailVerified {
		return 0, fmt.Errorf("email not verified")
	}

	dateKey := "email_login_limit:" + user.ID + ":" + time.Now().Format("20060102")
	count, _ := database.RDB.Get(ctx, dateKey).Int64()
	if count >= emailMaxCodesPerDay {
		return 0, fmt.Errorf("daily limit reached")
	}

	code := s.GenerateCode()
	codeKey := "email_login_code:" + phone
	database.RDB.Set(ctx, codeKey, code, emailCodeTTL)

	count++
	now := time.Now()
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(25 * time.Hour)
	database.RDB.Set(ctx, dateKey, count, endOfDay.Sub(now))

	remaining := int(emailMaxCodesPerDay - count)

	go func() {
		if err := s.sendLoginEmail(user.Email, code); err != nil {
			log.Printf("Email: failed to send login code to %s: %v", user.Email, err)
		}
	}()

	return remaining, nil
}

// VerifyLoginCode проверяет код входа, возвращает userID при успехе.
func (s *EmailService) VerifyLoginCode(ctx context.Context, phone, code string) (string, error) {
	codeKey := "email_login_code:" + phone
	stored, err := database.RDB.Get(ctx, codeKey).Result()
	if err != nil || stored == "" {
		return "", fmt.Errorf("code expired or not found")
	}
	if stored != code {
		return "", fmt.Errorf("invalid code")
	}
	database.RDB.Del(ctx, codeKey)

	var user model.User
	if err := database.DB.Where("phone = ?", phone).First(&user).Error; err != nil {
		return "", fmt.Errorf("user not found")
	}
	return user.ID, nil
}

func (s *EmailService) sendLoginEmail(to, code string) error {
	subject := "Код входа RentManager"
	body := fmt.Sprintf("Ваш код для входа: %s\n\nКод действителен 5 минут. Если вы не запрашивали вход, проигнорируйте письмо.", code)
	return s.send(subject, to, body)
}

func (s *EmailService) sendEmail(to, code string) error {
	subject := "Код подтверждения RentManager"
	body := fmt.Sprintf("Ваш код для подтверждения почты: %s\n\nКод действителен 5 минут.", code)
	return s.send(subject, to, body)
}

// toASCIIAddress переводит доменную часть адреса в punycode (для SMTP-конверта MAIL FROM).
// Кириллические домены (например .рф) почтовый сервер хранит в punycode, а юникод не матчит.
func toASCIIAddress(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return addr
	}
	local := addr[:at]
	domain := addr[at+1:]
	ascii, err := idna.ToASCII(domain)
	if err != nil || ascii == "" {
		return addr
	}
	return local + "@" + ascii
}

// send отправляет письмо. Порт 465 — implicit TLS (SSL), иначе STARTTLS (587) / plain (25).
func (s *EmailService) send(subject, to, body string) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		s.cfg.FromAddress, to, subject, body)

	addr := fmt.Sprintf("%s:%s", s.cfg.SMTPHost, s.cfg.SMTPPort)
	auth := &loginAuth{username: s.cfg.SMTPUser, password: s.cfg.SMTPPassword}

	if s.cfg.SMTPPort == "465" {
		return s.sendSSL(addr, auth, s.cfg.SMTPHost, to, msg)
	}
	return smtp.SendMail(addr, auth, toASCIIAddress(s.cfg.FromAddress), []string{to}, []byte(msg))
}

// loginAuth реализует smtp.Auth через механизм LOGIN (Beget не поддерживает PLAIN).
type loginAuth struct {
	username string
	password string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	s := strings.ToLower(string(fromServer))
	switch {
	case strings.Contains(s, "username"):
		return []byte(a.username), nil
	case strings.Contains(s, "password"):
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("unexpected server challenge: %q", fromServer)
	}
}

func (s *EmailService) sendSSL(addr string, auth smtp.Auth, host, to, msg string) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return fmt.Errorf("tls dial: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := client.Mail(toASCIIAddress(s.cfg.FromAddress)); err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close data: %w", err)
	}
	return client.Quit()
}
