package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/database"
	"rentmanager-server/internal/middleware"
	"rentmanager-server/internal/model"
	"rentmanager-server/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	gormlib "gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SendCodeRequest struct {
	Phone string `json:"phone" binding:"required"`
}

type SaveNameRequest struct {
	Name string `json:"name" binding:"required"`
}

type UpdateProfileRequest struct {
	Name               string  `json:"name"`
	FullName           string  `json:"full_name"`
	Email              *string `json:"email"`
	LegalName          *string `json:"legal_name"`
	AvatarURL          *string `json:"avatar_url"`
	DefaultStartScreen string  `json:"default_start_screen"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type SetPasswordRequest struct {
	Password string `json:"password"`
}

type AuthResponse struct {
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	User         model.User `json:"user"`
}

type AuthHandler struct {
	callCheck *service.CallCheckService
	s3        *service.S3Service
	fcm       *service.FCMService
	telegram  *service.TelegramService
	jwtCfg    config.JWTConfig
	uploadDir string
}

func NewAuthHandler(sms *service.SMSService, callCheck *service.CallCheckService, s3 *service.S3Service, fcm *service.FCMService, telegram *service.TelegramService, jwtCfg config.JWTConfig, uploadDir string) *AuthHandler {
	return &AuthHandler{callCheck: callCheck, s3: s3, fcm: fcm, telegram: telegram, jwtCfg: jwtCfg, uploadDir: uploadDir}
}

// notifyNewLogin — отправляет FCM-уведомление на все устройства, кроме текущего
func (h *AuthHandler) notifyNewLogin(userID string, excludeToken string) {
	if h.fcm == nil {
		return
	}
	now := time.Now()
	go h.fcm.SendToUser(userID, map[string]string{
		"type":      "new_login",
		"title":     "Новый вход в аккаунт",
		"body":      "Замечен вход в " + now.Format("15:04"),
		"timestamp": strconv.FormatInt(now.Unix(), 10),
	}, excludeToken)
}

// PinAttempts — возвращает оставшееся количество попыток ввода PIN
func (h *AuthHandler) PinAttempts(c *gin.Context) {
	phone := c.Query("phone")
	if phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	key := "pin_attempts:" + phone
	val, err := database.RDB.Get(c, key).Int64()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"attempts_left": 5})
		return
	}
	remaining := 5 - int(val)
	if remaining < 0 {
		remaining = 0
	}
	c.JSON(http.StatusOK, gin.H{"attempts_left": remaining})
}

func (h *AuthHandler) SaveName(c *gin.Context) {
	userID := c.GetString("userID")
	var req SaveNameRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
		return
	}
	if err := database.DB.Model(&model.User{}).Where("id = ?", userID).Update("name", req.Name).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save name"})
		return
	}
	var user model.User
	database.DB.First(&user, "id = ?", userID)
	c.JSON(http.StatusOK, user)
}

// isTrustedDevice — проверяет, верифицировано ли устройство для пользователя.
func (h *AuthHandler) isTrustedDevice(userID, deviceID string) bool {
	if deviceID == "" {
		return false
	}
	var count int64
	database.DB.Model(&model.TrustedDevice{}).
		Where("user_id = ? AND device_id = ?", userID, deviceID).
		Count(&count)
	return count > 0
}

// trustDevice — добавляет устройство в доверенные (или обновляет время последнего входа).
// При создании записи все устройства пользователя получают push devices_changed,
// чтобы открытые списки устройств обновились мгновенно.
func (h *AuthHandler) trustDevice(userID, deviceID, deviceName string) {
	if deviceID == "" {
		return
	}
	var dev model.TrustedDevice
	err := database.DB.Where("user_id = ? AND device_id = ?", userID, deviceID).First(&dev).Error
	if err != nil {
		dev = model.TrustedDevice{
			ID:         uuid.New().String(),
			UserID:     userID,
			DeviceID:   deviceID,
			Name:       deviceName,
			LastUsedAt: time.Now().UnixMilli(),
		}
		if err := database.DB.Create(&dev).Error; err != nil {
			log.Printf("trustDevice: create failed: %v", err)
			return
		}
		if h.fcm != nil {
			go h.fcm.SendToUser(userID, map[string]string{
				"type": "devices_changed",
			}, "")
		}
	} else {
		database.DB.Model(&dev).Update("last_used_at", time.Now().UnixMilli())
	}
}

// Login — проверяет телефон в БД и определяет сценарий входа.
// Токены выдаются ТОЛЬКО доверенному устройству, у которого не установлен PIN.
// Новое устройство проходит подтверждение: push-одобрение или звонок (callcheck).
func (h *AuthHandler) Login(c *gin.Context) {
	var req struct {
		Phone      string `json:"phone" binding:"required"`
		FcmToken   string `json:"fcm_token"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	var user model.User
	err := database.DB.Where("phone = ?", req.Phone).First(&user).Error
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"exists": false, "need_verify": true})
		return
	}

	trusted := h.isTrustedDevice(user.ID, req.DeviceID)

	// Доверенное устройство без PIN — выдаём токены сразу (пользователь не защищает аккаунт PIN).
	// Дыра старой логики закрыта: для НЕдоверенного устройства токены больше не выдаются.
	if trusted && user.PasswordHash == "" {
		tokenStr, _ := h.generateAccessToken(user)
		refreshToken, _ := h.createRefreshToken(user.ID)
		h.notifyNewLogin(user.ID, req.FcmToken)
		c.JSON(http.StatusOK, gin.H{
			"exists":            true,
			"has_password":      false,
			"is_trusted_device": true,
			"access_token":      tokenStr,
			"refresh_token":     refreshToken,
			"user":              user,
		})
		return
	}

	// Есть ли другие доверенные устройства → можно подтвердить вход push'ем
	var trustedCount int64
	database.DB.Model(&model.TrustedDevice{}).
		Where("user_id = ?", user.ID).
		Count(&trustedCount)

	canPush := trustedCount > 0 && h.fcm != nil
	canTelegram := h.telegram != nil && h.telegram.IsEnabled() && h.telegram.HasBinding(user.ID)
	log.Printf("LOGIN: phone=%s device=%s trusted=%v hasPassword=%v canPush=%v canTelegram=%v",
		req.Phone, req.DeviceID, trusted, user.HasPassword, canPush, canTelegram)

	c.JSON(http.StatusOK, gin.H{
		"exists":               true,
		"has_password":         user.HasPassword,
		"is_trusted_device":    trusted,
		"can_push":             canPush,
		"can_telegram":         canTelegram,
		"name":                 user.Name,
		"phone":                user.Phone,
		"default_start_screen": user.DefaultStartScreen,
	})
}

// CallCheckAdd — инициирует звонок через sms.ru
func (h *AuthHandler) CallCheckAdd(c *gin.Context) {
	var req SendCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	result, err := h.callCheck.AddCallCheck(req.Phone)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "callcheck failed"})
		return
	}
	database.RDB.Set(c, "callcheck:"+req.Phone, result.CheckID, 10*time.Minute).Err()
	c.JSON(http.StatusOK, gin.H{
		"status":            result.Status,
		"check_id":          result.CheckID,
		"call_phone":        result.CallPhone,
		"call_phone_pretty": result.CallPhonePretty,
	})
}

// CallCheckStatus — проверяет статус звонка, создаёт пользователя, выдаёт токены.
// Успешный звонок = доказательство владения номером → устройство становится доверенным.
func (h *AuthHandler) CallCheckStatus(c *gin.Context) {
	var req struct {
		Phone      string `json:"phone" binding:"required"`
		FcmToken   string `json:"fcm_token"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	checkID, _ := database.RDB.Get(c, "callcheck:"+req.Phone).Result()
	if checkID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no check_id found"})
		return
	}
	verified, err := h.callCheck.CheckStatus(checkID)
	if err != nil || !verified {
		c.JSON(http.StatusOK, gin.H{"verified": false})
		return
	}
	var user model.User
	isNewUser := false
	err = database.DB.Where("phone = ?", req.Phone).First(&user).Error
	if err != nil {
		user = model.User{
			BaseModel: model.BaseModel{ID: uuid.New().String()},
			Phone:     req.Phone,
		}
		database.DB.Create(&user)
		isNewUser = true
	}
	tokenStr, _ := h.generateAccessToken(user)
	refreshToken, _ := h.createRefreshToken(user.ID)
	h.trustDevice(user.ID, req.DeviceID, req.DeviceName)
	h.notifyNewLogin(user.ID, req.FcmToken)
	log.Printf("CALLCHECK: verified phone=%s new_user=%v device=%s (%s)",
		req.Phone, isNewUser, req.DeviceID, req.DeviceName)
	c.JSON(http.StatusOK, gin.H{
		"verified":      true,
		"is_new_user":   isNewUser,
		"has_password":  user.HasPassword,
		"access_token":  tokenStr,
		"refresh_token": refreshToken,
		"user":          user,
	})
}

// GetMe — профиль текущего пользователя
func (h *AuthHandler) GetMe(c *gin.Context) {
	userID := c.GetString("userID")
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	c.JSON(http.StatusOK, user)
}

// RefreshToken — обновляет access-токен по refresh-токену (ротация)
func (h *AuthHandler) RefreshToken(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refresh_token required"})
		return
	}

	var rt model.RefreshToken
	err := database.DB.Transaction(func(tx *gormlib.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("token = ? AND expires_at > ?", req.RefreshToken, time.Now().UnixMilli()).
			First(&rt).Error; err != nil {
			return err
		}
		return tx.Delete(&rt).Error
	})
	if err != nil {
		if errors.Is(err, gormlib.ErrRecordNotFound) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired refresh token"})
		} else {
			log.Printf("refresh failed: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "refresh failed"})
		}
		return
	}

	var user model.User
	if err := database.DB.First(&user, "id = ?", rt.UserID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}

	accessToken, err := h.generateAccessToken(user)
	if err != nil {
		log.Printf("refresh: generate access token: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refresh failed"})
		return
	}
	newRefreshToken, err := h.createRefreshToken(user.ID)
	if err != nil {
		log.Printf("refresh: create refresh token: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refresh failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": newRefreshToken,
	})
}

// UpdateProfile — обновление name, email, legal_name, avatar_url
func (h *AuthHandler) UpdateProfile(c *gin.Context) {
	userID := c.GetString("userID")
	var req UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	updates := map[string]interface{}{}
	if req.Name != "" {
		updates["name"] = req.Name
	}
	if req.FullName != "" {
		updates["full_name"] = req.FullName
	}
	if req.Email != nil {
		updates["email"] = *req.Email
	}
	if req.LegalName != nil {
		updates["legal_name"] = *req.LegalName
	}
	if req.AvatarURL != nil {
		var current model.User
		if err := database.DB.First(&current, "id = ?", userID).Error; err == nil && current.AvatarURL != "" && current.AvatarURL != *req.AvatarURL {
			if h.s3 != nil {
				key := h.s3.ExtractKey(current.AvatarURL)
				if key != "" {
					if err := h.s3.Delete(context.Background(), key); err != nil {
						log.Printf("WARNING: failed to delete old avatar from S3: %v", err)
					}
				}
			}
		}
		updates["avatar_url"] = *req.AvatarURL
	}
	if req.DefaultStartScreen != "" || req.AvatarURL != nil || req.Name != "" || req.Email != nil || req.LegalName != nil {
		updates["default_start_screen"] = req.DefaultStartScreen
	}
	if len(updates) > 0 {
		database.DB.Model(&model.User{}).Where("id = ?", userID).Updates(updates)
	}
	var user model.User
	database.DB.First(&user, "id = ?", userID)
	c.JSON(http.StatusOK, user)
}

// ChangePhone — инициирует смену номера (реверификация)
func (h *AuthHandler) ChangePhone(c *gin.Context) {
	var req SendCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	result, err := h.callCheck.AddCallCheck(req.Phone)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "callcheck failed"})
		return
	}
	database.RDB.Set(c, "phonechange:"+req.Phone, result.CheckID, 10*time.Minute).Err()
	c.JSON(http.StatusOK, gin.H{
		"status":            result.Status,
		"call_phone":        result.CallPhone,
		"call_phone_pretty": result.CallPhonePretty,
	})
}

// ConfirmPhoneChange — подтверждает смену номера после звонка
func (h *AuthHandler) ConfirmPhoneChange(c *gin.Context) {
	userID := c.GetString("userID")
	var req SendCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	checkID, _ := database.RDB.Get(c, "phonechange:"+req.Phone).Result()
	if checkID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no pending phone change"})
		return
	}
	verified, err := h.callCheck.CheckStatus(checkID)
	if err != nil || !verified {
		c.JSON(http.StatusOK, gin.H{"changed": false})
		return
	}
	database.DB.Model(&model.User{}).Where("id = ?", userID).Update("phone", req.Phone)
	var user model.User
	database.DB.First(&user, "id = ?", userID)
	tokenStr, _ := h.generateAccessToken(user)
	refreshToken, _ := h.createRefreshToken(user.ID)
	c.JSON(http.StatusOK, gin.H{
		"changed":       true,
		"access_token":  tokenStr,
		"refresh_token": refreshToken,
		"user":          user,
	})
}

// SetPassword — установить или изменить пароль (PIN)
func (h *AuthHandler) SetPassword(c *gin.Context) {
	userID := c.GetString("userID")
	var req SetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	// Пустой пароль = удаление
	if len(req.Password) == 0 {
		if err := database.DB.Model(&model.User{}).Where("id = ?", userID).Update("password_hash", "").Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove password"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "password removed"})
		return
	}

	if len(req.Password) < 4 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 4 characters"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
		return
	}

	if err := database.DB.Model(&model.User{}).Where("id = ?", userID).Update("password_hash", string(hash)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save password"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "password set"})
}

// VerifyPassword — проверяет пароль (PIN) по номеру телефона, возвращает токены
func (h *AuthHandler) VerifyPassword(c *gin.Context) {
	var req struct {
		Phone    string `json:"phone" binding:"required"`
		Password string `json:"password"`
		FcmToken string `json:"fcm_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone and password required"})
		return
	}

	var user model.User
	if err := database.DB.Where("phone = ?", req.Phone).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	if user.PasswordHash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password not set"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		// Инкрементируем счётчик неудачных попыток в Redis
		key := "pin_attempts:" + req.Phone
		attempts, _ := database.RDB.Incr(c, key).Result()
		database.RDB.Expire(c, key, 30*time.Minute)

		remainingAttempts := 5 - int(attempts)
		if remainingAttempts <= 0 {
			// Сброс пароля и логаут со всех устройств
			database.DB.Model(&model.User{}).Where("phone = ?", req.Phone).Update("password_hash", "")
			database.DB.Where("user_id = ?", user.ID).Delete(&model.RefreshToken{})
			database.DB.Model(&model.User{}).Where("id = ?", user.ID).
				Update("token_version", gormlib.Expr("token_version + 1"))
			database.RDB.Del(c, key)

			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":         "too many attempts",
				"attempts_left": 0,
			})
			return
		}

		c.JSON(http.StatusUnauthorized, gin.H{
			"error":         "wrong password",
			"attempts_left": remainingAttempts,
		})
		return
	}

	// Успешный вход — сбрасываем счётчик
	database.RDB.Del(c, "pin_attempts:"+req.Phone)

	tokenStr, _ := h.generateAccessToken(user)
	refreshToken, _ := h.createRefreshToken(user.ID)
	h.notifyNewLogin(user.ID, req.FcmToken)
	c.JSON(http.StatusOK, gin.H{
		"access_token":  tokenStr,
		"refresh_token": refreshToken,
		"user":          user,
	})
}

// loginRequestPayload — структура запроса подтверждения входа в Redis.
type loginRequestPayload struct {
	UserID     string `json:"user_id"`
	Phone      string `json:"phone"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Status     string `json:"status"` // pending / approved / denied
}

func (h *AuthHandler) readLoginRequest(c *gin.Context, requestID string) (*loginRequestPayload, error) {
	raw, err := database.RDB.Get(c, "login_request:"+requestID).Result()
	if err != nil {
		return nil, errors.New("request expired")
	}
	var p loginRequestPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, errors.New("corrupted request")
	}
	return &p, nil
}

func (h *AuthHandler) writeLoginRequest(c *gin.Context, p *loginRequestPayload, requestID string) {
	data, _ := json.Marshal(p)
	database.RDB.Set(c, "login_request:"+requestID, data, 5*time.Minute)
}

// RequestLoginApproval — новое устройство запрашивает подтверждение входа.
// На все доверенные устройства пользователя уходит push с request_id.
func (h *AuthHandler) RequestLoginApproval(c *gin.Context) {
	var req struct {
		Phone      string `json:"phone" binding:"required"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}

	var user model.User
	if err := database.DB.Where("phone = ?", req.Phone).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	requestID := uuid.New().String()
	p := &loginRequestPayload{
		UserID:     user.ID,
		Phone:      req.Phone,
		DeviceID:   req.DeviceID,
		DeviceName: req.DeviceName,
		Status:     "pending",
	}
	h.writeLoginRequest(c, p, requestID)

	if h.fcm != nil {
		name := req.DeviceName
		if name == "" {
			name = "нового устройства"
		} else {
			name = "устройства " + name
		}
		go h.fcm.SendToUser(user.ID, map[string]string{
			"type":        "login_request",
			"request_id":  requestID,
			"title":       "Подтвердите вход",
			"body":        "Вход с " + name,
			"device_name": req.DeviceName,
			"timestamp":   strconv.FormatInt(time.Now().Unix(), 10),
		}, "")
	}

	c.JSON(http.StatusOK, gin.H{"request_id": requestID, "expires_in": 300})
}

// LoginStatus — новое устройство опрашивает статус подтверждения входа.
// При одобрении выдаёт токены и добавляет устройство в доверенные.
func (h *AuthHandler) LoginStatus(c *gin.Context) {
	requestID := c.Query("request_id")
	deviceID := c.Query("device_id")
	if requestID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request_id required"})
		return
	}
	p, err := h.readLoginRequest(c, requestID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"status": "expired"})
		return
	}
	switch p.Status {
	case "approved":
		database.RDB.Del(c, "login_request:"+requestID)
		var user model.User
		if err := database.DB.First(&user, "id = ?", p.UserID).Error; err != nil {
			c.JSON(http.StatusOK, gin.H{"status": "denied"})
			return
		}
		tokenStr, _ := h.generateAccessToken(user)
		refreshToken, _ := h.createRefreshToken(user.ID)
		h.trustDevice(user.ID, deviceID, p.DeviceName)
		log.Printf("LOGIN_STATUS: request=%s approved for user=%s, trusted device added (%s)",
			requestID, p.UserID, deviceID)
		c.JSON(http.StatusOK, gin.H{
			"status":        "approved",
			"is_new_user":   false,
			"has_password":  user.HasPassword,
			"access_token":  tokenStr,
			"refresh_token": refreshToken,
			"user":          user,
		})
	case "denied":
		database.RDB.Del(c, "login_request:"+requestID)
		c.JSON(http.StatusOK, gin.H{"status": "denied"})
	default:
		c.JSON(http.StatusOK, gin.H{"status": "pending"})
	}
}

// Также сбрасывает доверенные устройства: после этого каждое устройство
// заново проходит подтверждение (push/звонок) при следующем входе.
func (h *AuthHandler) LogoutAll(c *gin.Context) {
	userID := c.GetString("userID")
	database.DB.Where("user_id = ?", userID).Delete(&model.RefreshToken{})
	database.DB.Where("user_id = ?", userID).Delete(&model.TrustedDevice{})
	database.DB.Model(&model.User{}).Where("id = ?", userID).
		Update("token_version", gormlib.Expr("token_version + 1"))

	// Отправляем push-уведомление на все устройства пользователя и очищаем токены
	if h.fcm != nil {
		h.fcm.SendToUser(userID, map[string]string{
			"type": "logout_all",
		}, "")
		h.fcm.ClearUserTokens(userID)
	}

	c.JSON(http.StatusOK, gin.H{"message": "logged out from all devices"})
}

// ApproveLogin — доверенное устройство одобряет вход с нового устройства (protected).
func (h *AuthHandler) ApproveLogin(c *gin.Context) {
	userID := c.GetString("userID")
	var req struct {
		RequestID string `json:"request_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request_id required"})
		return
	}
	p, err := h.readLoginRequest(c, req.RequestID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "request expired"})
		return
	}
	if p.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "foreign request"})
		return
	}
	p.Status = "approved"
	h.writeLoginRequest(c, p, req.RequestID)
	c.JSON(http.StatusOK, gin.H{"message": "approved"})
}

// DenyLogin — доверенное устройство отклоняет попытку входа (protected).
func (h *AuthHandler) DenyLogin(c *gin.Context) {
	userID := c.GetString("userID")
	var req struct {
		RequestID string `json:"request_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request_id required"})
		return
	}
	p, err := h.readLoginRequest(c, req.RequestID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "request expired"})
		return
	}
	if p.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "foreign request"})
		return
	}
	p.Status = "denied"
	h.writeLoginRequest(c, p, req.RequestID)
	c.JSON(http.StatusOK, gin.H{"message": "denied"})
}

// TrustedDeviceDto — доверенное устройство для отдачи клиенту.
type TrustedDeviceDto struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	CreatedAt     int64  `json:"created_at"`
	LastUsedAt    int64  `json:"last_used_at"`
	CurrentDevice bool   `json:"current_device"`
}

// ListDevices — список доверенных устройств текущего пользователя (protected).
func (h *AuthHandler) ListDevices(c *gin.Context) {
	userID := c.GetString("userID")
	currentDeviceID := c.Query("current_device_id")
	var devices []model.TrustedDevice
	database.DB.Where("user_id = ?", userID).Order("created_at ASC").Find(&devices)

	result := make([]TrustedDeviceDto, 0, len(devices))
	for _, d := range devices {
		result = append(result, TrustedDeviceDto{
			ID:            d.ID,
			Name:          d.Name,
			CreatedAt:     d.CreatedAt,
			LastUsedAt:    d.LastUsedAt,
			CurrentDevice: currentDeviceID != "" && d.DeviceID == currentDeviceID,
		})
	}
	c.JSON(http.StatusOK, result)
}

// RevokeDevice — отзывает доверенное устройство по его ID (protected).
// При отзыве отправляет FCM push device_revoked на все устройства пользователя:
// отозванное устройство получает сигнал на мгновенный разлогин (logout_all),
// остальные — обновляют список доверенных устройств.
func (h *AuthHandler) RevokeDevice(c *gin.Context) {
	userID := c.GetString("userID")
	deviceRowID := c.Param("deviceId")

	// Читаем device_id до удаления — нужен для push
	var dev model.TrustedDevice
	database.DB.Where("user_id = ? AND id = ?", userID, deviceRowID).First(&dev)

	res := database.DB.Where("user_id = ? AND id = ?", userID, deviceRowID).Delete(&model.TrustedDevice{})
	if res.Error != nil || res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		return
	}

	// Push на ВСЕ устройства: у отозванного — логаут, у остальных — обновление списка
	if h.fcm != nil && dev.DeviceID != "" {
		go h.fcm.SendToUser(userID, map[string]string{
			"type":      "device_revoked",
			"device_id": dev.DeviceID,
		}, "")
		log.Printf("REVOKE: device=%s (%s) revoked by user=%s — push sent", dev.DeviceID, dev.Name, userID)
	}

	c.JSON(http.StatusOK, gin.H{"message": "device revoked"})
}

// RegisterDevice — регистрирует FCM-токен для push-уведомлений
func (h *AuthHandler) RegisterDevice(c *gin.Context) {
	userID := c.GetString("userID")
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
		return
	}

	if h.fcm != nil {
		h.fcm.RegisterToken(userID, req.Token)
	}

	c.JSON(http.StatusOK, gin.H{"message": "device registered"})
}

// UnregisterDevice — удаляет FCM-токен устройства (вызывается при логауте)
func (h *AuthHandler) UnregisterDevice(c *gin.Context) {
	userID := c.GetString("userID")
	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
		return
	}

	if h.fcm != nil {
		h.fcm.RemoveToken(userID, req.Token)
	}

	c.JSON(http.StatusOK, gin.H{"message": "device unregistered"})
}

// DeleteAccount — жёсткое удаление пользователя и всех его данных
func (h *AuthHandler) DeleteAccount(c *gin.Context) {
	userID := c.GetString("userID")

	// Разлогиниваем ВСЕ устройства ДО удаления аккаунта:
	// push logout_all (FCM-токены ещё живы) + отзыв refresh-токенов.
	// Без этого другие устройства продолжали бы работать: access-токен до часа,
	// refresh — до 30 дней, бесконечно обновляясь.
	if h.fcm != nil {
		h.fcm.SendToUser(userID, map[string]string{
			"type": "logout_all",
		}, "")
	}
	database.DB.Where("user_id = ?", userID).Delete(&model.RefreshToken{})
	database.DB.Where("user_id = ?", userID).Delete(&model.TrustedDevice{})
	if h.telegram != nil {
		h.telegram.UnlinkByUserID(userID)
	}

	// ID объектов пользователя — нужны для удаления связанных записей (фото, брони, чаты и т.д.)
	var propertyIDs []string
	database.DB.Model(&model.Property{}).Unscoped().
		Where("user_id = ?", userID).Pluck("id", &propertyIDs)

	// ID чатов, участником которых является пользователь
	var chatIDs []string
	database.DB.Model(&model.Chat{}).Unscoped().
		Where("participant_ids::text LIKE ?", "%\""+userID+"\"%").
		Pluck("id", &chatIDs)

	// ID записей арендатора, где пользователь выступал арендатором у других арендодателей
	var tenantIDs []string
	database.DB.Model(&model.Tenant{}).Unscoped().
		Where("user_id = ?", userID).Pluck("id", &tenantIDs)

	// Собираем URL файлов (аватар + фотографии объектов), чтобы удалить их из S3 / с локального диска
	var fileURLs []string
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err == nil && user.AvatarURL != "" {
		fileURLs = append(fileURLs, user.AvatarURL)
	}
	if len(propertyIDs) > 0 {
		var photoURLs []string
		database.DB.Model(&model.Photo{}).Where("property_id IN ?", propertyIDs).Pluck("url", &photoURLs)
		fileURLs = append(fileURLs, photoURLs...)
	}

	// ---- Удаление записей из БД ----
	database.DB.Where("user_id = ?", userID).Delete(&model.RefreshToken{})

	// Фото и брони объектов пользователя
	if len(propertyIDs) > 0 {
		database.DB.Where("property_id IN ?", propertyIDs).Delete(&model.Photo{})
		database.DB.Unscoped().Where("property_id IN ?", propertyIDs).Delete(&model.Booking{})
	}

	// Сообщения: во всех чатах пользователя (включая полученные) + страховка по отправителю
	if len(chatIDs) > 0 {
		database.DB.Where("chat_id IN ?", chatIDs).Delete(&model.Message{})
	}
	database.DB.Where("sender_id = ?", userID).Delete(&model.Message{})
	if len(chatIDs) > 0 {
		database.DB.Unscoped().Where("id IN ?", chatIDs).Delete(&model.Chat{})
	}

	// Объекты, счётчики, платежи и графики платежей
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.Property{})
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.Meter{})
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.Payment{})
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.PaymentSchedule{})

	// Арендаторы, которыми владеет пользователь
	database.DB.Unscoped().Where("owner_id = ?", userID).Delete(&model.Tenant{})

	// Если пользователь был арендатором у других арендодателей — отвязываем его от их объектов
	if len(tenantIDs) > 0 {
		database.DB.Model(&model.Property{}).
			Where("tenant_id IN ?", tenantIDs).
			Updates(map[string]interface{}{"tenant_id": nil, "status": "free"})
		database.DB.Unscoped().Where("id IN ?", tenantIDs).Delete(&model.Tenant{})
	}

	// ---- Удаление файлов из S3 / локального диска ----
	for _, rawURL := range fileURLs {
		h.deleteStoredFile(rawURL)
	}

	// ---- Удаление самого аккаунта ----
	database.DB.Unscoped().Where("id = ?", userID).Delete(&model.User{})
	if h.fcm != nil {
		h.fcm.ClearUserTokens(userID)
	}
	c.JSON(http.StatusOK, gin.H{"message": "account deleted"})
}

// deleteStoredFile удаляет файл из S3, а при fallback на локальный диск — с диска.
func (h *AuthHandler) deleteStoredFile(rawURL string) {
	if rawURL == "" {
		return
	}

	// S3
	if h.s3 != nil {
		if key := h.s3.ExtractKey(rawURL); key != "" {
			if err := h.s3.Delete(context.Background(), key); err != nil {
				log.Printf("WARNING: failed to delete file from S3 during account deletion: %v", err)
			}
			return
		}
	}

	// Локальный диск (fallback): URL вида http(s)://host/uploads/<file>
	if filename := localUploadFilename(rawURL); filename != "" && h.uploadDir != "" {
		p := filepath.Join(h.uploadDir, filename)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("WARNING: failed to delete local file %s during account deletion: %v", p, err)
		}
	}
}

// localUploadFilename извлекает имя файла из URL локального upload-эндпоинта (/uploads/<file>).
func localUploadFilename(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return ""
	}
	if !strings.Contains(u.Path, "/uploads/") {
		return ""
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		return ""
	}
	return name
}

func (h *AuthHandler) createRefreshToken(userID string) (string, error) {
	token := uuid.New().String()
	expiresAt := time.Now().Add(time.Duration(h.jwtCfg.RefreshTTL) * time.Second).UnixMilli()

	rt := model.RefreshToken{
		ID:        uuid.New().String(),
		UserID:    userID,
		Token:     token,
		ExpiresAt: expiresAt,
	}
	if err := database.DB.Create(&rt).Error; err != nil {
		return "", err
	}
	return token, nil
}

func (h *AuthHandler) generateAccessToken(user model.User) (string, error) {
	claims := &middleware.Claims{
		UserID:       user.ID,
		Phone:        user.Phone,
		TokenVersion: user.TokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(h.jwtCfg.AccessTTL) * time.Second)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(h.jwtCfg.Secret))
}
