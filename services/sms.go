package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// SmsService sends SMS via Africa's Talking.
type SmsService struct{}

// NewSmsService constructs an SmsService.
func NewSmsService() *SmsService { return &SmsService{} }

// Send sends an SMS and saves a log record.
func (s *SmsService) Send(phone, message string) (*models.SmsLog, error) {
	provider := strings.ToLower(s.getSetting("sms_provider", config.Config.SmsProvider))

	status := "failed"
	providerResponse := ""

	if provider == "hostpinnacle" {
		phone = utils.FormatPhone(phone) // E.g. 254712345678

		apiURL := s.getSetting("hostpinnacle_base_url", config.Config.HostpinnacleBaseURL)
		apiKey := s.getSetting("hostpinnacle_api_key", config.Config.HostpinnacleApiKey)
		username := s.getSetting("hostpinnacle_username", config.Config.HostpinnacleUsername)
		password := s.getSetting("hostpinnacle_password", config.Config.HostpinnaclePassword)
		sender := s.getSetting("hostpinnacle_sender_id", config.Config.HostpinnacleSenderID)

		// Mock mode if no credentials are set
		if apiKey == "" && username == "" && password == "" {
			status = "sent"
			mock := map[string]string{"status": "mock_success", "reason": "No credentials configured for Hostpinnacle"}
			b, _ := json.Marshal(mock)
			providerResponse = string(b)
			log.Printf("[SMS] Hostpinnacle Mock: to=%s msg=%s", phone, message)
		} else {
			formData := url.Values{}
			formData.Set("apikey", apiKey)
			formData.Set("api_key", apiKey)
			formData.Set("userid", username)
			formData.Set("username", username)
			formData.Set("password", password)
			formData.Set("mobile", phone)
			formData.Set("phone", phone)
			formData.Set("to", phone)
			formData.Set("msg", message)
			formData.Set("message", message)
			formData.Set("senderid", sender)
			formData.Set("sender_id", sender)
			formData.Set("msgType", "text")
			formData.Set("duplicate", "true")

			req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(formData.Encode()))
			if err == nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Accept", "application/json")

				client := &http.Client{}
				resp, err := client.Do(req)
				if err == nil {
					defer resp.Body.Close()
					body, _ := io.ReadAll(resp.Body)
					providerResponse = string(body)
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						status = "sent"
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
	} else {
		// Africa's Talking (default)
		phone = utils.FormatPhoneE164(phone)

		username := s.getSetting("africastalking_username", config.Config.ATUsername)
		apiKey := s.getSetting("africastalking_api_key", config.Config.ATApiKey)
		sender := s.getSetting("africastalking_sender", config.Config.ATSenderID)

		isSandbox := strings.ToLower(username) == "sandbox"
		apiURL := "https://api.africastalking.com/version1/messaging"
		if isSandbox {
			apiURL = "https://api.sandbox.africastalking.com/version1/messaging"
		}

		if apiKey != "" && apiKey != "mock_api_key" {
			formData := url.Values{}
			formData.Set("username", username)
			formData.Set("to", phone)
			formData.Set("message", message)
			if !isSandbox {
				formData.Set("from", sender)
			}

			req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(formData.Encode()))
			if err == nil {
				req.Header.Set("apiKey", apiKey)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Accept", "application/json")

				client := &http.Client{}
				resp, err := client.Do(req)
				if err == nil {
					defer resp.Body.Close()
					body, _ := io.ReadAll(resp.Body)
					providerResponse = string(body)
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						status = "sent"
					} else {
						log.Printf("[SMS] Africa's Talking error: %s", providerResponse)
					}
				} else {
					providerResponse = err.Error()
					log.Printf("[SMS] HTTP error: %v", err)
				}
			} else {
				providerResponse = err.Error()
				log.Printf("[SMS] HTTP error: %v", err)
			}
		} else {
			// Mock mode
			status = "sent"
			mock := map[string]string{"status": "mock_success", "reason": "No credentials configured"}
			b, _ := json.Marshal(mock)
			providerResponse = string(b)
			log.Printf("[SMS] Mock: to=%s msg=%s", phone, message)
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
