package model

// Property — объект недвижимости
type Property struct {
	BaseModel
	UserID         string   `gorm:"index;not null;size:36" json:"user_id"`
	Name           string   `gorm:"not null;size:200" json:"name"`
	Address        string   `gorm:"size:500" json:"address"`
	Area           *float64 `json:"area"`
	Type           *string  `gorm:"size:50" json:"type"`
	RentType       *string  `gorm:"size:20" json:"rent_type"`
	Rooms          *string  `gorm:"size:50" json:"rooms"`
	SleepingPlaces *string  `gorm:"size:20" json:"sleeping_places"`
	Floor          *string  `gorm:"size:20" json:"floor"`
	FloorsInHouse  *string  `gorm:"size:20" json:"floors_in_house"`
	Description    *string  `gorm:"type:text" json:"description"`
	TenantInfo     *string  `gorm:"type:text" json:"tenant_info"`
	ServiceInfo    *string  `gorm:"type:text" json:"service_info"`
	Phone          *string  `gorm:"size:30" json:"phone"`
	WifiPassword   *string  `gorm:"size:100" json:"wifi_password"`
	HouseRules     *string  `gorm:"type:text" json:"house_rules"`
	Status         string   `gorm:"size:20;default:free" json:"status"` // free / occupied
	RentAmount     *float64 `json:"rent_amount"`
	RentEndDate    *string  `gorm:"size:30" json:"rent_end_date"`
	ContractNumber *string  `gorm:"size:50" json:"contract_number"`
	ContractDate   *string  `gorm:"size:30" json:"contract_date"`
	TenantID       *string  `gorm:"size:36;index" json:"tenant_id"`
	Photos         []Photo  `gorm:"foreignKey:PropertyID" json:"photos,omitempty"`
	Latitude       *float64 `json:"latitude"`
	Longitude      *float64 `json:"longitude"`
	// IsPublished — опубликовано ли объявление объекта для арендаторов
	IsPublished bool `gorm:"default:false" json:"is_published"`
}

// Photo — фотография объекта
type Photo struct {
	ID         string `gorm:"primaryKey;size:36" json:"id"`
	PropertyID string `gorm:"index;not null;size:36" json:"property_id"`
	URL        string `gorm:"not null;size:500" json:"url"`
}

// Tenant — арендатор
type Tenant struct {
	BaseModel
	OwnerID      string  `gorm:"index;not null;size:36" json:"owner_id"`
	UserID       *string `gorm:"size:36;index" json:"user_id"`
	FullName     string  `gorm:"not null;size:200" json:"full_name"`
	CompanyName  *string `gorm:"size:200" json:"company_name"`
	PassportData *string `gorm:"type:text" json:"passport_data"`
	Phone        string  `gorm:"not null;size:20" json:"phone"`
	Email        *string `gorm:"size:100" json:"email"`
	Active       bool    `gorm:"default:true" json:"active"`
	ServiceInfo  *string `gorm:"type:text" json:"service_info"`
	// Аватарка НЕ хранится в записи: живьём берётся с аккаунта по user_id
	// (решение Сергея/Дениса 2026-09-18: аву каждый ставит себе сам, как в мессенджерах;
	// сервер подтягивает её по телефону — уникальному идентификатору)
	AvatarURL *string `gorm:"-" json:"avatar_url"`
}

// TenantDocument — документ, прикреплённый к карточке арендатора (скан паспорта,
// фото документа, PDF). Сам файл лежит в S3 (folder=documents), в записи — метаданные:
// имя, тип (JPG/PDF…), ссылка и размер. Дата добавления = CreatedAt.
type TenantDocument struct {
	BaseModel
	TenantID string `gorm:"index;not null;size:36" json:"tenant_id"`
	Name     string `gorm:"not null;size:255" json:"name"`
	FileType string `gorm:"size:16" json:"file_type"`
	URL      string `gorm:"not null;size:500" json:"url"`
	Size     int64  `gorm:"default:0" json:"size"`
}

// Meter — прибор учёта
type Meter struct {
	BaseModel
	PropertyID           string  `gorm:"index;not null;size:36" json:"property_id"`
	UserID               string  `gorm:"index;not null;size:36" json:"user_id"`
	Type                 string  `gorm:"not null;size:30" json:"type"` // hot_water, cold_water, electricity, heat
	FactoryNumber        string  `gorm:"not null;size:100" json:"factory_number"`
	NextVerificationDate string  `gorm:"size:30" json:"next_verification_date"`
	CurrentValue         float64 `gorm:"not null" json:"current_value"`
	Unit                 string  `gorm:"not null;size:20" json:"unit"` // м³, кВт·ч, Гкал
	SubmitReadingsBy     string  `gorm:"size:30" json:"submit_readings_by"`
	LastUpdated          *string `gorm:"size:30" json:"last_updated"`
	// Напоминания из формы «Добавить счетчик» (тумблеры «Включить напоминание»)
	RemindVerification bool `gorm:"default:false" json:"remind_verification"`
	RemindReadings     bool `gorm:"default:false" json:"remind_readings"`
}

// MeterReading — внесённое показание счётчика (история)
type MeterReading struct {
	BaseModel
	MeterID    string  `gorm:"index;not null;size:36" json:"meter_id"`
	PropertyID string  `gorm:"index;not null;size:36" json:"property_id"`
	UserID     string  `gorm:"index;not null;size:36" json:"user_id"`
	Value      float64 `gorm:"not null" json:"value"`
	Unit       string  `gorm:"size:20" json:"unit"`
	Date       string  `gorm:"size:30" json:"date"` // YYYY-MM-DD
}

// Payment — платёж
type Payment struct {
	BaseModel
	PropertyID string  `gorm:"index;not null;size:36" json:"property_id"`
	UserID     string  `gorm:"index;not null;size:36" json:"user_id"`
	Amount     float64 `gorm:"not null" json:"amount"`
	Date       string  `gorm:"not null;size:30" json:"date"`
	Status     string  `gorm:"size:20;default:pending" json:"status"` // paid / pending / overdue
	Type       string  `gorm:"size:20;not null" json:"type"`          // income / expense
}

// PaymentSchedule — график платежей
type PaymentSchedule struct {
	BaseModel
	PropertyID  string   `gorm:"index;not null;size:36" json:"property_id"`
	UserID      string   `gorm:"index;not null;size:36" json:"user_id"`
	DayOfMonth  *int     `json:"day_of_month"` // для автоматических ежемесячных
	Amount      *float64 `json:"amount"`
	Type        string   `gorm:"size:20;default:auto" json:"type"` // auto / manual
	CustomDates *string  `gorm:"type:jsonb" json:"custom_dates"`   // JSONB для ручного ввода
	Requisites  *string  `gorm:"size:36" json:"requisites"`        // ID выбранных реквизитов (PaymentRequisite)
}

// PaymentRequisite — реквизиты арендодателя для приёма платежей
// (общие на аккаунт, не привязаны к объекту)
type PaymentRequisite struct {
	BaseModel
	UserID  string `gorm:"index;not null;size:36" json:"user_id"`
	Name    string `gorm:"not null;size:200" json:"name"` // «ИП Петров В.А.» / «ООО «Легенда»»
	Account string `gorm:"size:50" json:"account"`        // расчётный счёт
	Bank    string `gorm:"size:100" json:"bank"`          // «Точка»
}

// Booking — период занятости объекта (заливка шахматки)
type Booking struct {
	BaseModel
	PropertyID string  `gorm:"index;not null;size:36" json:"property_id"`
	TenantID   *string `gorm:"size:36;index" json:"tenant_id"`
	StartDate  string  `gorm:"not null;size:30" json:"start_date"` // YYYY-MM-DD
	EndDate    string  `gorm:"not null;size:30" json:"end_date"`   // YYYY-MM-DD
	Source     string  `gorm:"size:20;default:manual" json:"source"`
	CreatedBy  string  `gorm:"index;not null;size:36" json:"created_by"`
}

// Chat — чат
type Chat struct {
	BaseModel
	PropertyID     string `gorm:"index;size:36" json:"property_id"`
	ParticipantIDs string `gorm:"not null;type:jsonb" json:"participant_ids"`
}

// RefreshToken — токен для обновления access-токена.
// RotatedAt/ReissuedAt (мс, 0 = не выставлено): ротация ПОМЕЧАЕТ токен
// вместо удаления, чтобы отличить потерянный HTTP-ответ (повтор в
// grace-окне) от кражи (поздний/повторный replay).
type RefreshToken struct {
	ID         string `gorm:"primaryKey;size:36" json:"id"`
	UserID     string `gorm:"index;not null;size:36" json:"user_id"`
	Token      string `gorm:"uniqueIndex;not null;size:255" json:"token"`
	ExpiresAt  int64  `gorm:"not null;index" json:"expires_at"`
	CreatedAt  int64  `gorm:"autoCreateTime:milli" json:"created_at"`
	RotatedAt  int64  `gorm:"not null;default:0" json:"rotated_at"`  // мс; >0 = уже ротирован
	ReissuedAt int64  `gorm:"not null;default:0" json:"reissued_at"` // мс; >0 = переигрыш в grace уже выдавался
}

// Message — сообщение в чате
type Message struct {
	ID        string `gorm:"primaryKey;size:36" json:"id"`
	ChatID    string `gorm:"index;not null;size:36" json:"chat_id"`
	SenderID  string `gorm:"not null;size:36" json:"sender_id"`
	Text      string `gorm:"type:text;not null" json:"text"`
	Read      bool   `gorm:"default:false" json:"read"`
	CreatedAt int64  `gorm:"not null" json:"timestamp"`
}
