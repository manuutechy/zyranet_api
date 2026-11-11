package services

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
)

// Cyzora Pay is a second way to send customers their M-Pesa prompt. Zyra Net
// has one Cyzora merchant account (platform level): its secret key starts
// charges and its webhook secret signs the results Cyzora posts back.
//
// API (verified against Cyzora's source):
//
//	POST /charges         Bearer <secret key>, Idempotency-Key header,
//	                      {"amount": cents, "phone", "account_ref", "metadata"}
//	                      200 → charge, 202 → {"charge", "warning"} (prompt may
//	                      be on the phone: outcome unknown, NOT failed)
//	GET  /charges/{id}    → charge with its current status
//	GET  /charges         → list (used only to check the key works)
//
// Webhooks are raw JSON signed with X-Cyzora-Signature = hex HMAC-SHA256 of
// the body under the endpoint's webhook secret.

// Setting keys (global settings table, platform-only — never shown to ISPs).
const (
	CyzoraSecretKeySetting     = "cyzora_secret_key"
	CyzoraWebhookSecretSetting = "cyzora_webhook_secret"
)

// CyzoraWebhookURL is where Cyzora must post charge events. It is registered
// once in the Cyzora dashboard (Cyzora has no per-charge callback URL).
const CyzoraWebhookURL = "https://api.zyranet.co.ke/api/v1/payments/cyzora/webhook"

const defaultCyzoraAPIBase = "https://api.cyzora.co.ke/api/v1"

// CyzoraCharge is the part of Cyzora's charge object we use.
type CyzoraCharge struct {
	ID           string            `json:"id"`
	Reference    string            `json:"reference"`
	Status       string            `json:"status"` // awaiting_payment | pending | succeeded | failed | timeout | expired
	Amount       int64             `json:"amount"` // cents
	Currency     string            `json:"currency"`
	MpesaReceipt *string           `json:"mpesa_receipt"`
	ResultCode   *string           `json:"result_code"`
	Livemode     *bool             `json:"livemode"`
	Metadata     map[string]string `json:"metadata"`
}

// CyzoraService talks to the Cyzora API.
type CyzoraService struct {
	httpClient      *http.Client
	baseURLOverride string
}

// NewCyzoraService builds a client with a sane timeout.
func NewCyzoraService() *CyzoraService {
	return &CyzoraService{httpClient: &http.Client{Timeout: 30 * time.Second}}
}

// UseBaseURL points every call at another host. Tests only.
func (c *CyzoraService) UseBaseURL(u string) { c.baseURLOverride = strings.TrimRight(u, "/") }

func (c *CyzoraService) baseURL() string {
	if c.baseURLOverride != "" {
		return c.baseURLOverride
	}
	if b := strings.TrimRight(strings.TrimSpace(config.Config.CyzoraAPIBase), "/"); b != "" {
		return b
	}
	return defaultCyzoraAPIBase
}

// cyzoraSetting reads a Cyzora key from the (cached) settings, then the env.
func cyzoraSetting(key, envFallback string) string {
	m, ok := cachedSettings()
	if !ok {
		m = (&MpesaService{}).queryMpesaSettingsMap()
		storeSettings(m)
	}
	return getSettingFromMap(m, key, envFallback)
}

// CyzoraSecretKey is the platform's Cyzora secret key ("" = not set).
func CyzoraSecretKey() string {
	return cyzoraSetting(CyzoraSecretKeySetting, config.Config.CyzoraSecretKey)
}

// CyzoraWebhookSecret is the secret Cyzora signs webhooks with ("" = not set).
func CyzoraWebhookSecret() string {
	return cyzoraSetting(CyzoraWebhookSecretSetting, config.Config.CyzoraWebhookSecret)
}

// CyzoraConfigured reports whether charges can be started.
func CyzoraConfigured() bool { return CyzoraSecretKey() != "" }

// CyzoraKeyMode reads the mode from a Cyzora secret key: "test" for sk_test_
// (sandbox — simulated, no money moves), "live" for sk_live_ and for the
// older bare 64-hex keys (issued before test keys existed), "" otherwise.
func CyzoraKeyMode(key string) string {
	key = strings.TrimSpace(key)
	switch {
	case strings.HasPrefix(key, "sk_test_"):
		return "test"
	case strings.HasPrefix(key, "sk_live_"):
		return "live"
	case len(key) == 64 && isHex(key):
		return "live"
	}
	return ""
}

// ValidCyzoraSecretKey reports whether key looks like a Cyzora secret key.
func ValidCyzoraSecretKey(key string) bool {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "sk_live_") || strings.HasPrefix(key, "sk_test_") {
		return len(key) == 72 && isHex(key[8:])
	}
	return len(key) == 64 && isHex(key)
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// VerifyCyzoraSignature checks X-Cyzora-Signature (hex HMAC-SHA256 of the raw
// body) in constant time. An empty secret or signature never verifies.
func VerifyCyzoraSignature(body []byte, signature, secret string) bool {
	signature = strings.ToLower(strings.TrimSpace(signature))
	if secret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(signature), []byte(want)) == 1
}

// SignCyzoraBody is the signature Cyzora would send. Tests only (exported for
// the handler tests).
func SignCyzoraBody(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// CyzoraError is a failed Cyzora call.
type CyzoraError struct {
	Status int    // HTTP status, 0 if no response arrived
	Msg    string // Cyzora's "error" text or the transport error
	// NotSent: we know no prompt went to the phone (Cyzora answered with an
	// error, or the connection was never made).
	NotSent bool
}

func (e *CyzoraError) Error() string {
	if e.Status == 0 {
		return "cyzora: " + e.Msg
	}
	return fmt.Sprintf("cyzora %d: %s", e.Status, e.Msg)
}

func (c *CyzoraService) do(method, path, key, idemKey string, body interface{}) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL()+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

func cyzoraErrMsg(status int, b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if status == http.StatusForbidden {
		return "the Cyzora account is not verified or is suspended"
	}
	return strings.TrimSpace(string(b))
}

// errCyzoraNoKey: no secret key saved.
var errCyzoraNoKey = errors.New("cyzora is not set up (no secret key)")

// CreateCharge starts an STK prompt. accepted is true when Cyzora answered
// 202: the prompt may be on the phone but the outcome is unknown — the charge
// stays pending and its webhook (or GET /charges/{id}) says how it ends.
// charge can be nil on a 202 that carried none.
//
// idemKey makes the call safe to repeat: a network error is retried once with
// the same key, and Cyzora replays its first answer instead of prompting again.
func (c *CyzoraService) CreateCharge(idemKey string, amountCents int64, phone, accountRef string, metadata map[string]string) (charge *CyzoraCharge, accepted bool, err error) {
	key := CyzoraSecretKey()
	if key == "" {
		return nil, false, &CyzoraError{Msg: errCyzoraNoKey.Error(), NotSent: true}
	}
	payload := map[string]interface{}{
		"amount":      amountCents,
		"phone":       phone,
		"account_ref": accountRef,
		"metadata":    metadata,
	}
	var status int
	var body []byte
	for attempt := 1; attempt <= 2; attempt++ {
		status, body, err = c.do(http.MethodPost, "/charges", key, idemKey, payload)
		if err == nil {
			break
		}
		if attempt == 1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err != nil {
		// No answer: we can't know whether the prompt went out.
		return nil, false, &CyzoraError{Msg: err.Error(), NotSent: isDialError(err)}
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		var ch CyzoraCharge
		if err := json.Unmarshal(body, &ch); err != nil || ch.ID == "" {
			return nil, false, &CyzoraError{Status: status, Msg: "unreadable charge in Cyzora's answer"}
		}
		return &ch, false, nil
	case http.StatusAccepted:
		var wrap struct {
			Charge  *CyzoraCharge `json:"charge"`
			Warning string        `json:"warning"`
		}
		_ = json.Unmarshal(body, &wrap)
		if wrap.Charge != nil && wrap.Charge.ID == "" {
			wrap.Charge = nil
		}
		return wrap.Charge, true, nil
	case http.StatusConflict:
		// "prompt may be on phone" without a charge to follow.
		return nil, false, &CyzoraError{Status: status, Msg: cyzoraErrMsg(status, body)}
	}
	return nil, false, &CyzoraError{Status: status, Msg: cyzoraErrMsg(status, body), NotSent: true}
}

// GetCharge reads a charge's current status (Cyzora refreshes it first).
func (c *CyzoraService) GetCharge(id string) (*CyzoraCharge, error) {
	key := CyzoraSecretKey()
	if key == "" {
		return nil, errCyzoraNoKey
	}
	status, body, err := c.do(http.MethodGet, "/charges/"+url.PathEscape(id), key, "", nil)
	if err != nil {
		return nil, &CyzoraError{Msg: err.Error()}
	}
	if status != http.StatusOK {
		return nil, &CyzoraError{Status: status, Msg: cyzoraErrMsg(status, body)}
	}
	var ch CyzoraCharge
	if err := json.Unmarshal(body, &ch); err != nil || ch.ID == "" {
		return nil, &CyzoraError{Status: status, Msg: "unreadable charge in Cyzora's answer"}
	}
	return &ch, nil
}

// TestKey checks the saved key with a cheap authenticated read (today's
// succeeded charges). No money moves and no prompt is sent.
func (c *CyzoraService) TestKey() (bool, string) {
	key := CyzoraSecretKey()
	if key == "" {
		return false, "No Cyzora secret key is saved yet."
	}
	today := time.Now().In(nairobi()).Format("2006-01-02")
	status, body, err := c.do(http.MethodGet, "/charges?status=succeeded&from_date="+today, key, "", nil)
	if err != nil {
		return false, "Could not reach Cyzora: " + err.Error()
	}
	switch status {
	case http.StatusOK:
		mode := CyzoraKeyMode(key)
		if mode == "test" {
			return true, "The Cyzora key works (test key: prompts are simulated, no money moves)."
		}
		return true, "The Cyzora key works."
	case http.StatusUnauthorized:
		return false, "Cyzora rejected the key. Check the secret key in the Cyzora dashboard."
	case http.StatusForbidden:
		return false, "Cyzora refused the key: " + cyzoraErrMsg(status, body)
	}
	return false, fmt.Sprintf("Cyzora answered %d: %s", status, cyzoraErrMsg(status, body))
}

func nairobi() *time.Location {
	if loc, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*60*60)
}
