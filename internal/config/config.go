package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	ServerPort    string
	DB            DBConfig
	Redis         RedisConfig
	JWT           JWTConfig
	S3            S3Config
	UploadDir     string
	DownloadDir   string
	MaxUploadSize int64
	APKBaseURL    string
	Telegram      TelegramConfig
}

type TelegramConfig struct {
	BotToken    string
	BotUsername string
}

type DBConfig struct {
	Host     string
	Port     string
	Name     string
	User     string
	Password string
}

func (c DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		c.Host, c.Port, c.User, c.Password, c.Name,
	)
}

type RedisConfig struct {
	Host     string
	Port     string
	Password string
}

func (c RedisConfig) Addr() string {
	return fmt.Sprintf("%s:%s", c.Host, c.Port)
}

type JWTConfig struct {
	Secret     string
	AccessTTL  int
	RefreshTTL int
}

type S3Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	PublicURL string
	Region    string
}

func Load() *Config {
	return &Config{
		ServerPort: getEnv("SERVER_PORT", "8080"),
		DB: DBConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnv("DB_PORT", "5432"),
			Name:     getEnv("DB_NAME", "rentmanager"),
			User:     getEnv("DB_USER", "rentmanager"),
			Password: getEnv("DB_PASSWORD", "rentmanager_secret"),
		},
		Redis: RedisConfig{
			Host:     getEnv("REDIS_HOST", "localhost"),
			Port:     getEnv("REDIS_PORT", "6379"),
			Password: getEnv("REDIS_PASSWORD", ""),
		},
		JWT: JWTConfig{
			Secret:     getEnv("JWT_SECRET", "default_secret"),
			AccessTTL:  getEnvAsInt("JWT_ACCESS_TTL", 3600),
			RefreshTTL: getEnvAsInt("JWT_REFRESH_TTL", 604800),
		},
		S3: S3Config{
			Endpoint:  getEnv("S3_ENDPOINT", ""),
			Bucket:    getEnv("S3_BUCKET", ""),
			AccessKey: getEnv("S3_ACCESS_KEY", ""),
			SecretKey: getEnv("S3_SECRET_KEY", ""),
			PublicURL: getEnv("S3_PUBLIC_URL", ""),
			Region:    getEnv("S3_REGION", "ru1"),
		},
		UploadDir:     getEnv("UPLOAD_DIR", "./uploads"),
		DownloadDir:   getEnv("DOWNLOAD_DIR", "./downloads"),
		MaxUploadSize: int64(getEnvAsInt("MAX_UPLOAD_SIZE", 10485760)),
		APKBaseURL:    getEnv("APK_BASE_URL", "http://45.11.92.171:8080"),
		Telegram: TelegramConfig{
			BotToken:    getEnv("TELEGRAM_BOT_TOKEN", ""),
			BotUsername: getEnv("TELEGRAM_BOT_USERNAME", ""),
		},
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}
