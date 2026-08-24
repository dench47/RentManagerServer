package model

import "time"

// TelegramBinding — привязка пользователя к Telegram-аккаунту.
// Один пользователь = один Telegram-аккаунт (уникальный UserID).
type TelegramBinding struct {
	ID             string    `gorm:"primaryKey;size:36" json:"id"`
	UserID         string    `gorm:"uniqueIndex;not null;size:36" json:"user_id"`
	ChatID         int64     `gorm:"uniqueIndex;not null" json:"chat_id"` // Telegram chat_id (int64)
	TelegramUserID int64     `gorm:"not null" json:"telegram_user_id"`    // Telegram ID пользователя (может быть не уникальным если аккаунт удалён)
	Username       string    `gorm:"size:64" json:"username"`             // @username (может быть пустым)
	FirstName      string    `gorm:"size:128" json:"first_name"`
	LinkedAt       time.Time `gorm:"autoCreateTime" json:"linked_at"`
}

func (TelegramBinding) TableName() string {
	return "telegram_bindings"
}
