package service

import (
	"context"
	"fmt"
	"log"

	"rentmanager-server/internal/database"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"
)

const fcmTokenKeyPrefix = "fcm_tokens:"

type FCMService struct {
	app *firebase.App
}

func NewFCMService(credentialsJSON []byte) (*FCMService, error) {
	opt := option.WithCredentialsJSON(credentialsJSON)
	app, err := firebase.NewApp(context.Background(), nil, opt)
	if err != nil {
		return nil, fmt.Errorf("firebase.NewApp: %w", err)
	}

	return &FCMService{app: app}, nil
}

func (s *FCMService) tokenKey(userID string) string {
	return fcmTokenKeyPrefix + userID
}

// RegisterToken — привязывает FCM-токен к пользователю (хранится в Redis, переживает рестарты)
func (s *FCMService) RegisterToken(userID, token string) {
	if err := database.RDB.SAdd(context.Background(), s.tokenKey(userID), token).Err(); err != nil {
		log.Printf("FCM: failed to persist token for user %s: %v", userID, err)
		return
	}
	log.Printf("FCM: registered token for user %s", userID)
}

// RemoveToken — отвязывает FCM-токен от пользователя
func (s *FCMService) RemoveToken(userID, token string) {
	if err := database.RDB.SRem(context.Background(), s.tokenKey(userID), token).Err(); err != nil {
		log.Printf("FCM: failed to remove token for user %s: %v", userID, err)
	}
}

// ClearUserTokens — удаляет все FCM-токены пользователя
func (s *FCMService) ClearUserTokens(userID string) {
	if err := database.RDB.Del(context.Background(), s.tokenKey(userID)).Err(); err != nil {
		log.Printf("FCM: failed to clear tokens for user %s: %v", userID, err)
	}
}

func (s *FCMService) SendToUser(userID string, data map[string]string, excludeToken string) (int, error) {
	tokens := database.RDB.SMembers(context.Background(), s.tokenKey(userID)).Val()

	// Фильтруем — исключаем устройство, с которого был вход
	if excludeToken != "" {
		filtered := make([]string, 0, len(tokens))
		for _, t := range tokens {
			if t != excludeToken {
				filtered = append(filtered, t)
			}
		}
		tokens = filtered
	}

	if len(tokens) == 0 {
		log.Printf("FCM: no tokens for user %s — push skipped", userID)
		return 0, nil
	}

	log.Printf("FCM: sending push to %d device(s) for user %s", len(tokens), userID)
	ctx := context.Background()
	client, err := s.app.Messaging(ctx)
	if err != nil {
		return 0, fmt.Errorf("messaging client: %w", err)
	}

	msg := &messaging.MulticastMessage{
		Data:   data,
		Tokens: tokens,
		Android: &messaging.AndroidConfig{
			Priority: "high",
		},
	}

	resp, err := client.SendEachForMulticast(ctx, msg)
	if err != nil {
		log.Printf("FCM: send failed for user %s: %v", userID, err)
		return 0, fmt.Errorf("send multicast: %w", err)
	}

	log.Printf("FCM: sent %d/%d successfully for user %s", resp.SuccessCount, len(tokens), userID)

	// Удаляем невалидные токены
	for i, r := range resp.Responses {
		if !r.Success && (messaging.IsUnregistered(r.Error) || messaging.IsInvalidArgument(r.Error)) {
			s.RemoveToken(userID, tokens[i])
		}
	}

	return resp.SuccessCount, nil
}
