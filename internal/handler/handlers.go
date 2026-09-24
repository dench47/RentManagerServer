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
	// s3 — чистим файлы документов при их удалении (folder=documents)
	s3 *service.S3Service
}

func NewTenantHandler(fcm *service.FCMService, s3 *service.S3Service) *TenantHandler {
	return &TenantHandler{fcm: fcm, s3: s3}
}

func (h *TenantHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	var tenants []model.Tenant
	database.DB.Where("owner_id = ?", userID).Find(&tenants)
	BindTenantsByPhone(&tenants)
	attachTenantAvatars(&tenants)
	c.JSON(http.StatusOK, tenants)
}

func (h *TenantHandler) Get(c *gin.Context) {
	id := c.Param("id")
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	one := []model.Tenant{tenant}
	BindTenantsByPhone(&one)
	attachTenantAvatars(&one)
	c.JSON(http.StatusOK, one[0])
}

// BindTenantsByPhone — дозавязка записей, созданных ДО регистрации юзера:
// карточку создали 1-го, человек зарегистрировался 5-го — при первом же
// чтении телефон матчится с аккаунтом и user_id проставляется задним числом.
func BindTenantsByPhone(tenants *[]model.Tenant) {
	for i := range *tenants {
		t := &(*tenants)[i]
		if t.UserID != nil {
			continue
		}
		uid := findUserByPhone(t.Phone)
		if uid == "" {
			continue
		}
		database.DB.Model(&model.Tenant{}).Where("id = ?", t.ID).Update("user_id", uid)
		t.UserID = &uid
		database.DB.Model(&model.User{}).Where("id = ?", uid).Update("is_tenant", true)
	}
}

// BindUserTenantRecords — зеркальный случай: юзер зарегистрировался (или сменил
// номер), а карточки арендатора с его телефоном созданы раньше и висят без user_id.
// Вызывается ТОЛЬКО в двух событиях: регистрация (CallCheckStatus) и смена номера
// (ConfirmPhoneChange) — карточки, созданные при уже зарегистрированном юзере,
// биндятся в момент создания (Create), поэтому вечных проверок не нужно.
func BindUserTenantRecords(user model.User) {
	d := normalizePhoneDigits(user.Phone)
	if len(d) != 11 {
		return
	}
	var candidates []model.Tenant
	database.DB.Where("user_id IS NULL AND phone LIKE ?", "%"+d[1:]).Limit(50).Find(&candidates)
	bound := 0
	for i := range candidates {
		if normalizePhoneDigits(candidates[i].Phone) != d {
			continue
		}
		database.DB.Model(&model.Tenant{}).Where("id = ?", candidates[i].ID).Update("user_id", user.ID)
		bound++
	}
	if bound > 0 {
		database.DB.Model(&model.User{}).Where("id = ?", user.ID).Update("is_tenant", true)
	}
}

// attachTenantAvatars — данные арендатора живьём с его аккаунта (по user_id):
// аватарка, почта и название компании (= «название юридического лица» из
// настроек юзера). Юзер сменил данные — карточка у владельца обновится.
func attachTenantAvatars(tenants *[]model.Tenant) {
	ids := make([]string, 0, len(*tenants))
	for i := range *tenants {
		if (*tenants)[i].UserID != nil {
			ids = append(ids, *(*tenants)[i].UserID)
		}
	}
	if len(ids) == 0 {
		return
	}
	var users []model.User
	database.DB.Select("id", "avatar_url", "email", "legal_name").Where("id IN ?", ids).Find(&users)
	byID := make(map[string]model.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	for i := range *tenants {
		if (*tenants)[i].UserID == nil {
			continue
		}
		u, ok := byID[*(*tenants)[i].UserID]
		if !ok {
			continue
		}
		if u.AvatarURL != "" {
			av := u.AvatarURL
			(*tenants)[i].AvatarURL = &av
		}
		if u.Email != "" {
			e := u.Email
			(*tenants)[i].Email = &e
		}
		if u.LegalName != "" {
			ln := u.LegalName
			(*tenants)[i].CompanyName = &ln
		}
	}
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
	// Связка с аккаунтом: телефон арендатора совпадает с зарегистрированным
	// пользователем → биндим user_id (иначе запись навсегда «безликая» карточка)
	if tenant.UserID == nil {
		if uid := findUserByPhone(tenant.Phone); uid != "" {
			tenant.UserID = &uid
			database.DB.Model(&model.User{}).Where("id = ?", uid).Update("is_tenant", true)
		}
	}
	database.DB.Create(&tenant)
	c.JSON(http.StatusCreated, tenant)
}

// normalizePhoneDigits: только цифры, 8… → 7… — единый вид для сравнения
func normalizePhoneDigits(p string) string {
	digits := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] >= '0' && p[i] <= '9' {
			digits = append(digits, p[i])
		}
	}
	if len(digits) == 11 && digits[0] == '8' {
		digits[0] = '7'
	}
	return string(digits)
}

// findUserByPhone ищет зарегистрированного пользователя по телефону
// (нормализуем оба номера: +7/8/пробелы не должны мешать совпадению)
func findUserByPhone(phone string) string {
	d := normalizePhoneDigits(phone)
	if len(d) != 11 {
		return ""
	}
	var users []model.User
	database.DB.Where("phone LIKE ?", "%"+d[1:]).Limit(5).Find(&users)
	for _, u := range users {
		if normalizePhoneDigits(u.Phone) == d {
			return u.ID
		}
	}
	return ""
}

func (h *TenantHandler) Delete(c *gin.Context) {
	id := c.Param("id")

	// Загружаем арендатора — нужен user_id для уведомления и сброса флага
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}

	// Диалог «Нельзя удалить карточку» (Figma 3005:53081): при активной
	// аренде удаление запрещено — сначала завершите аренду
	var occupied []model.Property
	database.DB.Where("tenant_id = ?", id).Find(&occupied)
	if len(occupied) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "has_active_rent"})
		return
	}

	// Сбрасываем is_tenant, если у пользователя не осталось записей арендатора
	if tenant.UserID != nil {
		var count int64
		database.DB.Model(&model.Tenant{}).
			Where("user_id = ? AND id <> ?", *tenant.UserID, id).
			Count(&count)
		if count == 0 {
			database.DB.Model(&model.User{}).Where("id = ?", *tenant.UserID).Update("is_tenant", false)
		}
		if h.fcm != nil {
			go h.fcm.SendToUser(*tenant.UserID, map[string]string{
				"type":  "tenant_detached",
				"title": "Доступ к объекту отозван",
				"body":  "Арендодатель удалил вашу карточку арендатора",
			}, "")
		}
	}

	// Документы карточки: чистим записи и сами файлы в хранилище
	var docs []model.TenantDocument
	database.DB.Where("tenant_id = ?", id).Find(&docs)
	for _, d := range docs {
		if h.s3 != nil {
			if key := h.s3.ExtractKey(d.URL); key != "" {
				_ = h.s3.Delete(c, key)
			}
		}
	}
	database.DB.Where("tenant_id = ?", id).Delete(&model.TenantDocument{})

	database.DB.Delete(&model.Tenant{}, "id = ?", id)
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// Update — редактирование карточки арендатора владельцем (канвас «14»,
// «Редактирование арендатора»). Смена телефона перепривязывает аккаунт:
// новый номер совпал с юзером → бинд, нет → отвязка.
func (h *TenantHandler) Update(c *gin.Context) {
	id := c.Param("id")
	userID := c.GetString("userID")
	var input model.Tenant
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updates := map[string]interface{}{}
	if input.FullName != "" {
		updates["full_name"] = input.FullName
	}
	if input.CompanyName != nil {
		updates["company_name"] = *input.CompanyName
	}
	if input.Email != nil {
		updates["email"] = *input.Email
	}
	if input.Phone != "" {
		updates["phone"] = input.Phone
		if uid := findUserByPhone(input.Phone); uid != "" {
			updates["user_id"] = uid
			database.DB.Model(&model.User{}).Where("id = ?", uid).Update("is_tenant", true)
		} else {
			updates["user_id"] = nil
		}
	}
	if input.PassportData != nil {
		updates["passport_data"] = *input.PassportData
	}
	if input.ServiceInfo != nil {
		updates["service_info"] = *input.ServiceInfo
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
		return
	}
	res := database.DB.Model(&model.Tenant{}).
		Where("id = ? AND owner_id = ?", id, userID).Updates(updates)
	if res.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	var tenant model.Tenant
	database.DB.First(&tenant, "id = ?", id)
	one := []model.Tenant{tenant}
	attachTenantAvatars(&one)
	c.JSON(http.StatusOK, one[0])
}

// Restore — «Отменить удаление» (Figma 3014:22433): вернуть мягко удалённую
// карточку; связей с объектами у удалённого и не было (удаление при активной
// аренде запрещено), поэтому восстанавливаем запись и флаг is_tenant
func (h *TenantHandler) Restore(c *gin.Context) {
	id := c.Param("id")
	if err := database.DB.Unscoped().Model(&model.Tenant{}).Where("id = ?", id).
		Update("deleted_at", nil).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	if tenant.UserID != nil {
		database.DB.Model(&model.User{}).Where("id = ?", *tenant.UserID).Update("is_tenant", true)
	}
	c.JSON(http.StatusOK, tenant)
}

// TenantBookingView — бронь арендатора с данными объекта (для карточки арендатора)
type TenantBookingView struct {
	PropertyID      string `json:"property_id"`
	PropertyName    string `json:"property_name"`
	PropertyAddress string `json:"property_address"`
	PropertyPhoto   string `json:"property_photo"`
	StartDate       string `json:"start_date"`
	EndDate         string `json:"end_date"`
}

type TenantCardResponse struct {
	model.Tenant
	Bookings  []TenantBookingView    `json:"bookings"`
	Documents []model.TenantDocument `json:"documents"`
}

// Card — весь экран «Карточка арендатора» одним запросом: арендатор
// (дозавязка + аватарка) и его брони с именами/фото объектов. Заменяет
// клиентский веер tenant + properties + bookings×N.
func (h *TenantHandler) Card(c *gin.Context) {
	id := c.Param("id")
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	one := []model.Tenant{tenant}
	BindTenantsByPhone(&one)
	attachTenantAvatars(&one)

	views := make([]TenantBookingView, 0)
	var bookings []model.Booking
	if err := database.DB.Where("tenant_id = ?", id).Order("start_date asc").Find(&bookings).Error; err == nil && len(bookings) > 0 {
		propIDs := make([]string, 0, len(bookings))
		for _, b := range bookings {
			propIDs = append(propIDs, b.PropertyID)
		}
		var props []model.Property
		database.DB.Preload("Photos").Where("id IN ?", propIDs).Find(&props)
		byID := make(map[string]model.Property, len(props))
		for _, p := range props {
			byID[p.ID] = p
		}
		for _, b := range bookings {
			v := TenantBookingView{PropertyID: b.PropertyID, StartDate: b.StartDate, EndDate: b.EndDate}
			if p, ok := byID[b.PropertyID]; ok {
				v.PropertyName = p.Name
				v.PropertyAddress = p.Address
				if len(p.Photos) > 0 {
					v.PropertyPhoto = p.Photos[0].URL
				}
			}
			views = append(views, v)
		}
	}
	// Документы карточки (сканы/фото/файлы) — тем же запросом, что и брони
	docs := make([]model.TenantDocument, 0)
	database.DB.Where("tenant_id = ?", id).Order("created_at asc").Find(&docs)
	c.JSON(http.StatusOK, TenantCardResponse{Tenant: one[0], Bookings: views, Documents: docs})
}

// AddDocument — прикрепить документ к карточке арендатора (канвас «14», 2983:42232).
// Файл уже загружен в хранилище через POST /upload?folder=documents; сюда приходят
// метаданные: имя (имя файла либо «Фото от DD.MM.YYYY» для камеры), тип (JPG/PDF…),
// ссылка и размер. Дата добавления — CreatedAt.
func (h *TenantHandler) AddDocument(c *gin.Context) {
	id := c.Param("id")
	userID := c.GetString("userID")
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	if tenant.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not owner"})
		return
	}
	var req struct {
		Name     string `json:"name" binding:"required"`
		FileType string `json:"file_type"`
		URL      string `json:"url" binding:"required"`
		Size     int64  `json:"size"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	doc := model.TenantDocument{
		TenantID: id,
		Name:     req.Name,
		FileType: req.FileType,
		URL:      req.URL,
		Size:     req.Size,
	}
	if err := database.DB.Create(&doc).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, doc)
}

// DeleteDocument — удаление документа: запись + файл в хранилище.
func (h *TenantHandler) DeleteDocument(c *gin.Context) {
	id := c.Param("id")
	docID := c.Param("docId")
	userID := c.GetString("userID")
	var tenant model.Tenant
	if err := database.DB.First(&tenant, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	if tenant.OwnerID != userID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not owner"})
		return
	}
	var doc model.TenantDocument
	if err := database.DB.First(&doc, "id = ? AND tenant_id = ?", docID, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "document not found"})
		return
	}
	if h.s3 != nil {
		if key := h.s3.ExtractKey(doc.URL); key != "" {
			_ = h.s3.Delete(c, key)
		}
	}
	database.DB.Delete(&model.TenantDocument{}, "id = ?", docID)
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
		if err := database.DB.Unscoped().Where("owner_id = ? AND user_id = ?", c.GetString("userID"), req.UserID).First(&tenant).Error; err == nil {
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

	// Арендатор нужен сразу: обновляем снимок на объекте его живыми данными
	var tenant model.Tenant
	database.DB.First(&tenant, "id = ?", tenantID)

	database.DB.Model(&model.Property{}).Where("id = ?", propertyID).
		Updates(map[string]interface{}{
			"tenant_id":   tenantID,
			"status":      "occupied",
			"tenant_info": tenant.FullName,
			"phone":       tenant.Phone,
		})

	// Push-уведомление арендатору о предоставлении доступа
	if tenant.ID != "" && tenant.UserID != nil && h.fcm != nil {
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

// DetachFromProperty — открепить арендатора от объекта (Figma 2936:41556).
// Делает две вещи сразу:
//  1. освобождает объект: tenant_id/tenant_info/phone/rent_end_date → nil,
//     status = "free";
//  2. освобождает шахматку с первого числа текущего месяца: бронь, начавшаяся
//     в прошлом, обрезается концом предыдущего месяца (история аренды
//     остаётся), брони текущего месяца и будущие снимаются.
//
// Без п.2 карточка объекта уже показывает «Арендатор не добавлен · готова
// к аренде», а шахматка остаётся зелёной — расхождение, которое чинится здесь.
func (h *TenantHandler) DetachFromProperty(c *gin.Context) {
	propertyID := c.Param("id")
	var property model.Property
	if err := database.DB.First(&property, "id = ?", propertyID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "property not found"})
		return
	}

	// Арендатор нужен до очистки связи — для push-уведомления
	var tenant model.Tenant
	if property.TenantID != nil {
		database.DB.First(&tenant, "id = ?", *property.TenantID)
	}

	database.DB.Model(&model.Property{}).Where("id = ?", propertyID).
		Updates(map[string]interface{}{
			"tenant_id":     nil,
			"tenant_info":   nil,
			"phone":         nil,
			"rent_end_date": nil,
			"status":        "free",
		})

	// Шахматка свободна с первого числа текущего месяца.
	// Даты броней — строки YYYY-MM-DD, лексикографическое сравнение корректно.
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).
		Format("2006-01-02")
	prevMonthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, -1).Format("2006-01-02")
	var bookings []model.Booking
	database.DB.Where("property_id = ? AND end_date >= ?", propertyID, monthStart).Find(&bookings)
	for _, b := range bookings {
		if b.StartDate < monthStart {
			database.DB.Model(&model.Booking{}).Where("id = ?", b.ID).
				Update("end_date", prevMonthEnd)
		} else {
			database.DB.Delete(&model.Booking{}, "id = ?", b.ID)
		}
	}

	// Push арендатору: доступ к объекту отозван (обещание шита открепления)
	if tenant.ID != "" && tenant.UserID != nil && h.fcm != nil {
		name := property.Name
		if name == "" {
			name = "объект"
		}
		go h.fcm.SendToUser(*tenant.UserID, map[string]string{
			"type":  "tenant_detached",
			"title": "Доступ к объекту отозван",
			"body":  "Арендодатель открепил вас от объекта «" + name + "»",
		}, "")
	}

	c.JSON(http.StatusOK, gin.H{"message": "tenant detached"})
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
	// Бронь наследует арендатора объекта: клиент создаёт её сразу после
	// прикрепления, и без tenant_id история аренды в карточке пустая
	if booking.TenantID == nil || *booking.TenantID == "" {
		var property model.Property
		if err := database.DB.First(&property, "id = ?", propertyID).Error; err == nil && property.TenantID != nil {
			booking.TenantID = property.TenantID
		}
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

// CreateSchedule — создание графика платежей.
// UPSERT по (user_id, property_id): график у объекта один, повторные
// сохранения обновляют его, а не создают дубликаты.
func (h *PaymentHandler) CreateSchedule(c *gin.Context) {
	userID := c.GetString("userID")
	var schedule model.PaymentSchedule
	if err := c.ShouldBindJSON(&schedule); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	schedule.UserID = userID
	var existing model.PaymentSchedule
	err := database.DB.Where("user_id = ? AND property_id = ?", userID, schedule.PropertyID).
		First(&existing).Error
	if err == nil {
		updates := map[string]interface{}{
			"day_of_month": schedule.DayOfMonth,
			"amount":       schedule.Amount,
			"type":         schedule.Type,
			"custom_dates": schedule.CustomDates,
			"requisites":   schedule.Requisites,
		}
		if err := database.DB.Model(&existing).Updates(updates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		database.DB.Where("user_id = ? AND property_id = ?", userID, schedule.PropertyID).
			First(&existing)
		c.JSON(http.StatusOK, existing)
		return
	}
	schedule.BaseModel.ID = uuid.New().String()
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
