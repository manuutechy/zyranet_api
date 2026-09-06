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

// smsCreds is the resolved set of Hostpinnacle credentials to use for one
// outgoing SMS, from either the platform-wide defaults or a tenant's own
// configured Hostpinnacle account.
type smsCreds struct {
	Provider           string
	BaseURL            string
	APIKey             string
	Username           string
	SenderID           string
	MobilesasaBaseURL  string
	MobilesasaToken    string
	MobilesasaSenderID string
	BrevoAPIKey        string
	BrevoSenderID      string
}

// resolveSmsCreds returns the SMS credentials to use for a send
// tied to organizationID, mirroring resolveMpesaCreds in services/mpesa.go.
func (s *SmsService) resolveSmsCreds(organizationID uint) smsCreds {
	provider := strings.ToLower(s.GetSetting("sms_provider", "hostpinnacle"))
	creds := smsCreds{
		Provider:           provider,
		BaseURL:            s.GetSetting("hostpinnacle_base_url", config.Config.HostpinnacleBaseURL),
		APIKey:             s.GetSetting("hostpinnacle_api_key", config.Config.HostpinnacleApiKey),
		Username:           s.GetSetting("hostpinnacle_username", config.Config.HostpinnacleUsername),
		SenderID:           s.GetSetting("hostpinnacle_sender_id", config.Config.HostpinnacleSenderID),
		MobilesasaBaseURL:  s.GetSetting("mobilesasa_base_url", config.Config.MobilesasaBaseURL),
		MobilesasaToken:    s.GetSetting("mobilesasa_api_token", config.Config.MobilesasaAPIToken),
		MobilesasaSenderID: s.GetSetting("mobilesasa_sender_id", config.Config.MobilesasaSenderID),
		BrevoAPIKey:        s.GetSetting("brevo_api_key", ""),
		BrevoSenderID:      s.GetSetting("brevo_sender_id", "ZyraNet"),
	}
	if organizationID == 0 {
		return creds
	}

	var cfg models.OrganizationSmsConfig
	if err := config.DB.Where("organization_id = ? AND mode = ?", organizationID, "own").First(&cfg).Error; err != nil {
		return creds
	}

	if cfg.Provider != "" {
		creds.Provider = cfg.Provider
	}
	if cfg.HostpinnacleBaseURL != "" {
		creds.BaseURL = cfg.HostpinnacleBaseURL
	}
	if cfg.HostpinnacleAPIKey != "" {
		creds.APIKey = cfg.HostpinnacleAPIKey
	}
	if cfg.HostpinnacleUsername != "" {
		creds.Username = cfg.HostpinnacleUsername
	}
	if cfg.HostpinnacleSenderID != "" {
		creds.SenderID = cfg.HostpinnacleSenderID
	}
	if cfg.MobilesasaBaseURL != "" {
		creds.MobilesasaBaseURL = cfg.MobilesasaBaseURL
	}
	if cfg.MobilesasaAPIToken != "" {
		creds.MobilesasaToken = cfg.MobilesasaAPIToken
	}
	if cfg.MobilesasaSenderID != "" {
		creds.MobilesasaSenderID = cfg.MobilesasaSenderID
	}
	return creds
}

// SendForZone is a convenience wrapper over Send for callers that have a
// zoneID (payment/customer context) handy rather than an organizationID —
// it resolves the zone's Organization first, then sends on its behalf. A
// zoneID of 0, or a zone that no longer exists, falls back to
// organizationID 0 (platform-wide shared credentials), matching
// resolveSmsCreds' behavior for organizationID 0.
func (s *SmsService) SendForZone(zoneID uint, phone, message string) (*models.SmsLog, error) {
	var organizationID uint
	if zoneID != 0 {
		var zone models.Zone
		if err := config.DB.Select("organization_id").First(&zone, zoneID).Error; err == nil {
			organizationID = zone.OrganizationID
		}
	}
	return s.Send(organizationID, phone, message)
}

// Send sends an SMS via MobileSasa, HostPinnacle, or Brevo and saves a log record.
func (s *SmsService) Send(organizationID uint, phone, message string) (*models.SmsLog, error) {
	phone = utils.FormatPhone(phone) // E.g. 254712345678

	status := "failed"
	providerResponse := ""

	creds := s.resolveSmsCreds(organizationID)

	if creds.Provider == "mobilesasa" {
		apiURL := creds.MobilesasaBaseURL
		if apiURL == "" {
			apiURL = "https://api.mobilesasa.com/v1/send/message"
		}
		token := creds.MobilesasaToken
		sender := creds.MobilesasaSenderID
		if sender == "" {
			sender = "MOBILESASA"
		}

		if token == "" {
			status = "sent"
			mock := map[string]string{"status": "mock_success", "reason": "No API token configured for MobileSasa"}
			b, _ := json.Marshal(mock)
			providerResponse = string(b)
			log.Printf("[SMS] MobileSasa Mock: to=%s msg=%s", phone, message)
		} else {
			payload := map[string]string{
				"senderID": sender,
				"phone":    phone,
				"message":  message,
			}
			payloadBytes, _ := json.Marshal(payload)
			req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewBuffer(payloadBytes))
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json")
				req.Close = true

				client := &http.Client{}
				resp, err := client.Do(req)
				if err == nil {
					defer resp.Body.Close()
					respBytes, _ := io.ReadAll(resp.Body)
					providerResponse = string(respBytes)

					var msResp struct {
						Status       bool   `json:"status"`
						ResponseCode string `json:"responseCode"`
						Message      string `json:"message"`
						MessageID    string `json:"messageId"`
					}
					if errUnmarshal := json.Unmarshal(respBytes, &msResp); errUnmarshal == nil {
						if msResp.Status || msResp.ResponseCode == "0200" {
							status = "sent"
						} else {
							log.Printf("[SMS] MobileSasa error code=%s: %s", msResp.ResponseCode, msResp.Message)
						}
					} else if resp.StatusCode == http.StatusOK {
						status = "sent"
					} else {
						log.Printf("[SMS] MobileSasa HTTP error status=%d: %s", resp.StatusCode, providerResponse)
					}
				} else {
					providerResponse = err.Error()
					log.Printf("[SMS] MobileSasa request error: %v", err)
				}
			} else {
				providerResponse = err.Error()
				log.Printf("[SMS] Request creation error: %v", err)
			}
		}
	} else if creds.Provider == "brevo" || (creds.BrevoAPIKey != "" && creds.APIKey == "" && creds.MobilesasaToken == "") {
		if creds.BrevoAPIKey == "" {
			status = "sent"
			mock := map[string]string{"status": "mock_success", "reason": "No Brevo API key configured"}
			b, _ := json.Marshal(mock)
			providerResponse = string(b)
			log.Printf("[SMS] Brevo Mock: to=%s msg=%s", phone, message)
		} else {
			recipient := phone
			if !strings.HasPrefix(recipient, "+") {
				recipient = "+" + recipient
			}
			sender := creds.BrevoSenderID
			if sender == "" {
				sender = "ZyraNet"
			}
			if len(sender) > 11 {
				sender = sender[:11]
			}

			payload := map[string]interface{}{
				"sender":    sender,
				"recipient": recipient,
				"content":   message,
				"type":      "transactional",
			}
			payloadBytes, _ := json.Marshal(payload)
			req, err := http.NewRequest(http.MethodPost, "https://api.brevo.com/v3/transactionalSMS/sms", bytes.NewBuffer(payloadBytes))
			if err == nil {
				req.Header.Set("api-key", creds.BrevoAPIKey)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json")

				client := &http.Client{}
				resp, err := client.Do(req)
				if err == nil {
					defer resp.Body.Close()
					respBytes, _ := io.ReadAll(resp.Body)
					providerResponse = string(respBytes)
					if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
						status = "sent"
					} else {
						log.Printf("[SMS] Brevo HTTP error status=%d: %s", resp.StatusCode, providerResponse)
					}
				} else {
					providerResponse = err.Error()
					log.Printf("[SMS] Brevo request error: %v", err)
				}
			} else {
				providerResponse = err.Error()
			}
		}
	} else {
		apiURL := creds.BaseURL
		apiKey := creds.APIKey
		userID := creds.Username
		sender := creds.SenderID

		// Mock mode if required credentials are missing
		if apiKey == "" || userID == "" {
			status = "sent"
			mock := map[string]string{"status": "mock_success", "reason": "No credentials configured for Hostpinnacle"}
			b, _ := json.Marshal(mock)
			providerResponse = string(b)
			log.Printf("[SMS] Hostpinnacle Mock: to=%s msg=%s", phone, message)
		} else {
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
				req.Header.Set("apikey", apiKey)
				req.Close = true

				client := &http.Client{}
				resp, err := client.Do(req)
				if err == nil {
					defer resp.Body.Close()
					respBytes, _ := io.ReadAll(resp.Body)
					providerResponse = string(respBytes)

					if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
						var hpResp struct {
							Status     string `json:"status"`
							Reason     string `json:"reason"`
							StatusCode string `json:"statusCode"`
						}
						if len(respBytes) > 0 {
							var parsed map[string]interface{}
							if errUnmarshal := json.Unmarshal(respBytes, &parsed); errUnmarshal == nil {
								if statusVal, ok := parsed["status"].(string); ok {
									hpResp.Status = statusVal
								}
							}
						}
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
