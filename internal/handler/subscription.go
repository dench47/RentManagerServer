package handler

import (
	"net/http"

	"rentmanager-server/internal/service"
	"strings"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SubscriptionHandler — баланс и промокоды подписки.
// Пополнение пока демо (+30 ₽ по нажатию); реальная оплата через ЮKassa
// подключается позже на место TopUp.
type SubscriptionHandler struct {
	Charges *service.SubscriptionChargeService
}

func NewSubscriptionHandler(charges *service.SubscriptionChargeService) *SubscriptionHandler {
	return &SubscriptionHandler{Charges: charges}
}

// SeedDemoPromo — демо-промокод «ОСЕНЬ», если таблица пуста
func SeedDemoPromo() {
	// Демо-промокод из макетов (скидочный тариф 8 ₽ / объект / день);
	// список промокодов пополняется прямо в базе
	var count int64
	database.DB.Model(&model.PromoCode{}).Count(&count)
	if count == 0 {
		database.DB.Create(&model.PromoCode{Code: "ОСЕНЬ", Rate: 8.0, Note: "Демо-промокод из макета"})
	}
}

// State — GET /subscription: баланс, объекты, тариф и применённый промокод
func (h *SubscriptionHandler) State(c *gin.Context) {
	userID := c.GetString("userID")
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	const baseRate = 10.0
	rate := baseRate
	var promo *gin.H
	if user.AppliedPromoCode != "" {
		var pc model.PromoCode
		if err := database.DB.First(&pc, "code = ?", user.AppliedPromoCode).Error; err == nil {
			rate = pc.Rate
			promo = &gin.H{"code": pc.Code, "rate": pc.Rate}
		}
	}
	var objects int64
	database.DB.Model(&model.Property{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Count(&objects)
	c.JSON(http.StatusOK, gin.H{
		"balance":      user.Balance,
		"objects":      objects,
		"base_rate":    baseRate,
		"rate":         rate,
		"daily_charge": float64(objects) * rate,
		"promo":        promo,
		"blocked":      user.SubscriptionBlocked,
	})
}

// TopUp — POST /subscription/topup: демо-зачисление 30 ₽
func (h *SubscriptionHandler) TopUp(c *gin.Context) {
	userID := c.GetString("userID")
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	const amount = 30.0
	if err := database.DB.Model(&user).
		Update("balance", gorm.Expr("balance + ?", amount)).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// История операций: пополнение (источник подставит ЮKassa позже)
	database.DB.Create(&model.SubscriptionOperation{
		UserID:   userID,
		Type:     "topup",
		Title:    "Пополнение баланса",
		Subtitle: "Банковская карта •• 2545",
		Amount:   amount,
		Status:   "credited",
	})
	// Появились деньги при существующих объектах — запуск колеса списаний
	if h.Charges != nil {
		go h.Charges.ActivateIfDue(userID)
	}
	c.JSON(http.StatusOK, gin.H{
		"added":   amount,
		"balance": user.Balance + amount,
	})
}

// Operations — GET /subscription/operations: история баланса (новые сверху)
func (h *SubscriptionHandler) Operations(c *gin.Context) {
	userID := c.GetString("userID")
	var ops []model.SubscriptionOperation
	if err := database.DB.Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("created_at desc").Find(&ops).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ops)
}

// ApplyPromo — POST /subscription/promo {code}: проверить и применить
func (h *SubscriptionHandler) ApplyPromo(c *gin.Context) {
	userID := c.GetString("userID")
	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code is required"})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	var pc model.PromoCode
	if err := database.DB.First(&pc, "code = ?", code).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "promo_not_found"})
		return
	}
	var user model.User
	if err := database.DB.First(&user, "id = ?", userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err := database.DB.Model(&user).
		Update("applied_promo_code", pc.Code).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": pc.Code, "rate": pc.Rate})
}
