package handler

import (
	"encoding/json"
	"log"
	"net/http"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"github.com/gin-gonic/gin"
)

// EmailSendCode — отправляет код подтверждения на почту пользователя.
func (h *AuthHandler) EmailSendCode(c *gin.Context) {
	if h.email == nil || !h.email.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email service disabled"})
		return
	}
	userID := c.GetString("userID")

	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if user.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email not set"})
		return
	}

	remaining, err := h.email.SendVerificationCode(c.Request.Context(), userID, user.Email, user.Phone)
	if err != nil {
		log.Printf("EmailSendCode: %v", err)
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"attempts_left": remaining})
}

// EmailVerify — проверяет код и помечает email как подтверждённый.
func (h *AuthHandler) EmailVerify(c *gin.Context) {
	if h.email == nil || !h.email.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email service disabled"})
		return
	}
	userID := c.GetString("userID")
	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code required"})
		return
	}

	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	if err := h.email.VerifyCode(c.Request.Context(), userID, user.Phone, req.Code); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"verified": true})
}

// EmailStatus — возвращает статус подтверждения email.
func (h *AuthHandler) EmailStatus(c *gin.Context) {
	userID := c.GetString("userID")
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"email":    user.Email,
		"verified": user.EmailVerified,
	})
}

// EmailToggle2FA — включает/выключает способ входа через Email (protected).
func (h *AuthHandler) EmailToggle2FA(c *gin.Context) {
	userID := c.GetString("userID")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled required"})
		return
	}

	database.DB.Model(&model.User{}).Where("id = ?", userID).Update("email_2fa_enabled", req.Enabled)
	c.JSON(http.StatusOK, gin.H{"enabled": req.Enabled})
}

// EmailLoginSendCode — отправляет код входа на подтверждённую почту (public, для недоверенного устройства).
func (h *AuthHandler) EmailLoginSendCode(c *gin.Context) {
	if h.email == nil || !h.email.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email service disabled"})
		return
	}
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}

	// Пользователь выбрал вход по email — отменяем pending push-подтверждение
	h.cancelLoginRequestsForPhone(c, req.Phone)

	remaining, err := h.email.SendLoginCode(c.Request.Context(), req.Phone)
	if err != nil {
		log.Printf("EmailLoginSendCode: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"attempts_left": remaining})
}

// EmailLoginVerifyCode — проверяет код входа из письма, выдаёт токены и делает устройство доверенным.
func (h *AuthHandler) EmailLoginVerifyCode(c *gin.Context) {
	if h.email == nil || !h.email.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email service disabled"})
		return
	}
	var req struct {
		Phone      string `json:"phone" binding:"required"`
		Code       string `json:"code" binding:"required"`
		FcmToken   string `json:"fcm_token"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone and code required"})
		return
	}

	userID, err := h.email.VerifyLoginCode(c.Request.Context(), req.Phone, req.Code)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	tokenStr, _ := h.generateAccessToken(user)
	refreshToken, _ := h.createRefreshToken(user.ID)
	h.trustDevice(user.ID, req.DeviceID, req.DeviceName)
	h.notifyNewLogin(user.ID, req.FcmToken)

	// Успешный вход — чистим все pending login_request для этого телефона,
	// чтобы на доверенных устройствах не висел бесконечный диалог «Это вы?»
	keys, _ := database.RDB.Keys(c.Request.Context(), "login_request:*").Result()
	for _, key := range keys {
		raw, err := database.RDB.Get(c.Request.Context(), key).Result()
		if err != nil {
			continue
		}
		var p loginRequestPayload
		if json.Unmarshal([]byte(raw), &p) == nil && p.Phone == req.Phone {
			database.RDB.Del(c.Request.Context(), key)
		}
	}

	log.Printf("EMAIL VERIFY: phone=%s device=%s", req.Phone, req.DeviceID)
	c.JSON(http.StatusOK, gin.H{
		"verified":      true,
		"has_password":  user.HasPassword,
		"access_token":  tokenStr,
		"refresh_token": refreshToken,
		"user":          user,
	})
}
