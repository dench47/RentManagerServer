package service

import (
	"log"
)

type SMSService struct{}

func NewSMSService() *SMSService {
	return &SMSService{}
}

// SendCode — заглушка. В будущем — реальная отправка SMS через API.
func (s *SMSService) SendCode(phone, code string) error {
	log.Printf("[SMS STUB] Sending code %s to phone %s", code, phone)
	return nil
}
