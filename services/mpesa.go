package services

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// MpesaService handles Safaricom Daraja STK Push payments.
type MpesaService struct {
	SMS      *SmsService
	Voucher  *VoucherService
	MikroTik *MikroTikService

	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

// NewMpesaService constructs an MpesaService.
func NewMpesaService(sms *SmsService, voucher *VoucherService, mikrotik *MikroTikService) *MpesaService {
	return &MpesaService{SMS: sms, Voucher: voucher, MikroTik: mikrotik}
}

// MpesaSTKResponse is the result of an STK push initiation.
type MpesaSTKResponse struct {
	Status              string `json:"status"`
	CheckoutRequestID   string `json:"checkout_request_id"`
	ResponseDescription string `json:"response_description"`
	IsMock              bool   `json:"is_mock"`
}

// getBaseURL returns the Daraja API base URL.
func (s *MpesaService) getBaseURL() string {
	env := s.getSetting("mpesa_environment", config.Config.MpesaEnv)
	if strings.ToLower(env) == "production" {
		return "https://api.safaricom.co.ke"
	}
	return "https://sandbox.safaricom.co.ke"
}

// GetAccessToken fetches the OAuth token from Daraja, caching it in-memory
// for its reported lifetime (Daraja tokens are valid ~1 hour) so we don't
// make a round trip to Safaricom on every STK push.
func (s *MpesaService) GetAccessToken() (string, error) {
	consumerKey := s.getSetting("mpesa_consumer_key", config.Config.MpesaConsumerKey)
	consumerSecret := s.getSetting("mpesa_consumer_secret", config.Config.MpesaConsumerSecret)

	if consumerKey == "" || consumerKey == "mock_consumer_key" {
		return "mock_token", nil
	}

	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()

	if s.cachedToken != "" && time.Now().Before(s.tokenExpiry) {
		return s.cachedToken, nil
	}

	apiURL := s.getBaseURL() + "/oauth/v1/generate?grant_type=client_credentials"
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	creds := base64.StdEncoding.EncodeToString([]byte(consumerKey + ":" + consumerSecret))
	req.Header.Set("Authorization", "Basic "+creds)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("daraja auth failed: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	token, ok := result["access_token"].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("no access_token in response")
	}

	expiresIn := 3500 * time.Second // safe default, just under Daraja's ~1h lifetime
	if raw, ok := result["expires_in"]; ok {
		switch v := raw.(type) {
		case string:
			if secs, err := time.ParseDuration(v + "s"); err == nil {
				expiresIn = secs - 60*time.Second
			}
		case float64:
			expiresIn = time.Duration(v)*time.Second - 60*time.Second
		}
	}
	if expiresIn <= 0 {
		expiresIn = 60 * time.Second
	}

	s.cachedToken = token
	s.tokenExpiry = time.Now().Add(expiresIn)
	return token, nil
}

// InitiateSTKPush sends a payment prompt to the customer's phone.
func (s *MpesaService) InitiateSTKPush(phone string, amount float64, reference, description string) (*MpesaSTKResponse, error) {
	phone = utils.FormatPhone(phone)

	shortcode := s.getSetting("mpesa_shortcode", config.Config.MpesaShortcode)
	passkey := s.getSetting("mpesa_passkey", config.Config.MpesaPasskey)
	callbackURL := s.getSetting("mpesa_callback_url", config.Config.MpesaCallbackURL)
	env := s.getSetting("mpesa_environment", config.Config.MpesaEnv)

	token, err := s.GetAccessToken()
	if err != nil {
		return nil, err
	}

	isLocalCallback := callbackURL == "" ||
		strings.Contains(callbackURL, "localhost") ||
		strings.Contains(callbackURL, "127.0.0.1") ||
		strings.Contains(callbackURL, "192.168.") ||
		!strings.HasPrefix(callbackURL, "https://")

	if strings.ToLower(env) != "production" && (token == "mock_token" || isLocalCallback) {
		checkoutID := fmt.Sprintf("ws_CO_%d_%d", rand.Intn(999999)+100000, time.Now().Unix())
		log.Printf("[M-Pesa] Mock STK Push: phone=%s amount=%.0f ref=%s", phone, amount, reference)
		return &MpesaSTKResponse{
			Status:              "success",
			CheckoutRequestID:   checkoutID,
			ResponseDescription: "Mock STK Push initiated successfully",
			IsMock:              true,
		}, nil
	}

	billingType := s.getSetting("mpesa_billing_type", "paybill")
	tillNumber := s.getSetting("mpesa_till_number", "")
	paybillNumber := s.getSetting("mpesa_paybill_number", "")
	paybillAccount := s.getSetting("mpesa_paybill_account", "")
	bankAccount := s.getSetting("mpesa_bank_account", "")

	transactionType := "CustomerPayBillOnline"
	partyB := paybillNumber
	if partyB == "" {
		partyB = shortcode
	}
	accountReference := paybillAccount
	if accountReference == "" {
		accountReference = reference
	}

	if billingType == "till" {
		transactionType = "CustomerBuyGoodsOnline"
		partyB = tillNumber
		accountReference = reference
	} else if billingType == "bank" {
		bankName := s.getSetting("mpesa_bank_name", "")
		partyB = bankPaybill(bankName)
		accountReference = bankAccount
		if accountReference == "" {
			accountReference = reference
		}
	}

	timestamp := time.Now().Format("20060102150405")
	password := base64.StdEncoding.EncodeToString([]byte(shortcode + passkey + timestamp))

	payload := map[string]interface{}{
		"BusinessShortCode": shortcode,
		"Password":          password,
		"Timestamp":         timestamp,
		"TransactionType":   transactionType,
		"Amount":            int(amount),
		"PartyA":            phone,
		"PartyB":            partyB,
		"PhoneNumber":       phone,
		"CallBackURL":       callbackURL,
		"AccountReference":  accountReference,
		"TransactionDesc":   description,
	}

	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, s.getBaseURL()+"/mpesa/stkpush/v1/processrequest", strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("STK push request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	if code, ok := result["ResponseCode"].(string); ok && code == "0" {
		checkoutID, _ := result["CheckoutRequestID"].(string)
		desc, _ := result["ResponseDescription"].(string)
		if checkoutID == "" {
			return nil, fmt.Errorf("daraja response missing CheckoutRequestID")
		}
		return &MpesaSTKResponse{
			Status:              "success",
			CheckoutRequestID:   checkoutID,
			ResponseDescription: desc,
			IsMock:              false,
		}, nil
	}

	log.Printf("[M-Pesa] STK Push initiation failed. HTTP Status: %d. Response: %s", resp.StatusCode, string(body))

	desc := "STK push initiation failed"
	if d, ok := result["ResponseDescription"].(string); ok && d != "" {
		desc = d
	} else if errMsg, ok := result["errorMessage"].(string); ok && errMsg != "" {
		desc = errMsg
	} else if errCode, ok := result["errorCode"].(string); ok && errCode != "" {
		desc = fmt.Sprintf("Daraja Error %s", errCode)
	}
	return nil, fmt.Errorf(desc)
}

// HandleCallback processes the async Daraja payment notification.
func (s *MpesaService) HandleCallback(payload map[string]interface{}) error {
	log.Printf("[M-Pesa] Callback received: %+v", payload)

	body, ok := payload["Body"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid callback structure: missing Body")
	}
	stkCallback, ok := body["stkCallback"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid callback structure: missing stkCallback")
	}

	checkoutRequestID, _ := stkCallback["CheckoutRequestID"].(string)
	resultCode := stkCallback["ResultCode"]

	var payment models.Payment
	if err := config.DB.Where("mpesa_transaction_id = ?", checkoutRequestID).First(&payment).Error; err != nil {
		return fmt.Errorf("payment with CheckoutRequestID %s not found", checkoutRequestID)
	}

	// Determine result code value
	var rc float64
	switch v := resultCode.(type) {
	case float64:
		rc = v
	case int:
		rc = float64(v)
	case int64:
		rc = float64(v)
	case string:
		var parsed float64
		if _, err := fmt.Sscanf(v, "%f", &parsed); err == nil {
			rc = parsed
		}
	}

	if rc != 0 {
		reason := "Transaction was rejected."
		if rd, ok := stkCallback["ResultDesc"].(string); ok {
			reason = rd
		}
		res := config.DB.Model(&models.Payment{}).
			Where("id = ? AND status = ?", payment.ID, "pending").
			Updates(map[string]interface{}{
				"status":        "failed",
				"status_reason": reason,
			})
		if res.RowsAffected == 0 {
			log.Printf("[M-Pesa] Duplicate/late failure callback for payment %d ignored (status already %s)", payment.ID, payment.Status)
			return nil
		}
		log.Printf("[M-Pesa] Payment failed: %s", reason)
		return nil
	}

	// Extract metadata
	var amount float64
	receiptNumber := ""
	phone := payment.Phone

	if meta, ok := stkCallback["CallbackMetadata"].(map[string]interface{}); ok {
		if items, ok := meta["Item"].([]interface{}); ok {
			for _, itemRaw := range items {
				item, _ := itemRaw.(map[string]interface{})
				name, _ := item["Name"].(string)
				val := item["Value"]
				if val == nil {
					continue
				}
				switch name {
				case "Amount":
					switch v := val.(type) {
					case float64:
						amount = v
					case int:
						amount = float64(v)
					case int64:
						amount = float64(v)
					}
				case "MpesaReceiptNumber":
					if v, ok := val.(string); ok {
						receiptNumber = v
					}
				case "PhoneNumber":
					switch v := val.(type) {
					case float64:
						phone = fmt.Sprintf("%.0f", v)
					case string:
						phone = v
					case int:
						phone = fmt.Sprintf("%d", v)
					case int64:
						phone = fmt.Sprintf("%d", v)
					}
				}
			}
		}
	}
	_ = amount

	res := config.DB.Model(&models.Payment{}).
		Where("id = ? AND status = ?", payment.ID, "pending").
		Updates(map[string]interface{}{
			"status":               "completed",
			"mpesa_receipt_number": receiptNumber,
		})
	if res.RowsAffected == 0 {
		log.Printf("[M-Pesa] Duplicate/late success callback for payment %d ignored (status already %s)", payment.ID, payment.Status)
		return nil
	}

	if payment.PackageID == nil {
		if payment.CustomerID != nil {
			var customer models.Customer
			if err := config.DB.First(&customer, *payment.CustomerID).Error; err == nil {
				newBalance := customer.CreditBalance + payment.Amount
				config.DB.Model(&customer).Update("credit_balance", newBalance)

				note := fmt.Sprintf("M-Pesa top-up (Receipt: %s)", receiptNumber)
				config.DB.Create(&models.CreditLog{
					CustomerID: customer.ID,
					Amount:     payment.Amount,
					Type:       "credit",
					Note:       &note,
				})

				msg := fmt.Sprintf("Hi %s, KES %.2f credited to your account. Your new balance is KES %.2f. Enjoy browsing!",
					customer.Name, payment.Amount, newBalance)
				go s.SMS.Send(phone, msg)
			}
		}
		return nil
	}

	var pkg models.Package
	if err := config.DB.First(&pkg, *payment.PackageID).Error; err != nil {
		log.Printf("[M-Pesa] Package %d not found for payment %d", *payment.PackageID, payment.ID)
		return nil
	}

	// Load voucher (if any) up front so it's available as a router-login fallback
	var voucher *models.Voucher
	if payment.VoucherID != nil {
		var v models.Voucher
		if err := config.DB.First(&v, *payment.VoucherID).Error; err == nil {
			config.DB.Model(&v).Update("status", "unused")
			voucher = &v
		}
	}

	// Load Zone to run MikroTik commands
	var zone models.Zone
	if err := config.DB.First(&zone, payment.ZoneID).Error; err == nil {
		if payment.MacAddress != "" {
			// Whitelist by MAC, with a fallback to pushing the voucher as a router
			// login if whitelisting ultimately fails — gives the customer a manual
			// way to get online even if the router call keeps failing.
			go func() {
				err := s.whitelistWithRetry(&zone, payment.MacAddress, &pkg, 3)
				if err != nil {
					log.Printf("[M-Pesa] Failed to whitelist MAC %s on router after retries: %v", payment.MacAddress, err)
					if voucher != nil {
						if _, pushErr := s.MikroTik.PushHotspotUsers(&zone, []models.Voucher{*voucher}); pushErr != nil {
							log.Printf("[M-Pesa] Fallback voucher push also failed for payment %d: %v", payment.ID, pushErr)
						} else {
							log.Printf("[M-Pesa] Fallback: voucher %s pushed as router login for payment %d", voucher.Code, payment.ID)
						}
					}
				} else {
					log.Printf("[M-Pesa] Successfully whitelisted MAC %s on router", payment.MacAddress)
				}
			}()
		} else if voucher != nil {
			go func() {
				_, _ = s.MikroTik.PushHotspotUsers(&zone, []models.Voucher{*voucher})
			}()
		}
	}

	if voucher != nil {
		msg := fmt.Sprintf("Hi, payment of KES %.0f received. Your voucher code is %s. Enjoy browsing!", payment.Amount, voucher.Code)
		go s.SMS.Send(phone, msg) //nolint:errcheck
	} else if payment.CustomerID != nil {
		var customer models.Customer
		if err := config.DB.First(&customer, *payment.CustomerID).Error; err == nil {
			expiresAt := utils.CalculateExpiry(pkg.BillingCycle, customer.ExpiresAt)
			config.DB.Model(&customer).Updates(map[string]interface{}{
				"status":     "active",
				"package_id": pkg.ID,
				"zone_id":    pkg.ZoneID,
				"expires_at": expiresAt,
			})
			msg := fmt.Sprintf("Hi %s, your account is active. Package: %s Expires: %s. Support: 0113297270",
				customer.Name, pkg.Name, expiresAt.Format("2006-01-02 15:04"))
			go s.SMS.Send(phone, msg) //nolint:errcheck
		}
	}

	return nil
}

// whitelistWithRetry attempts to whitelist a MAC address on the router,
// retrying with a short backoff since router calls can fail transiently.
func (s *MpesaService) whitelistWithRetry(zone *models.Zone, mac string, pkg *models.Package, attempts int) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = s.MikroTik.WhitelistMAC(zone, mac, pkg); err == nil {
			return nil
		}
		log.Printf("[M-Pesa] WhitelistMAC attempt %d/%d failed for %s: %v", i+1, attempts, mac, err)
		if i < attempts-1 {
			time.Sleep(time.Duration(i+1) * 2 * time.Second)
		}
	}
	return err
}

// SimulateCallback simulates a successful M-Pesa callback for mock/testing mode.
func (s *MpesaService) SimulateCallback(checkoutRequestID string, amount float64, phone string) {
	go func() {
		time.Sleep(500 * time.Millisecond)
		receipt := "MOCK" + strings.ToUpper(randomHex(3))
		payload := map[string]interface{}{
			"Body": map[string]interface{}{
				"stkCallback": map[string]interface{}{
					"MerchantRequestID": "mock_" + randomHex(3),
					"CheckoutRequestID": checkoutRequestID,
					"ResultCode":        float64(0),
					"ResultDesc":        "The service request is processed successfully.",
					"CallbackMetadata": map[string]interface{}{
						"Item": []interface{}{
							map[string]interface{}{"Name": "Amount", "Value": amount},
							map[string]interface{}{"Name": "MpesaReceiptNumber", "Value": receipt},
							map[string]interface{}{"Name": "TransactionDate", "Value": time.Now().Format("20060102150405")},
							map[string]interface{}{"Name": "PhoneNumber", "Value": phone},
						},
					},
				},
			},
		}
		if err := s.HandleCallback(payload); err != nil {
			log.Printf("[M-Pesa] Simulated callback error: %v", err)
		}
	}()
}

// getSetting retrieves a setting from DB, falling back to defaultVal.
func (s *MpesaService) getSetting(key, defaultVal string) string {
	var setting models.Setting
	if err := config.DB.Where("`key` = ?", key).First(&setting).Error; err == nil && setting.Value != nil {
		v := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, *setting.Value)
		if v != "" {
			return v
		}
	}
	return strings.TrimSpace(defaultVal)
}

func bankPaybill(bankName string) string {
	banks := map[string]string{
		"Equity Bank":              "247247",
		"KCB Bank":                 "522522",
		"Co-operative Bank":        "400200",
		"NCBA Bank":                "880100",
		"Absa Bank Kenya":          "303030",
		"Standard Chartered":       "329329",
		"Family Bank":              "222111",
		"I & M Bank":               "542542",
		"Diamond Trust Bank (DTB)": "516600",
		"National Bank":            "547700",
		"Bank of Africa (BOA)":     "972900",
	}
	if v, ok := banks[bankName]; ok {
		return v
	}
	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint:gosec
	return fmt.Sprintf("%X", b)
}
