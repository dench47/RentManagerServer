package model

import "gorm.io/gorm"

type User struct {
	BaseModel
	Phone              string `gorm:"uniqueIndex;not null;size:20" json:"phone"`
	Name               string `gorm:"not null;default:'';size:100" json:"name"`
	IsLandlord         bool   `gorm:"default:false" json:"is_landlord"`
	IsTenant           bool   `gorm:"default:false" json:"is_tenant"`
	LegalName          string `gorm:"size:200" json:"legal_name"`
	AvatarURL          string `gorm:"size:500" json:"avatar_url"`
	Email              string `gorm:"size:200" json:"email"`
	EmailVerified      bool   `gorm:"default:false" json:"email_verified"`
	Email2FAEnabled    bool   `gorm:"column:email_2fa_enabled;default:false" json:"email_2fa_enabled"`
	TokenVersion       int    `gorm:"default:0" json:"token_version"`
	PasswordHash       string `gorm:"size:255" json:"-"`
	HasPassword        bool   `gorm:"-" json:"has_password"`
	FullName           string `gorm:"default:'';size:300" json:"full_name"`
	DefaultStartScreen string `gorm:"default:'';size:20" json:"default_start_screen"`
	// Подписка: баланс (пополнения списываются на тариф) и применённый промокод
	Balance          float64 `gorm:"default:0" json:"balance"`
	AppliedPromoCode string  `gorm:"default:'';size:50" json:"applied_promo_code"`
}

// PromoCode — промокод подписки: льготный тариф вместо базового 10 ₽/день
type PromoCode struct {
	BaseModel
	Code string  `gorm:"uniqueIndex;not null;size:50" json:"code"`
	Rate float64 `json:"rate"` // ₽ / объект / день
	Note string  `gorm:"size:200" json:"note"`
}

// SubscriptionOperation — операция по балансу подписки: пополнение,
// ежедневное списание за тариф, бонус промокода
type SubscriptionOperation struct {
	BaseModel
	UserID   string  `gorm:"index;not null;size:36" json:"user_id"`
	Type     string  `gorm:"size:20;not null" json:"type"`       // topup / charge / bonus
	Title    string  `gorm:"size:100" json:"title"`              // «Пополнение баланса»
	Subtitle string  `gorm:"size:200" json:"subtitle"`           // «Банковская карта •• 2545»
	Amount   float64 `gorm:"not null" json:"amount"`             // +30 / −30
	Status   string  `gorm:"size:20;default:done" json:"status"` // done / credited / failed
}

// AfterFind заполняет HasPassword после чтения из БД, не отдавая сам хэш клиенту.
func (u *User) AfterFind(tx *gorm.DB) error {
	u.HasPassword = u.PasswordHash != ""
	return nil
}

// TrustedDevice — устройство, прошедшее верификацию (звонок / push-подтверждение).
// На доверенном устройстве вход выполняется по локальному PIN/биометрии,
// с нового устройства требуется подтверждение (push или звонок).
type TrustedDevice struct {
	ID         string `gorm:"primaryKey;size:36" json:"id"`
	UserID     string `gorm:"index;not null;size:36" json:"user_id"`
	DeviceID   string `gorm:"index;not null;size:64;uniqueIndex:idx_user_device" json:"device_id"`
	Name       string `gorm:"size:120" json:"name"` // человекочитаемое имя: "Xiaomi Redmi Note 12"
	CreatedAt  int64  `gorm:"autoCreateTime:milli" json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
}
