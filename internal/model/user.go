package model

type User struct {
	BaseModel
	Phone              string `gorm:"uniqueIndex;not null;size:20" json:"phone"`
	Name               string `gorm:"not null;default:'';size:100" json:"name"`
	IsLandlord         bool   `gorm:"default:false" json:"is_landlord"`
	IsTenant           bool   `gorm:"default:false" json:"is_tenant"`
	LegalName          string `gorm:"size:200" json:"legal_name"`
	AvatarURL          string `gorm:"size:500" json:"avatar_url"`
	Email              string `gorm:"size:200" json:"email"`
	TokenVersion       int    `gorm:"default:0" json:"token_version"`
	PasswordHash       string `gorm:"size:255" json:"password_hash"`
	FullName           string `gorm:"default:'';size:300" json:"full_name"`
	DefaultStartScreen string `gorm:"default:'';size:20" json:"default_start_screen"`
}
