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
	s3      *service.S3Service
	Charges *service.SubscriptionChargeService
}

func NewPropertyHandler(s3 *service.S3Service, charges *service.SubscriptionChargeService) *PropertyHandler {
	return &PropertyHandler{s3: s3, Charges: charges}
}

func (h *PropertyHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var properties []model.Property
	if err := database.DB.Where("user_id = ?", userID).Preload("Photos").Find(&properties).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resolveTenantDisplay(properties)
	c.JSON(http.StatusOK, properties)
}

// resolveTenantDisplay — карточка объекта показывает арендатора по ЖИВОЙ связи,
// а не по снимку tenant_info в строке объекта: при наличии tenant_id имя/телефон
// берём из записи арендатора; висячая ссылка (арендатор удалён в обход хендлера)
// самолечится — объект освобождается.
func resolveTenantDisplay(properties []model.Property) {
	ids := make([]string, 0, len(properties))
	for i := range properties {
		if properties[i].TenantID != nil && *properties[i].TenantID != "" {
			ids = append(ids, *properties[i].TenantID)
		}
	}
	if len(ids) == 0 {
		return
	}
	var tenants []model.Tenant
	database.DB.Where("id IN ?", ids).Find(&tenants) // soft-delete учитывается: удалённые не находятся
	byID := make(map[string]model.Tenant, len(tenants))
	for _, t := range tenants {
		byID[t.ID] = t
	}
	for i := range properties {
		if properties[i].TenantID == nil {
			continue
		}
		t, ok := byID[*properties[i].TenantID]
		if !ok {
			// Висячая ссылка — чиним и объект, и ответ
			database.DB.Model(&model.Property{}).Where("id = ?", properties[i].ID).
				Updates(map[string]interface{}{"tenant_id": nil, "tenant_info": nil, "phone": nil, "status": "free"})
			properties[i].TenantID = nil
			properties[i].TenantInfo = nil
			properties[i].Phone = nil
			properties[i].Status = "free"
			continue
		}
		name := t.FullName
		phone := t.Phone
		properties[i].TenantInfo = &name
		properties[i].Phone = &phone
	}
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
	props := []model.Property{property}
	resolveTenantDisplay(props)
	c.JSON(http.StatusOK, props[0])
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

	// Первый объект при уже пополненном балансе — запуск колеса списаний
	if h.Charges != nil {
		go h.Charges.ActivateIfDue(userID)
	}

	// График платежей создаётся сразу по типу аренды из шага 2 создания:
	// посуточно → переменный (manual), длительно → постоянный (auto).
	// Дата расчёта/сумма/реквизиты пользователь задаёт потом в самом графике.
	scheduleType := "auto"
	if property.RentType != nil && *property.RentType == "посуточно" {
		scheduleType = "manual"
	}
	schedule := model.PaymentSchedule{
		BaseModel:  model.BaseModel{ID: uuid.New().String()},
		PropertyID: property.ID,
		UserID:     userID,
		Type:       scheduleType,
	}
	if err := database.DB.Create(&schedule).Error; err != nil {
		log.Printf("WARNING: failed to create payment schedule: %v", err)
	}

	c.JSON(http.StatusCreated, property)
}

func (h *PropertyHandler) Update(c *gin.Context) {
	id := c.Param("id")
	var existing model.Property
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "property not found"})
		return
	}
	// Частичное обновление: собираем map только из переданных в JSON полей —
	// так очистка необязательных полей (null/пустые строки) корректно сохраняется
	// (Updates со структурой игнорирует zero-значения).
	var payload map[string]interface{}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	allowed := []string{
		"name", "address", "area", "type", "rent_type", "rooms", "sleeping_places",
		"floor", "floors_in_house", "description", "tenant_info", "service_info",
		"phone", "wifi_password", "house_rules", "status", "rent_amount",
		"rent_end_date", "contract_number", "contract_date", "tenant_id",
		"latitude", "longitude", "is_published",
	}
	updates := map[string]interface{}{}
	for _, key := range allowed {
		if v, ok := payload[key]; ok {
			updates[key] = v
		}
	}
	if len(updates) > 0 {
		if err := database.DB.Model(&existing).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	// Перечитываем, чтобы вернуть актуальное состояние объекта вместе с фото
	if err := database.DB.Preload("Photos").First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, existing)
}

// Publish — публикация объявления объекта.
func (h *PropertyHandler) Publish(c *gin.Context) {
	h.setPublished(c, true)
}

// Unpublish — снятие объявления объекта с публикации.
func (h *PropertyHandler) Unpublish(c *gin.Context) {
	h.setPublished(c, false)
}

func (h *PropertyHandler) setPublished(c *gin.Context, published bool) {
	id := c.Param("id")
	var property model.Property
	if err := database.DB.Preload("Photos").First(&property, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "property not found"})
		return
	}
	// Update по конкретной колонке: Updates со структурой не записал бы false (zero-value)
	if err := database.DB.Model(&property).Update("is_published", published).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	property.IsPublished = published
	c.JSON(http.StatusOK, property)
}

func (h *PropertyHandler) Delete(c *gin.Context) {
	id := c.Param("id")

	var property model.Property
	if err := database.DB.First(&property, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "property not found"})
		return
	}

	// Полное удаление после подтверждения: файлы из S3 + ВСЕ связанные данные
	// из БД физически (Unscoped — без soft-delete, строки не остаются).
	var photos []model.Photo
	database.DB.Where("property_id = ?", id).Find(&photos)
	if h.s3 != nil {
		for _, ph := range photos {
			key := h.s3.ExtractKey(ph.URL)
			if key != "" {
				if err := h.s3.Delete(context.Background(), key); err != nil {
					log.Printf("WARNING: failed to delete photo from S3: %v", err)
				}
			}
		}
	}

	// Чаты объекта: сначала сообщения, потом сами чаты
	var chatIDs []string
	database.DB.Model(&model.Chat{}).Where("property_id = ?", id).Pluck("id", &chatIDs)
	if len(chatIDs) > 0 {
		database.DB.Unscoped().Where("chat_id IN ?", chatIDs).Delete(&model.Message{})
		database.DB.Unscoped().Where("id IN ?", chatIDs).Delete(&model.Chat{})
	}

	database.DB.Unscoped().Where("property_id = ?", id).Delete(&model.Photo{})
	database.DB.Unscoped().Where("property_id = ?", id).Delete(&model.Meter{})
	database.DB.Unscoped().Where("property_id = ?", id).Delete(&model.Payment{})
	database.DB.Unscoped().Where("property_id = ?", id).Delete(&model.PaymentSchedule{})
	database.DB.Unscoped().Where("property_id = ?", id).Delete(&model.Booking{})
	database.DB.Unscoped().Delete(&model.Property{}, "id = ?", id)

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
