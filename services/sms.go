package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
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

	apiURL := s.GetSetting("hostpinnacle_base_url", config.Config.HostpinnacleBaseURL)
	apiKey := s.GetSetting("hostpinnacle_api_key", config.Config.HostpinnacleApiKey)
	userID := s.GetSetting("hostpinnacle_username", config.Config.HostpinnacleUsername)
	sender := s.GetSetting("hostpinnacle_sender_id", config.Config.HostpinnacleSenderID)

	// Mock mode if required credentials are missing
	if apiKey == "" || userID == "" {
		status = "sent"
		mock := map[string]string{"status": "mock_success", "reason": "No credentials configured for Hostpinnacle"}
		b, _ := json.Marshal(mock)
		providerResponse = string(b)
		log.Printf("[SMS] Hostpinnacle Mock: to=%s msg=%s", phone, message)
	} else {
		// Per HostPinnacle API documentation:
		//   - Method: POST
		//   - Format: multipart/form-data
		//   - Headers: "apikey" contains the API Key
		//   - Success Response: JSON {"status":"success", ...} under HTTP 200
		//   - Error Response: JSON {"status":"error", "reason": "...", ...}
		
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("userid", userID)
		_ = writer.WriteField("mobile", phone)
		_ = writer.WriteField("msg", message)
		_ = writer.WriteField("senderid", sender)
		_ = writer.WriteField("sendMethod", "quick")
		_ = writer.WriteField("msgType", "text")
		_ = writer.WriteField("output", "json")
		_ = writer.WriteField("duplicatecheck", "true")
		_ = writer.Close()

		req, err := http.NewRequest(http.MethodPost, apiURL, &body)
		if err == nil {
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Accept", "application/json")
			req.Header.Set("apikey", apiKey) // API key goes in the header
			req.Close = true

			client := &http.Client{}
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				respBytes, _ := io.ReadAll(resp.Body)
				providerResponse = string(respBytes)

				if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
					// Parse response to check for status=success
					var hpResp struct {
						Status     string `json:"status"`
						Reason     string `json:"reason"`
						StatusCode string `json:"statusCode"`
					}
					// Handle JSON response
					if len(respBytes) > 0 {
						var parsed map[string]interface{}
						if errUnmarshal := json.Unmarshal(respBytes, &parsed); errUnmarshal == nil {
							if statusVal, ok := parsed["status"].(string); ok {
								hpResp.Status = statusVal
							}
							if reasonVal, ok := parsed["reason"].(string); ok {
								hpResp.Reason = reasonVal
							}
							if codeVal, ok := parsed["statusCode"].(string); ok {
								hpResp.StatusCode = codeVal
							}
						}
					}
					
					// HostPinnacle returns "success" status for successful SMS delivery
					if hpResp.Status == "success" || resp.StatusCode == http.StatusNoContent {
						status = "sent"
					} else {
						log.Printf("[SMS] Hostpinnacle error status=%d: %s", resp.StatusCode, providerResponse)
					}
				} else {
					log.Printf("[SMS] Hostpinnacle HTTP error status=%d: %s", resp.StatusCode, providerResponse)
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

// GetSetting returns value from settings table or falls back to default.
func (s *SmsService) GetSetting(key, defaultVal string) string {
	var setting models.Setting
	if err := config.DB.Where("`key` = ?", key).First(&setting).Error; err == nil && setting.Value != nil {
		v := strings.TrimSpace(*setting.Value)
		if v != "" {
			return v
		}
	}
	return strings.TrimSpace(defaultVal)
}
