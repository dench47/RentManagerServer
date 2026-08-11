package handler

import (
	"net/http"
	"path/filepath"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"
	"rentmanager-server/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------------- Tenant ----------------

type TenantHandler struct{}

func NewTenantHandler() *TenantHandler { return &TenantHandler{} }

func (h *TenantHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var tenants []model.Tenant
	database.DB.Where("owner_id = ?", userID).Find(&tenants)
	c.JSON(http.StatusOK, tenants)
}

func (h *TenantHandler) Get(c *gin.Context) {
	id := c.Param("id")
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	c.JSON(http.StatusOK, tenant)
}

func (h *TenantHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var tenant model.Tenant
	if err := c.ShouldBindJSON(&tenant); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tenant.BaseModel.ID = uuid.New().String()
	tenant.OwnerID = userID
	database.DB.Create(&tenant)
	c.JSON(http.StatusCreated, tenant)
}

func (h *TenantHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	database.DB.Delete(&model.Tenant{}, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// AttachTenant — прикрепить арендатора к объекту
func (h *TenantHandler) AttachToProperty(c *gin.Context) {
	propertyID := c.Param("id")
	var req struct {
		TenantID string `json:"tenant_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	database.DB.Model(&model.Property{}).Where("id = ?", propertyID).
		Updates(map[string]interface{}{"tenant_id": req.TenantID, "status": "occupied"})

	// Auto-set is_tenant when tenant is attached to a property
	database.DB.Model(&model.User{}).Where("id = ?", req.TenantID).Update("is_tenant", true)

	c.JSON(http.StatusOK, gin.H{"message": "tenant attached"})
}

// ---------------- Payment ----------------

type PaymentHandler struct{}

func NewPaymentHandler() *PaymentHandler { return &PaymentHandler{} }

func (h *PaymentHandler) ListSchedules(c *gin.Context) {
	userID := c.GetString("userID")
	var schedules []model.PaymentSchedule
	database.DB.Where("user_id = ?", userID).Find(&schedules)
	c.JSON(http.StatusOK, schedules)
}

func (h *PaymentHandler) CreateSchedule(c *gin.Context) {
	userID := c.GetString("userID")
	var schedule model.PaymentSchedule
	if err := c.ShouldBindJSON(&schedule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	schedule.BaseModel.ID = uuid.New().String()
	schedule.UserID = userID
	database.DB.Create(&schedule)
	c.JSON(http.StatusCreated, schedule)
}

// ---------------- Finance ----------------

type FinanceHandler struct{}

func NewFinanceHandler() *FinanceHandler { return &FinanceHandler{} }

func (h *FinanceHandler) Report(c *gin.Context) {
	userID := c.GetString("userID")
	propertyID := c.Query("property_id")
	from := c.Query("from")
	to := c.Query("to")

	query := database.DB.Where("user_id = ?", userID)
	if propertyID != "" {
		query = query.Where("property_id = ?", propertyID)
	}
	if from != "" && to != "" {
		query = query.Where("date BETWEEN ? AND ?", from, to)
	}

	var payments []model.Payment
	query.Find(&payments)

	var income float64
	var expense float64
	for _, p := range payments {
		if p.Type == "income" {
			income += p.Amount
		} else {
			expense += p.Amount
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"payments":      payments,
		"total_income":  income,
		"total_expense": expense,
		"balance":       income - expense,
	})
}

// ---------------- Chat ----------------

type ChatHandler struct{}

func NewChatHandler() *ChatHandler { return &ChatHandler{} }

func (h *ChatHandler) ListChats(c *gin.Context) {
	userID := c.GetString("userID")
	var chats []model.Chat
	database.DB.Where("participant_ids LIKE ?", "%"+userID+"%").Find(&chats)

	// Map to DTO
	var result []gin.H
	for _, chat := range chats {
		result = append(result, gin.H{
			"id":           chat.ID,
			"last_message": nil,
			"unread_count": 0,
		})
	}
	c.JSON(http.StatusOK, result)
}

func (h *ChatHandler) GetMessages(c *gin.Context) {
	chatID := c.Param("chatId")
	var messages []model.Message
	database.DB.Where("chat_id = ?", chatID).Order("created_at asc").Find(&messages)
	c.JSON(http.StatusOK, messages)
}

func (h *ChatHandler) SendMessage(c *gin.Context) {
	chatID := c.Param("chatId")
	userID := c.GetString("userID")
	var req struct {
		Text string `json:"text" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	msg := model.Message{
		ID:        uuid.New().String(),
		ChatID:    chatID,
		SenderID:  userID,
		Text:      req.Text,
		CreatedAt: 0,
	}
	database.DB.Create(&msg)
	c.JSON(http.StatusCreated, msg)
}

// ---------------- Upload ----------------

type UploadHandler struct {
	UploadDir string
	s3        *service.S3Service
}

func NewUploadHandler(uploadDir string, s3 *service.S3Service) *UploadHandler {
	return &UploadHandler{UploadDir: uploadDir, s3: s3}
}

var allowedFolders = map[string]bool{"avatars": true, "photos": true, "documents": true}

func (h *UploadHandler) Upload(c *gin.Context) {
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
		return
	}
	filename := uuid.New().String() + filepath.Ext(file.Filename)

	// Determine folder
	folder := c.DefaultQuery("folder", "avatars")
	if !allowedFolders[folder] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid folder, allowed: avatars, photos, documents"})
		return
	}

	// Try S3 first
	if h.s3 != nil {
		src, err := file.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		buf := make([]byte, file.Size)
		_, err = src.Read(buf)
		src.Close()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		key := service.BuildKey(folder, filename)
		url, err := h.s3.Upload(c, key, buf)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "s3 upload failed: " + err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"url": url})
		return
	}

	// Fallback to local disk
	path := filepath.Join(h.UploadDir, filename)
	if err := c.SaveUploadedFile(file, path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	fullURL := "http://" + c.Request.Host + "/uploads/" + filename
	c.JSON(http.StatusOK, gin.H{"url": fullURL})
}
