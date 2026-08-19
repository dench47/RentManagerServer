package handler

import (
	"context"
	"log"
	"net/http"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"
	"rentmanager-server/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type PropertyHandler struct {
	s3 *service.S3Service
}

func NewPropertyHandler(s3 *service.S3Service) *PropertyHandler {
	return &PropertyHandler{s3: s3}
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

// ListForTenant — объекты, где текущий пользователь является арендатором
func (h *PropertyHandler) ListForTenant(c *gin.Context) {
	userID := c.GetString("userID")
	var tenantIDs []string
	database.DB.Model(&model.Tenant{}).Where("user_id = ?", userID).Pluck("id", &tenantIDs)

	properties := make([]model.Property, 0)
	if len(tenantIDs) > 0 {
		database.DB.Where("tenant_id IN ?", tenantIDs).Preload("Photos").Find(&properties)
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

	// Detach photos, create property, then create photos explicitly with generated IDs
	photos := property.Photos
	property.Photos = nil
	if err := database.DB.Create(&property).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for i := range photos {
		photos[i].ID = uuid.New().String()
		photos[i].PropertyID = property.ID
		if err := database.DB.Create(&photos[i]).Error; err != nil {
			log.Printf("WARNING: failed to create photo: %v", err)
		}
	}
	property.Photos = photos

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

// AddPhoto — добавляет фотографию к объекту (файл уже загружен в S3 через /upload).
func (h *PropertyHandler) AddPhoto(c *gin.Context) {
	propertyID := c.Param("id")

	var property model.Property
	if err := database.DB.First(&property, "id = ?", propertyID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "property not found"})
		return
	}

	var req struct {
		URL string `json:"url" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	photo := model.Photo{
		ID:         uuid.New().String(),
		PropertyID: propertyID,
		URL:        req.URL,
	}
	if err := database.DB.Create(&photo).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, photo)
}

// DeletePhoto — удаляет фотографию из БД и из S3.
func (h *PropertyHandler) DeletePhoto(c *gin.Context) {
	photoID := c.Param("photoId")

	var photo model.Photo
	if err := database.DB.First(&photo, "id = ?", photoID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "photo not found"})
		return
	}

	// Удаляем файл из S3, если URL указывает на S3
	if h.s3 != nil {
		key := h.s3.ExtractKey(photo.URL)
		if key != "" {
			if err := h.s3.Delete(context.Background(), key); err != nil {
				log.Printf("WARNING: failed to delete photo from S3: %v", err)
			}
		}
	}

	if err := database.DB.Delete(&model.Photo{}, "id = ?", photoID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}
