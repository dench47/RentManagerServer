package handler

import (
	"net/http"

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
	if err := database.DB.Create(&meter).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, meter)
}

func (h *MeterHandler) Update(c *gin.Context) {
	id := c.Param("meterId")
	var existing model.Meter
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "meter not found"})
		return
	}
	var input model.Meter
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	database.DB.Model(&existing).Updates(input)
	c.JSON(http.StatusOK, existing)
}

func (h *MeterHandler) Delete(c *gin.Context) {
	id := c.Param("meterId")
	if err := database.DB.Delete(&model.Meter{}, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}