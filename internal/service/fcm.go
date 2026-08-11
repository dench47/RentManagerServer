package service

import (
	"context"
	"fmt"
	"log"
	"sync"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"google.golang.org/api/option"
)

type FCMService struct {
	app    *firebase.App
	mu     sync.RWMutex
	tokens map[string][]string // userID -> []fcmToken
}

func NewFCMService(credentialsJSON []byte) (*FCMService, error) {
	opt := option.WithCredentialsJSON(credentialsJSON)
	app, err := firebase.NewApp(context.Background(), nil, opt)
	if err != nil {
		return nil, fmt.Errorf("firebase.NewApp: %w", err)
	}

	return &FCMService{
		app:    app,
		tokens: make(map[string][]string),
	}, nil
}

func (s *FCMService) RegisterToken(userID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Удаляем дубликаты и добавляем
	existing := s.tokens[userID]
	for _, t := range existing {
		if t == token {
			return // уже зарегистрирован
		}
	}
	s.tokens[userID] = append(existing, token)
	log.Printf("FCM: registered token for user %s", userID)
}

func (s *FCMService) RemoveToken(userID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tokens := s.tokens[userID]
	filtered := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t != token {
			filtered = append(filtered, t)
		}
	}
	s.tokens[userID] = filtered
}

func (s *FCMService) SendToUser(userID string, data map[string]string, excludeToken string) (int, error) {
	s.mu.RLock()
	tokens := s.tokens[userID]
	s.mu.RUnlock()

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
	invalidTokens := make([]string, 0)
	for i, r := range resp.Responses {
		if !r.Success && (messaging.IsUnregistered(r.Error) || messaging.IsInvalidArgument(r.Error)) {
			invalidTokens = append(invalidTokens, tokens[i])
		}
	}
	if len(invalidTokens) > 0 {
		s.mu.Lock()
		for _, t := range invalidTokens {
			s.RemoveToken(userID, t)
		}
		s.mu.Unlock()
	}

	return resp.SuccessCount, nil
}
