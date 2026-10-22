package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// SmsService sends SMS via the configured provider.
//
// Provider credentials come only from the settings table (platform SMS page)
// or the environment (HOSTPINNACLE_*, MOBILESASA_*) — never from source code.
// If the primary provider fails, the message is retried through the
// platform's HostPinnacle account, then MobileSasa if one is configured.
type SmsService struct{}

// NewSmsService constructs an SmsService.
func NewSmsService() *SmsService { return &SmsService{} }

// hostPinnacleCreds is one HostPinnacle account.
type hostPinnacleCreds struct {
	BaseURL, APIKey, Username, SenderID string
}

func (h hostPinnacleCreds) configured() bool { return h.APIKey != "" && h.Username != "" }

// smsCreds is the resolved set of SMS credentials to use for one outgoing
// SMS, from either the platform-wide defaults or a tenant's own account.
type smsCreds struct {
	Provider           string
	HostPinnacle       hostPinnacleCreds
	MobilesasaBaseURL  string
	MobilesasaToken    string
	MobilesasaSenderID string
	BrevoAPIKey        string
	BrevoSenderID      string
}

// platformHostPinnacle is Zyra Net's own HostPinnacle account — the failover
// for every other provider.
func (s *SmsService) platformHostPinnacle() hostPinnacleCreds {
	base := s.GetSetting("hostpinnacle_base_url", config.Config.HostpinnacleBaseURL)
	if base == "" {
		base = "https://smsportal.hostpinnacle.co.ke/SMSApi/send"
	}
	return hostPinnacleCreds{
		BaseURL:  base,
		APIKey:   s.GetSetting("hostpinnacle_api_key", config.Config.HostpinnacleApiKey),
		Username: s.GetSetting("hostpinnacle_username", config.Config.HostpinnacleUsername),
		SenderID: s.GetSetting("hostpinnacle_sender_id", config.Config.HostpinnacleSenderID),
	}
}

// resolveSmsCreds returns the SMS credentials to use for a send
// tied to organizationID, mirroring resolveMpesaCreds in services/mpesa.go.
func (s *SmsService) resolveSmsCreds(organizationID uint) smsCreds {
	hp := s.platformHostPinnacle()
	msToken := s.GetSetting("mobilesasa_api_token", config.Config.MobilesasaAPIToken)

	provider := strings.ToLower(s.GetSetting("sms_provider", "hostpinnacle"))
	// HostPinnacle is preferred whenever it's configured; otherwise MobileSasa.
	if hp.configured() {
		provider = "hostpinnacle"
	} else if msToken != "" {
		provider = "mobilesasa"
	}

	creds := smsCreds{
		Provider:           provider,
		HostPinnacle:       hp,
		MobilesasaBaseURL:  s.GetSetting("mobilesasa_base_url", config.Config.MobilesasaBaseURL),
		MobilesasaToken:    msToken,
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
		creds.HostPinnacle.BaseURL = cfg.HostpinnacleBaseURL
	}
	if cfg.HostpinnacleAPIKey != "" {
		creds.HostPinnacle.APIKey = cfg.HostpinnacleAPIKey
	}
	if cfg.HostpinnacleUsername != "" {
		creds.HostPinnacle.Username = cfg.HostpinnacleUsername
	}
	if cfg.HostpinnacleSenderID != "" {
		creds.HostPinnacle.SenderID = cfg.HostpinnacleSenderID
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

// Send sends an SMS via the organization's provider (HostPinnacle,
// MobileSasa or Brevo), failing over to the platform's HostPinnacle and then
// MobileSasa, and saves a log record.
func (s *SmsService) Send(organizationID uint, phone, message string) (*models.SmsLog, error) {
	phone = utils.FormatPhone(phone) // E.g. 254712345678

	creds := s.resolveSmsCreds(organizationID)
	var status, providerResponse string
	switch {
	case creds.Provider == "mobilesasa":
		status, providerResponse = s.sendMobileSasa(creds, phone, message)
	case creds.Provider == "brevo" || (creds.BrevoAPIKey != "" && creds.HostPinnacle.APIKey == "" && creds.MobilesasaToken == ""):
		status, providerResponse = s.sendBrevo(creds, phone, message)
	default:
		status, providerResponse = s.sendHostPinnacle(creds.HostPinnacle, phone, message)
	}

	// Failover 1: the platform's HostPinnacle account, unless that's exactly
	// what just failed.
	if status != "sent" {
		platformHP := s.platformHostPinnacle()
		triedPlatformHP := creds.Provider == "hostpinnacle" && creds.HostPinnacle == platformHP
		if platformHP.configured() && !triedPlatformHP {
			log.Printf("[SMS] Primary provider %s failed (%s). Failing over to HostPinnacle...", creds.Provider, truncate(providerResponse, 200))
			status, providerResponse = s.sendHostPinnacle(platformHP, phone, message)
		}
	}
	// Failover 2: MobileSasa, if configured and not already the primary.
	if status != "sent" && creds.MobilesasaToken != "" && creds.Provider != "mobilesasa" {
		log.Printf("[SMS] HostPinnacle failed (%s). Failing over to MobileSasa...", truncate(providerResponse, 200))
		status, providerResponse = s.sendMobileSasa(creds, phone, message)
	}

	var logOrg *uint
	if organizationID != 0 {
		logOrg = &organizationID
	}
	logEntry := &models.SmsLog{
		OrganizationID:   logOrg,
		Phone:            phone,
		Message:          message,
		Status:           status,
		ProviderResponse: &providerResponse,
	}
	if err := config.DB.Create(logEntry).Error; err != nil {
		log.Printf("[SMS] Failed to save sms_log: %v", err)
	}

	if status != "sent" {
		return logEntry, fmt.Errorf("SMS failed: %s", providerResponse)
	}
	return logEntry, nil
}

// notConfigured is what a send with missing credentials returns: a pretend
// success in local/dev (so flows can be exercised without a provider), a
// failure anywhere else (so failover kicks in and nobody believes an SMS went
// out when it didn't).
func notConfigured(provider string, phone, message string) (string, string) {
	b, _ := json.Marshal(map[string]string{"status": "not_configured", "reason": "No credentials configured for " + provider})
	if config.IsDevEnv(config.Config.AppEnv) && !testing.Testing() {
		log.Printf("[SMS] %s mock (no credentials): to=%s msg=%s", provider, phone, message)
		return "sent", string(b)
	}
	return "failed", string(b)
}

// smsRequestAllowed stops a test binary from ever reaching a real SMS
// provider: under `go test` only loopback (httptest) servers may be called.
func smsRequestAllowed(rawURL string) bool {
	if !testing.Testing() {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func (s *SmsService) sendHostPinnacle(hp hostPinnacleCreds, phone, message string) (string, string) {
	if !hp.configured() {
		return notConfigured("HostPinnacle", phone, message)
	}
	if !smsRequestAllowed(hp.BaseURL) {
		return "failed", "blocked: real SMS provider called from a test"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("userid", hp.Username)
	_ = writer.WriteField("mobile", phone)
	_ = writer.WriteField("msg", message)
	_ = writer.WriteField("senderid", hp.SenderID)
	_ = writer.WriteField("sendMethod", "quick")
	_ = writer.WriteField("msgType", "text")
	_ = writer.WriteField("output", "json")
	_ = writer.WriteField("duplicatecheck", "true")
	_ = writer.Close()

	req, err := http.NewRequest(http.MethodPost, hp.BaseURL, &body)
	if err != nil {
		log.Printf("[SMS] Request creation error: %v", err)
		return "failed", err.Error()
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("apikey", hp.APIKey)
	req.Close = true

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[SMS] Hostpinnacle HTTP error: %v", err)
		return "failed", err.Error()
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	providerResponse := string(respBytes)

	if resp.StatusCode == http.StatusNoContent {
		return "sent", providerResponse
	}
	if resp.StatusCode == http.StatusOK {
		var parsed map[string]interface{}
		if json.Unmarshal(respBytes, &parsed) == nil {
			if st, _ := parsed["status"].(string); st == "success" {
				return "sent", providerResponse
			}
		}
		log.Printf("[SMS] Hostpinnacle error status=%d: %s", resp.StatusCode, providerResponse)
		return "failed", providerResponse
	}
	log.Printf("[SMS] Hostpinnacle HTTP error status=%d: %s", resp.StatusCode, providerResponse)
	return "failed", providerResponse
}

func (s *SmsService) sendBrevo(creds smsCreds, phone, message string) (string, string) {
	if creds.BrevoAPIKey == "" {
		return notConfigured("Brevo", phone, message)
	}
	const apiURL = "https://api.brevo.com/v3/transactionalSMS/sms"
	if !smsRequestAllowed(apiURL) {
		return "failed", "blocked: real SMS provider called from a test"
	}
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
	payloadBytes, _ := json.Marshal(map[string]interface{}{
		"sender":    sender,
		"recipient": recipient,
		"content":   message,
		"type":      "transactional",
	})
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return "failed", err.Error()
	}
	req.Header.Set("api-key", creds.BrevoAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[SMS] Brevo request error: %v", err)
		return "failed", err.Error()
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		return "sent", string(respBytes)
	}
	log.Printf("[SMS] Brevo HTTP error status=%d: %s", resp.StatusCode, string(respBytes))
	return "failed", string(respBytes)
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

func (s *SmsService) sendMobileSasa(creds smsCreds, phone, message string) (string, string) {
	token := creds.MobilesasaToken
	if token == "" {
		return notConfigured("MobileSasa", phone, message)
	}
	apiURL := creds.MobilesasaBaseURL
	if apiURL == "" {
		apiURL = "https://api.mobilesasa.com/v1/send/message"
	}
	if !smsRequestAllowed(apiURL) {
		return "failed", "blocked: real SMS provider called from a test"
	}
	sender := creds.MobilesasaSenderID
	if sender == "" {
		sender = "MOBILESASA"
	}

	payload := map[string]string{
		"senderID": sender,
		"phone":    phone,
		"message":  message,
	}
	payloadBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return "failed", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Close = true

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[SMS] MobileSasa HTTP error: %v", err)
		return "failed", err.Error()
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	providerResponse := string(respBytes)

	var msResp struct {
		Status       bool   `json:"status"`
		ResponseCode string `json:"responseCode"`
		Message      string `json:"message"`
		MessageID    string `json:"messageId"`
	}
	if errUnmarshal := json.Unmarshal(respBytes, &msResp); errUnmarshal == nil {
		if msResp.Status || msResp.ResponseCode == "0200" {
			log.Printf("[SMS] MobileSasa SMS sent successfully to %s, msgId=%s", phone, msResp.MessageID)
			return "sent", providerResponse
		}
		log.Printf("[SMS] MobileSasa error code=%s: %s", msResp.ResponseCode, msResp.Message)
		return "failed", providerResponse
	}
	if resp.StatusCode == http.StatusOK {
		return "sent", providerResponse
	}
	return "failed", providerResponse
}
