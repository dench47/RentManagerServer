package handler

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"
	"rentmanager-server/internal/service"

	"github.com/gin-gonic/gin"
)

// TelegramWebhook — точка входа для апдейтов Telegram Bot API.
func (h *AuthHandler) TelegramWebhook(c *gin.Context) {
	if h.telegram == nil || !h.telegram.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "telegram bot disabled"})
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
		return
	}
	update, err := service.ParseUpdate(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid update"})
		return
	}
	go h.telegram.HandleUpdate(context.Background(), update)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// TelegramLink — создаёт одноразовый токен привязки, возвращает ссылку на бота.
func (h *AuthHandler) TelegramLink(c *gin.Context) {
	if h.telegram == nil || !h.telegram.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "telegram bot disabled"})
		return
	}
	userID := c.GetString("userID")

	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	token, err := h.telegram.GenerateLinkToken(c.Request.Context(), user.Phone)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate link"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":   token,
		"bot_url": h.telegram.BotLink(token),
	})
}

// TelegramStatus — возвращает статус привязки.
func (h *AuthHandler) TelegramStatus(c *gin.Context) {
	if h.telegram == nil || !h.telegram.IsEnabled() {
		c.JSON(http.StatusOK, gin.H{"linked": false})
		return
	}
	userID := c.GetString("userID")
	binding := h.telegram.GetBinding(userID)
	if binding == nil {
		c.JSON(http.StatusOK, gin.H{"linked": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"linked":     true,
		"username":   binding.Username,
		"first_name": binding.FirstName,
		"chat_id":    binding.ChatID,
	})
}

// TelegramUnlink — отвязывает Telegram.
func (h *AuthHandler) TelegramUnlink(c *gin.Context) {
	if h.telegram == nil || !h.telegram.IsEnabled() {
		c.JSON(http.StatusOK, gin.H{"message": "unlinked"})
		return
	}
	userID := c.GetString("userID")
	if err := h.telegram.Unlink(userID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no binding"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unlinked"})
}

// TelegramSendCode — генерирует и отправляет код входа через Telegram.
func (h *AuthHandler) TelegramSendCode(c *gin.Context) {
	if h.telegram == nil || !h.telegram.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "telegram bot disabled"})
		return
	}
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}

	// Пользователь выбрал вход по Telegram — отменяем pending push-подтверждение
	h.cancelLoginRequestsForPhone(c, req.Phone)

	var user model.User
	if err := database.DB.Where("phone = ?", req.Phone).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	remaining, err := h.telegram.SendLoginCode(c.Request.Context(), user.ID, req.Phone)
	if err != nil {
		log.Printf("TelegramSendCode: %v", err)
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"attempts_left": remaining})
}

// TelegramVerifyCode — проверяет код из Telegram, выдаёт токены и делает устройство доверенным.
func (h *AuthHandler) TelegramVerifyCode(c *gin.Context) {
	if h.telegram == nil || !h.telegram.IsEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "telegram bot disabled"})
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

	userID, err := h.telegram.VerifyLoginCode(c.Request.Context(), req.Phone, req.Code)
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

	// Успешный вход через Telegram — чистим все pending login_request для этого телефона,
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

	log.Printf("TELEGRAM VERIFY: phone=%s device=%s", req.Phone, req.DeviceID)
	c.JSON(http.StatusOK, gin.H{
		"verified":      true,
		"has_password":  user.HasPassword,
		"access_token":  tokenStr,
		"refresh_token": refreshToken,
		"user":          user,
	})
}
