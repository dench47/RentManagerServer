package handler

import (
	"net/http"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type RequisiteHandler struct{}

func NewRequisiteHandler() *RequisiteHandler {
	return &RequisiteHandler{}
}

// List — реквизиты текущего пользователя (для шита выбора в графике платежей).
func (h *RequisiteHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var requisites []model.PaymentRequisite
	if err := database.DB.Where("user_id = ?", userID).Order("created_at asc").Find(&requisites).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, requisites)
}

func (h *RequisiteHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var requisite model.PaymentRequisite
	if err := c.ShouldBindJSON(&requisite); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	requisite.ID = uuid.New().String()
	requisite.UserID = userID
	if err := database.DB.Create(&requisite).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, requisite)
}

// Update — обновление по карте полей: очистка необязательных полей
// (пустые строки) сохраняется корректно.
func (h *RequisiteHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("requisiteId")
	var existing model.PaymentRequisite
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "requisite not found"})
		return
	}
	if existing.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var input model.PaymentRequisite
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]interface{}{
		"name":    input.Name,
		"account": input.Account,
		"bank":    input.Bank,
	}
	if err := database.DB.Model(&existing).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	database.DB.First(&existing, "id = ?", id)
	c.JSON(http.StatusOK, existing)
}

func (h *RequisiteHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("requisiteId")
	var existing model.PaymentRequisite
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "requisite not found"})
		return
	}
	if existing.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	database.DB.Unscoped().Delete(&model.PaymentRequisite{}, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}
