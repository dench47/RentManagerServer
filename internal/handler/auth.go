package handler

import (
	"context"
	"log"
	"net/http"
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
	Password string `json:"password" binding:"required"`
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
	jwtCfg    config.JWTConfig
}

func NewAuthHandler(sms *service.SMSService, callCheck *service.CallCheckService, s3 *service.S3Service, fcm *service.FCMService, jwtCfg config.JWTConfig) *AuthHandler {
	return &AuthHandler{callCheck: callCheck, s3: s3, fcm: fcm, jwtCfg: jwtCfg}
}

// notifyNewLogin — отправляет FCM-уведомление на все устройства, кроме текущего
func (h *AuthHandler) notifyNewLogin(userID string, excludeToken string) {
	if h.fcm == nil {
		return
	}
	now := time.Now().Format("15:04")
	go h.fcm.SendToUser(userID, map[string]string{
		"type":  "new_login",
		"title": "Новый вход в аккаунт",
		"body":  "Замечен вход в " + now,
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

// Login — проверяет телефон в БД, отдаёт JWT + refresh_token если существует
func (h *AuthHandler) Login(c *gin.Context) {
	var req struct {
		Phone    string `json:"phone" binding:"required"`
		FcmToken string `json:"fcm_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	var user model.User
	err := database.DB.Where("phone = ?", req.Phone).First(&user).Error
	if err == nil {
		tokenStr, _ := h.generateAccessToken(user)
		refreshToken, _ := h.createRefreshToken(user.ID)
		h.notifyNewLogin(user.ID, req.FcmToken)
		c.JSON(http.StatusOK, gin.H{
			"access_token":  tokenStr,
			"refresh_token": refreshToken,
			"user":          user,
			"exists":        true,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"exists": false, "need_verify": true})
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

// CallCheckStatus — проверяет статус звонка, создаёт пользователя, выдаёт токены
func (h *AuthHandler) CallCheckStatus(c *gin.Context) {
	var req struct {
		Phone    string `json:"phone" binding:"required"`
		FcmToken string `json:"fcm_token"`
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
	err = database.DB.Where("phone = ?", req.Phone).First(&user).Error
	if err != nil {
		user = model.User{
			BaseModel: model.BaseModel{ID: uuid.New().String()},
			Phone:     req.Phone,
		}
		database.DB.Create(&user)
	}
	tokenStr, _ := h.generateAccessToken(user)
	refreshToken, _ := h.createRefreshToken(user.ID)
	h.notifyNewLogin(user.ID, req.FcmToken)
	c.JSON(http.StatusOK, gin.H{
		"verified":      true,
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
	if err := database.DB.Where("token = ? AND expires_at > ?", req.RefreshToken, time.Now().UnixMilli()).First(&rt).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired refresh token"})
		return
	}

	database.DB.Delete(&rt)

	var user model.User
	if err := database.DB.First(&user, "id = ?", rt.UserID).Error; err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "user not found"})
		return
	}

	accessToken, _ := h.generateAccessToken(user)
	newRefreshToken, _ := h.createRefreshToken(user.ID)

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
		Password string `json:"password" binding:"required"`
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

// LogoutAll — удаляет все refresh-токены и инкрементит token_version
func (h *AuthHandler) LogoutAll(c *gin.Context) {
	userID := c.GetString("userID")
	database.DB.Where("user_id = ?", userID).Delete(&model.RefreshToken{})
	database.DB.Model(&model.User{}).Where("id = ?", userID).
		Update("token_version", gormlib.Expr("token_version + 1"))

	// Отправляем push-уведомление на все устройства пользователя
	if h.fcm != nil {
		go h.fcm.SendToUser(userID, map[string]string{
			"type": "logout_all",
		}, "")
	}

	c.JSON(http.StatusOK, gin.H{"message": "logged out from all devices"})
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

// DeleteAccount — жёсткое удаление пользователя и всех его данных
func (h *AuthHandler) DeleteAccount(c *gin.Context) {
	userID := c.GetString("userID")

	database.DB.Where("user_id = ?", userID).Delete(&model.RefreshToken{})
	database.DB.Where("property_id IN (SELECT id FROM properties WHERE user_id = ?)", userID).Delete(&model.Photo{})
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.Property{})
	database.DB.Unscoped().Where("owner_id = ?", userID).Delete(&model.Tenant{})
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.Meter{})
	database.DB.Where("sender_id = ?", userID).Delete(&model.Message{})
	database.DB.Unscoped().Where("participant_ids LIKE ?", "%\""+userID+"\"%").Delete(&model.Chat{})
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.Payment{})
	database.DB.Unscoped().Where("user_id = ?", userID).Delete(&model.PaymentSchedule{})

	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err == nil && user.AvatarURL != "" {
		if h.s3 != nil {
			key := h.s3.ExtractKey(user.AvatarURL)
			if key != "" {
				if err := h.s3.Delete(context.Background(), key); err != nil {
					log.Printf("WARNING: failed to delete avatar from S3 during account deletion: %v", err)
				}
			}
		}
	}

	database.DB.Unscoped().Where("id = ?", userID).Delete(&model.User{})
	c.JSON(http.StatusOK, gin.H{"message": "account deleted"})
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
