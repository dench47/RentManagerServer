package service

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
)

type CallCheckService struct {
	APIID string // sms.ru api_id
}

func NewCallCheckService(apiID string) *CallCheckService {
	return &CallCheckService{APIID: apiID}
}

type CallCheckResult struct {
	Status          string `json:"status"`
	CheckID         string `json:"check_id"`
	CallPhone       string `json:"call_phone"`
	CallPhonePretty string `json:"call_phone_pretty"`
}

func (s *CallCheckService) AddCallCheck(phone string) (CallCheckResult, error) {
	result := CallCheckResult{}
	cleanPhone := strings.TrimPrefix(phone, "+")
	resp, err := http.Get("https://sms.ru/callcheck/add?api_id=" + s.APIID + "&phone=" + url.QueryEscape(cleanPhone) + "&json=1")
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, err
	}
	log.Printf("[CALLCheck] add %s -> status=%s check_id=%s call_phone=%s", cleanPhone, result.Status, result.CheckID, result.CallPhone)
	return result, nil
}

type CallCheckStatus struct {
	Status          string `json:"status"`
	CheckStatus     int    `json:"check_status"`
	CheckStatusText string `json:"check_status_text"`
}

// CheckStatus returns true if verified (check_status == 401)
func (s *CallCheckService) CheckStatus(checkID string) (bool, error) {
	resp, err := http.Get("https://sms.ru/callcheck/status?api_id=" + s.APIID + "&check_id=" + checkID + "&json=1")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var result CallCheckStatus
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	log.Printf("[CALLCheck] status %s -> check_status=%d text=%s", checkID, result.CheckStatus, result.CheckStatusText)
	return result.CheckStatus == 401, nil
}
