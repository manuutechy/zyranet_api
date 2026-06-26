package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// SmsService sends SMS via the configured provider.
type SmsService struct{}

// NewSmsService constructs an SmsService.
func NewSmsService() *SmsService { return &SmsService{} }

// Send sends an SMS via HostPinnacle and saves a log record.
func (s *SmsService) Send(phone, message string) (*models.SmsLog, error) {
	phone = utils.FormatPhone(phone) // E.g. 254712345678

	status := "failed"
	providerResponse := ""

	apiURL := s.getSetting("hostpinnacle_base_url", config.Config.HostpinnacleBaseURL)
	apiKey := s.getSetting("hostpinnacle_api_key", config.Config.HostpinnacleApiKey)
	userID := s.getSetting("hostpinnacle_username", config.Config.HostpinnacleUsername)
	sender := s.getSetting("hostpinnacle_sender_id", config.Config.HostpinnacleSenderID)

	// Mock mode if required credentials are missing
	if apiKey == "" || userID == "" {
		status = "sent"
		mock := map[string]string{"status": "mock_success", "reason": "No credentials configured for Hostpinnacle"}
		b, _ := json.Marshal(mock)
		providerResponse = string(b)
		log.Printf("[SMS] Hostpinnacle Mock: to=%s msg=%s", phone, message)
	} else {
		// Per live testing against the real API:
		//   - Correct format: JSON body + "apikey" in the request HEADER
		//   - Success response: HTTP 204 No Content (empty body)
		//   - Error response: JSON {"status":"error","statusCode":"...","reason":"..."}
		//   - Wrong format returns: status=error | errorCode=152 | reason=Invalid method
		payload := map[string]string{
			"userid":    userID,
			"mobile":    phone,
			"msg":       message,
			"senderid":  sender,
			"msgType":   "text",
			"duplicate": "1",
		}
		payloadBytes, _ := json.Marshal(payload)

		req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("apikey", apiKey) // API key goes in the header

			client := &http.Client{}
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				providerResponse = string(body)

				// HTTP 204 = success (empty body — no content)
				// HTTP 200 with JSON error = failure
				if resp.StatusCode == http.StatusNoContent {
					status = "sent"
					providerResponse = `{"status":"success","http":204}`
				} else {
					log.Printf("[SMS] Hostpinnacle error status=%d: %s", resp.StatusCode, providerResponse)
				}
			} else {
				providerResponse = err.Error()
				log.Printf("[SMS] Hostpinnacle HTTP error: %v", err)
			}
		} else {
			providerResponse = err.Error()
			log.Printf("[SMS] Request creation error: %v", err)
		}
	}

	logEntry := &models.SmsLog{
		Phone:            phone,
		Message:          message,
		Status:           status,
		ProviderResponse: &providerResponse,
	}
	if err := config.DB.Create(logEntry).Error; err != nil {
		log.Printf("[SMS] Failed to save sms_log: %v", err)
	}

	if status == "failed" {
		return logEntry, fmt.Errorf("SMS failed: %s", providerResponse)
	}
	return logEntry, nil
}

// getSetting returns value from settings table or falls back to default.
func (s *SmsService) getSetting(key, defaultVal string) string {
	var setting models.Setting
	if err := config.DB.Where("`key` = ?", key).First(&setting).Error; err == nil && setting.Value != nil {
		v := strings.TrimSpace(*setting.Value)
		if v != "" {
			return v
		}
	}
	return strings.TrimSpace(defaultVal)
}
