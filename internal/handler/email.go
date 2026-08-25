package handler

import (
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
