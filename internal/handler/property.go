package handler

import (
	"net/http"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type PropertyHandler struct{}

func NewPropertyHandler() *PropertyHandler {
	return &PropertyHandler{}
}

func (h *PropertyHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var properties []model.Property
	if err := database.DB.Where("user_id = ?", userID).Preload("Photos").Find(&properties).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, properties)
}

func (h *PropertyHandler) Get(c *gin.Context) {
	id := c.Param("id")
	var property model.Property
	if err := database.DB.Preload("Photos").First(&property, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "property not found"})
		return
	}
	c.JSON(http.StatusOK, property)
}

func (h *PropertyHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var property model.Property
	if err := c.ShouldBindJSON(&property); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	property.ID = uuid.New().String()
	property.UserID = userID
	property.Status = "free"

	if err := database.DB.Create(&property).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Auto-set is_landlord when first property created
	database.DB.Model(&model.User{}).Where("id = ?", userID).Update("is_landlord", true)

	c.JSON(http.StatusCreated, property)
}

func (h *PropertyHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var existing model.Property
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "property not found"})
		return
	}
	var input model.Property
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	database.DB.Model(&existing).Updates(input)
	c.JSON(http.StatusOK, existing)
}

func (h *PropertyHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	if err := database.DB.Delete(&model.Property{}, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}
