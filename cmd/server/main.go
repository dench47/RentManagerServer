package main

import (
	"fmt"
	"log"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/database"
	"rentmanager-server/internal/router"

	"github.com/joho/godotenv"
)

func main() {
	// Load .env file (ignore error if not found — env vars may be set directly)
	_ = godotenv.Load()

	cfg := config.Load()

	// Database
	database.InitPostgres(cfg.DB)
	database.InitRedis(cfg.Redis)

	// Router
	r := router.Setup(cfg)

	addr := fmt.Sprintf(":%s", cfg.ServerPort)
	log.Printf("Server starting on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
