package handler

import (
	"net/http"
	"time"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type MeterHandler struct{}

func NewMeterHandler() *MeterHandler {
	return &MeterHandler{}
}

func (h *MeterHandler) ListByProperty(c *gin.Context) {
	propertyID := c.Param("id")
	var meters []model.Meter
	if err := database.DB.Where("property_id = ?", propertyID).Find(&meters).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, meters)
}

func (h *MeterHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	propertyID := c.Param("id")
	var meter model.Meter
	if err := c.ShouldBindJSON(&meter); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	meter.ID = uuid.New().String()
	meter.PropertyID = propertyID
	meter.UserID = userID
	today := time.Now().Format("2006-01-02")
	meter.LastUpdated = &today
	if err := database.DB.Create(&meter).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Начальное показание — первая запись истории
	if meter.Unit != "" {
		reading := model.MeterReading{
			BaseModel:  model.BaseModel{ID: uuid.New().String()},
			MeterID:    meter.ID,
			PropertyID: propertyID,
			UserID:     userID,
			Value:      meter.CurrentValue,
			Unit:       meter.Unit,
			Date:       today,
		}
		database.DB.Create(&reading)
	}
	c.JSON(http.StatusCreated, meter)
}

// Update — полное обновление по карте полей: очистка необязательных полей
// (пустые строки/false) сохраняется корректно, в отличие от Updates со структурой.
func (h *MeterHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("meterId")
	var existing model.Meter
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "meter not found"})
		return
	}
	if existing.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var input model.Meter
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]interface{}{
		"type":                   input.Type,
		"factory_number":         input.FactoryNumber,
		"next_verification_date": input.NextVerificationDate,
		"current_value":          input.CurrentValue,
		"unit":                   input.Unit,
		"submit_readings_by":     input.SubmitReadingsBy,
		"last_updated":           input.LastUpdated,
		"remind_verification":    input.RemindVerification,
		"remind_readings":        input.RemindReadings,
	}
	// Текущее показание меняется только через внесение показаний —
	// случайно затереть его правкой полей нельзя
	if input.CurrentValue != existing.CurrentValue {
		updates["current_value"] = existing.CurrentValue
	}
	if err := database.DB.Model(&existing).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	database.DB.First(&existing, "id = ?", id)
	c.JSON(http.StatusOK, existing)
}

// Delete — счётчик удаляется вместе с историей показаний (хард-делит).
func (h *MeterHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("meterId")
	var existing model.Meter
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "meter not found"})
		return
	}
	if existing.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	database.DB.Unscoped().Where("meter_id = ?", id).Delete(&model.MeterReading{})
	database.DB.Unscoped().Delete(&model.Meter{}, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// ListReadings — история показаний счётчика (свежие сверху).
func (h *MeterHandler) ListReadings(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("meterId")
	var meter model.Meter
	if err := database.DB.First(&meter, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "meter not found"})
		return
	}
	if meter.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var readings []model.MeterReading
	if err := database.DB.Where("meter_id = ?", id).Order("date desc, created_at desc").Find(&readings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, readings)
}

// CreateReading — внести показание: пишет запись в историю и обновляет
// текущее показание + дату последнего изменения счётчика.
func (h *MeterHandler) CreateReading(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("meterId")
	var meter model.Meter
	if err := database.DB.First(&meter, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "meter not found"})
		return
	}
	if meter.UserID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	var req struct {
		Value float64 `json:"value"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	today := time.Now().Format("2006-01-02")
	reading := model.MeterReading{
		BaseModel:  model.BaseModel{ID: uuid.New().String()},
		MeterID:    id,
		PropertyID: meter.PropertyID,
		UserID:     userID,
		Value:      req.Value,
		Unit:       meter.Unit,
		Date:       today,
	}
	if err := database.DB.Create(&reading).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	database.DB.Model(&meter).Updates(map[string]interface{}{
		"current_value": req.Value,
		"last_updated":  today,
	})
	meter.CurrentValue = req.Value
	meter.LastUpdated = &today
	c.JSON(http.StatusCreated, reading)
}
