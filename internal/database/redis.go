package database

import (
	"context"
	"log"
	"rentmanager-server/internal/config"
	"time"

	"github.com/redis/go-redis/v9"
)

var RDB *redis.Client

func InitRedis(cfg config.RedisConfig) {
	RDB = redis.NewClient(&redis.Options{
		Addr:     cfg.Addr(),
		Password: cfg.Password,
		DB:       0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := RDB.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	log.Println("Redis connected")
}