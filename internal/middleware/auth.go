package middleware

import (
	"log"
	"net/http"
	"strings"

	"rentmanager-server/internal/config"
	"rentmanager-server/internal/database"
	"rentmanager-server/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID       string `json:"user_id"`
	Phone        string `json:"phone"`
	TokenVersion int    `json:"token_version"`
	jwt.RegisteredClaims
}

func AuthMiddleware(cfg config.JWTConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization header required"})
			c.Abort()
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authorization header must be: Bearer <token>"})
			c.Abort()
			return
		}

		tokenStr := parts[1]
		claims := &Claims{}

		token, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (interface{}, error) {
			return []byte(cfg.Secret), nil
		})

		if err != nil || token == nil || !token.Valid {
			valid := token != nil && token.Valid
			log.Printf("JWT auth failed: err=%v valid=%v token=%s", err, valid, tokenStr[:min(20, len(tokenStr))])
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			c.Abort()
			return
		}

		// Проверяем, что пользователь существует и токен не отозван
		// (logoutAll / сброс PIN / УДАЛЕНИЕ АККАУНТА).
		// Раньше отсутствие пользователя в БД молча пропускало проверку —
		// после удаления аккаунта чужие access-токены продолжали работать.
		var user model.User
		if err := database.DB.Select("token_version").First(&user, "id = ?", claims.UserID).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Token revoked"})
			c.Abort()
			return
		}
		if user.TokenVersion != claims.TokenVersion {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Token revoked"})
			c.Abort()
			return
		}

		c.Set("userID", claims.UserID)
		c.Set("phone", claims.Phone)
		c.Next()
	}
}
