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
const fcmTokenOwnerPrefix = "fcm_token_owner:"
const fcmDeviceKeyPrefix = "fcm_device:"

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

func (s *FCMService) deviceKey(userID, deviceID string) string {
	return fcmDeviceKeyPrefix + userID + ":" + deviceID
}

// RegisterToken — привязывает FCM-токен к пользователю и его устройству (хранится в Redis).
// deviceID нужен, чтобы слать push только на доверенные устройства (login_request)
// и удалять токен при отзыве доверенности.
func (s *FCMService) RegisterToken(userID, token, deviceID string) {
	ctx := context.Background()
	ownerKey := fcmTokenOwnerPrefix + token

	if oldOwner, err := database.RDB.Get(ctx, ownerKey).Result(); err == nil && oldOwner != "" && oldOwner != userID {
		database.RDB.SRem(ctx, s.tokenKey(oldOwner), token)
		log.Printf("FCM: token moved from user %s to %s", oldOwner, userID)
	}

	if err := database.RDB.SAdd(ctx, s.tokenKey(userID), token).Err(); err != nil {
		log.Printf("FCM: failed to persist token for user %s: %v", userID, err)
		return
	}
	database.RDB.Set(ctx, ownerKey, userID, 0)
	if deviceID != "" {
		database.RDB.Set(ctx, s.deviceKey(userID, deviceID), token, 0)
	}
	log.Printf("FCM: registered token for user %s device %s", userID, deviceID)
}

// RemoveToken — отвязывает FCM-токен от пользователя
func (s *FCMService) RemoveToken(userID, token string) {
	ctx := context.Background()
	if err := database.RDB.SRem(ctx, s.tokenKey(userID), token).Err(); err != nil {
		log.Printf("FCM: failed to remove token for user %s: %v", userID, err)
	}
	database.RDB.Del(ctx, fcmTokenOwnerPrefix+token)
}

// RemoveTokenForDevice — удаляет токен конкретного устройства (при отзыве доверенности).
func (s *FCMService) RemoveTokenForDevice(userID, deviceID string) {
	if deviceID == "" {
		return
	}
	ctx := context.Background()
	key := s.deviceKey(userID, deviceID)
	token, err := database.RDB.Get(ctx, key).Result()
	if err == nil && token != "" {
		database.RDB.SRem(ctx, s.tokenKey(userID), token)
		database.RDB.Del(ctx, fcmTokenOwnerPrefix+token)
		log.Printf("FCM: removed token for revoked device %s", deviceID)
	}
	database.RDB.Del(ctx, key)
}

// SendToDeviceIDs — отправляет push только на токены конкретных устройств (device_id).
// Используется для login_request: push уходит только на доверенные устройства.
func (s *FCMService) SendToDeviceIDs(userID string, deviceIDs []string, data map[string]string) (int, error) {
	ctx := context.Background()
	var tokens []string
	seen := map[string]bool{}
	for _, did := range deviceIDs {
		if did == "" {
			continue
		}
		t, err := database.RDB.Get(ctx, s.deviceKey(userID, did)).Result()
		if err == nil && t != "" && !seen[t] {
			seen[t] = true
			tokens = append(tokens, t)
		}
	}
	if len(tokens) == 0 {
		log.Printf("FCM: no trusted device tokens for user %s — push skipped", userID)
		return 0, nil
	}
	return s.sendMulticast(userID, tokens, data)
}

// ClearUserTokens — удаляет все FCM-токены пользователя
func (s *FCMService) ClearUserTokens(userID string) {
	ctx := context.Background()
	key := s.tokenKey(userID)
	if tokens, err := database.RDB.SMembers(ctx, key).Result(); err == nil {
		for _, t := range tokens {
			database.RDB.Del(ctx, fcmTokenOwnerPrefix+t)
		}
	}
	if err := database.RDB.Del(ctx, key).Err(); err != nil {
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

	return s.sendMulticast(userID, tokens, data)
}

func (s *FCMService) sendMulticast(userID string, tokens []string, data map[string]string) (int, error) {
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
