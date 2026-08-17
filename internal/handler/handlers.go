package handler

import (
	"net/http"
	"path/filepath"
	"time"

	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"
	"rentmanager-server/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------------- Tenant ----------------

type TenantHandler struct {
	fcm *service.FCMService
}

func NewTenantHandler(fcm *service.FCMService) *TenantHandler { return &TenantHandler{fcm: fcm} }

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

	// Загружаем арендатора — нужен user_id для уведомления и сброса флага
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}

	// Снимаем арендатора со всех объектов, к которым он был привязан
	var properties []model.Property
	database.DB.Where("tenant_id = ?", id).Find(&properties)
	for _, p := range properties {
		database.DB.Model(&model.Property{}).Where("id = ?", p.ID).
			Updates(map[string]interface{}{"tenant_id": nil, "status": "free"})
	}

	// Уведомляем пользователя, если арендатор привязан к аккаунту приложения
	if tenant.UserID != nil && h.fcm != nil {
		body := "Арендодатель удалил вас из объекта"
		if len(properties) == 1 {
			body = "Арендодатель удалил вас из объекта «" + properties[0].Name + "»"
		}
		go h.fcm.SendToUser(*tenant.UserID, map[string]string{
			"type":  "tenant_detached",
			"title": "Доступ к объекту отозван",
			"body":  body,
		}, "")
	}

	// Сбрасываем is_tenant, если у пользователя не осталось активных записей арендатора
	if tenant.UserID != nil {
		var count int64
		database.DB.Model(&model.Tenant{}).
			Where("user_id = ? AND id <> ?", *tenant.UserID, id).
			Count(&count)
		if count == 0 {
			database.DB.Model(&model.User{}).Where("id = ?", *tenant.UserID).Update("is_tenant", false)
		}
	}

	database.DB.Delete(&model.Tenant{}, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// AttachTenant — прикрепить арендатора к объекту.
// Принимает tenant_id (ручная запись) либо user_id (пользователь приложения).
func (h *TenantHandler) AttachToProperty(c *gin.Context) {
	propertyID := c.Param("id")
	var req struct {
		TenantID string `json:"tenant_id"`
		UserID   string `json:"user_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	tenantID := req.TenantID

	// Если передан user_id — находим/создаём запись арендатора по пользователю
	if req.UserID != "" {
		var user model.User
		if err := database.DB.First(&user, "id = ?", req.UserID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}

		// Ищем запись арендатора, включая ранее удалённую (soft delete), чтобы не плодить дубли
		var tenant model.Tenant
		if err := database.DB.Unscoped().Where("user_id = ?", req.UserID).First(&tenant).Error; err == nil {
			// Восстанавливаем запись, если она была удалена ранее
			if tenant.DeletedAt.Valid {
				database.DB.Unscoped().Model(&model.Tenant{}).Where("id = ?", tenant.ID).
					Update("deleted_at", nil)
			}
			tenantID = tenant.ID
		} else {
			newTenant := model.Tenant{
				BaseModel: model.BaseModel{ID: uuid.New().String()},
				OwnerID:   c.GetString("userID"),
				UserID:    &req.UserID,
				FullName:  user.Name,
				Phone:     user.Phone,
				Active:    true,
			}
			if err := database.DB.Create(&newTenant).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			tenantID = newTenant.ID
		}

		// Помечаем пользователя как арендатора
		database.DB.Model(&model.User{}).Where("id = ?", req.UserID).Update("is_tenant", true)
	}

	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tenant_id or user_id required"})
		return
	}

	database.DB.Model(&model.Property{}).Where("id = ?", propertyID).
		Updates(map[string]interface{}{"tenant_id": tenantID, "status": "occupied"})

	// Push-уведомление арендатору о предоставлении доступа
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", tenantID).Error; err == nil && tenant.UserID != nil && h.fcm != nil {
		var property model.Property
		name := "объект"
		if err := database.DB.First(&property, "id = ?", propertyID).Error; err == nil && property.Name != "" {
			name = property.Name
		}
		go h.fcm.SendToUser(*tenant.UserID, map[string]string{
			"type":  "tenant_attached",
			"title": "Вам предоставлен доступ к объекту",
			"body":  "Арендодатель добавил вас в объект «" + name + "»",
		}, "")
	}

	c.JSON(http.StatusOK, gin.H{"message": "tenant attached", "tenant_id": tenantID})
}

// ---------------- User search ----------------

type UserHandler struct{}

func NewUserHandler() *UserHandler { return &UserHandler{} }

// UserPublic — публичное представление пользователя (поиск, список арендодателей)
type UserPublic struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Phone      string `json:"phone"`
	AvatarURL  string `json:"avatar_url"`
	IsLandlord bool   `json:"is_landlord"`
}

// Search — поиск пользователей по номеру телефона
func (h *UserHandler) Search(c *gin.Context) {
	phone := c.Query("phone")
	var users []model.User
	query := database.DB.Model(&model.User{})
	if phone != "" {
		query = query.Where("phone LIKE ?", "%"+normalizePhoneForSearch(phone)+"%")
	}
	query.Limit(50).Find(&users)

	result := make([]UserPublic, 0, len(users))
	for _, u := range users {
		result = append(result, UserPublic{
			ID:         u.ID,
			Name:       u.Name,
			Phone:      u.Phone,
			AvatarURL:  u.AvatarURL,
			IsLandlord: u.IsLandlord,
		})
	}
	c.JSON(http.StatusOK, result)
}

// ListLandlordsForTenant — арендодатели (владельцы объектов), у которых арендует текущий пользователь
func (h *UserHandler) ListLandlordsForTenant(c *gin.Context) {
	userID := c.GetString("userID")
	var landlordIDs []string
	database.DB.Model(&model.Property{}).
		Joins("JOIN tenants ON tenants.id = properties.tenant_id").
		Where("tenants.user_id = ?", userID).
		Distinct().
		Pluck("properties.user_id", &landlordIDs)

	landlords := make([]UserPublic, 0)
	if len(landlordIDs) > 0 {
		var users []model.User
		database.DB.Where("id IN ?", landlordIDs).Find(&users)
		for _, u := range users {
			landlords = append(landlords, UserPublic{
				ID:         u.ID,
				Name:       u.Name,
				Phone:      u.Phone,
				AvatarURL:  u.AvatarURL,
				IsLandlord: true,
			})
		}
	}
	c.JSON(http.StatusOK, landlords)
}

// normalizePhoneForSearch — приводит номер к национальным цифрам, чтобы поиск
// находил пользователя в любом формате написания:
// 89009999999 / +79009999999 / 9009999999 → 9009999999; +8613800138000 → 13800138000.
func normalizePhoneForSearch(raw string) string {
	digits := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] >= '0' && raw[i] <= '9' {
			digits = append(digits, raw[i])
		}
	}
	d := string(digits)
	switch {
	case len(d) == 11 && (d[0] == '7' || d[0] == '8'):
		return d[1:]
	case len(d) == 10 && d[0] == '9':
		return d
	case len(d) == 13 && d[:2] == "86":
		return d[2:]
	case len(d) == 11 && d[0] == '1':
		return d
	default:
		return d
	}
}

// ---------------- Booking ----------------

type BookingHandler struct{}

func NewBookingHandler() *BookingHandler { return &BookingHandler{} }

func (h *BookingHandler) List(c *gin.Context) {
	propertyID := c.Param("id")
	var bookings []model.Booking
	database.DB.Where("property_id = ?", propertyID).Order("start_date asc").Find(&bookings)
	c.JSON(http.StatusOK, bookings)
}

func (h *BookingHandler) Create(c *gin.Context) {
	propertyID := c.Param("id")
	userID := c.GetString("userID")
	var booking model.Booking
	if err := c.ShouldBindJSON(&booking); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	booking.ID = uuid.New().String()
	booking.PropertyID = propertyID
	booking.CreatedBy = userID
	if booking.Source == "" {
		booking.Source = "manual"
	}
	if err := database.DB.Create(&booking).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, booking)
}

func (h *BookingHandler) Update(c *gin.Context) {
	id := c.Param("bookingId")
	var existing model.Booking
	if err := database.DB.First(&existing, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "booking not found"})
		return
	}
	var input model.Booking
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	database.DB.Model(&existing).Updates(map[string]interface{}{
		"start_date": input.StartDate,
		"end_date":   input.EndDate,
		"source":     input.Source,
		"tenant_id":  input.TenantID,
	})
	c.JSON(http.StatusOK, existing)
}

func (h *BookingHandler) Delete(c *gin.Context) {
	id := c.Param("bookingId")
	database.DB.Delete(&model.Booking{}, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
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

// ListSchedulesForTenant — графики платежей объектов, которые арендует текущий пользователь
func (h *PaymentHandler) ListSchedulesForTenant(c *gin.Context) {
	userID := c.GetString("userID")
	var tenantIDs []string
	database.DB.Model(&model.Tenant{}).Where("user_id = ?", userID).Pluck("id", &tenantIDs)

	schedules := make([]model.PaymentSchedule, 0)
	if len(tenantIDs) > 0 {
		var propertyIDs []string
		database.DB.Model(&model.Property{}).Where("tenant_id IN ?", tenantIDs).Pluck("id", &propertyIDs)
		if len(propertyIDs) > 0 {
			database.DB.Where("property_id IN ?", propertyIDs).Find(&schedules)
		}
	}
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

// CreatePayment — записывает платёж (имитация оплаты арендатором)
func (h *PaymentHandler) CreatePayment(c *gin.Context) {
	propertyID := c.Param("id")
	userID := c.GetString("userID")
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	payment := model.Payment{
		BaseModel:  model.BaseModel{ID: uuid.New().String()},
		PropertyID: propertyID,
		UserID:     userID,
		Amount:     req.Amount,
		Date:       time.Now().Format("2006-01-02"),
		Status:     "paid",
		Type:       "income",
	}
	if err := database.DB.Create(&payment).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, payment)
}

// ListPayments — платежи по объекту
func (h *PaymentHandler) ListPayments(c *gin.Context) {
	propertyID := c.Param("id")
	var payments []model.Payment
	database.DB.Where("property_id = ?", propertyID).Order("date desc").Find(&payments)
	c.JSON(http.StatusOK, payments)
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
