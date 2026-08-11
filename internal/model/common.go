package model

import (
	"time"

	"gorm.io/gorm"
)

type BaseModel struct {
	ID        string         `gorm:"primaryKey;size:36" json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}