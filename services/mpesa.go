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

	httpClient *http.Client

	tokenMu sync.Mutex
	// tokenCache is keyed by consumer key rather than a single shared field,
	// since different Organizations can now use different Daraja apps (see
	// resolveMpesaCreds) — a single cached token would otherwise leak one
	// tenant's OAuth token into another tenant's requests.
	tokenCache map[string]cachedToken

	// Map to throttle STK status queries per CheckoutRequestID
	queryThrottles sync.Map
}

type cachedToken struct {
	token  string
	expiry time.Time
}

// mpesaCreds is the resolved set of Daraja credentials/billing routing to
// use for one request, from either the platform-wide defaults or a
// tenant's own configured Daraja app.
type mpesaCreds struct {
	ConsumerKey    string
	ConsumerSecret string
	Shortcode      string
	Passkey        string
	CallbackURL    string
	Env            string
	BillingType    string
	TillNumber     string
	PaybillNumber  string
	PaybillAccount string
	BankName       string
	BankAccount    string
}

// NewMpesaService constructs an MpesaService with an optimized, connection-pooled HTTP client.
func NewMpesaService(sms *SmsService, voucher *VoucherService, mikrotik *MikroTikService) *MpesaService {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &MpesaService{
		SMS:        sms,
		Voucher:    voucher,
		MikroTik:   mikrotik,
		httpClient: &http.Client{
			Timeout:   25 * time.Second,
			Transport: tr,
		},
		tokenCache: make(map[string]cachedToken),
	}
}

// resolveMpesaCreds returns the Daraja credentials/billing routing to use
// for a payment tied to zoneID. If that zone's Organization has configured
// its own Daraja app (OrganizationMpesaConfig.Mode == "own"), those
// credentials are used; any field left blank on the org's config still
// falls back to the platform-wide default for that one field. A zoneID of
// 0, or an org with no config row (the default), uses the platform-wide
// credentials unchanged — identical to the single-tenant behavior before
// per-org Daraja support existed.
func (s *MpesaService) resolveMpesaCreds(zoneID uint) mpesaCreds {
	// Load all mpesa settings in a single SQL query instead of 12 sequential queries
	settingsMap := s.loadMpesaSettingsMap()

	creds := mpesaCreds{
		ConsumerKey:    getSettingFromMap(settingsMap, "mpesa_consumer_key", config.Config.MpesaConsumerKey),
		ConsumerSecret: getSettingFromMap(settingsMap, "mpesa_consumer_secret", config.Config.MpesaConsumerSecret),
		Shortcode:      getSettingFromMap(settingsMap, "mpesa_shortcode", config.Config.MpesaShortcode),
		Passkey:        getSettingFromMap(settingsMap, "mpesa_passkey", config.Config.MpesaPasskey),
		CallbackURL:    getSettingFromMap(settingsMap, "mpesa_callback_url", config.Config.MpesaCallbackURL),
		Env:            getSettingFromMap(settingsMap, "mpesa_environment", config.Config.MpesaEnv),
		BillingType:    getSettingFromMap(settingsMap, "mpesa_billing_type", "paybill"),
		TillNumber:     getSettingFromMap(settingsMap, "mpesa_till_number", ""),
		PaybillNumber:  getSettingFromMap(settingsMap, "mpesa_paybill_number", ""),
		PaybillAccount: getSettingFromMap(settingsMap, "mpesa_paybill_account", ""),
		BankName:       getSettingFromMap(settingsMap, "mpesa_bank_name", ""),
		BankAccount:    getSettingFromMap(settingsMap, "mpesa_bank_account", ""),
	}
	if zoneID == 0 {
		return creds
	}

	var zone models.Zone
	if err := config.DB.Select("organization_id").First(&zone, zoneID).Error; err != nil {
		return creds
	}
	var orgCfg models.OrganizationMpesaConfig
	if err := config.DB.Where("organization_id = ? AND mode = ?", zone.OrganizationID, "own").First(&orgCfg).Error; err != nil {
		return creds
	}

	if orgCfg.ConsumerKey != "" {
		creds.ConsumerKey = orgCfg.ConsumerKey
	}
	if orgCfg.ConsumerSecret != "" {
		creds.ConsumerSecret = orgCfg.ConsumerSecret
	}
	if orgCfg.Shortcode != "" {
		creds.Shortcode = orgCfg.Shortcode
	}
	if orgCfg.Passkey != "" {
		creds.Passkey = orgCfg.Passkey
	}
	if orgCfg.CallbackURL != "" {
		creds.CallbackURL = orgCfg.CallbackURL
	}
	if orgCfg.Env != "" {
		creds.Env = orgCfg.Env
	}
	if orgCfg.BillingType != "" {
		creds.BillingType = orgCfg.BillingType
	}
	if orgCfg.TillNumber != "" {
		creds.TillNumber = orgCfg.TillNumber
	}
	if orgCfg.PaybillNumber != "" {
		creds.PaybillNumber = orgCfg.PaybillNumber
	}
	if orgCfg.PaybillAccount != "" {
		creds.PaybillAccount = orgCfg.PaybillAccount
	}
	if orgCfg.BankName != "" {
		creds.BankName = orgCfg.BankName
	}
	if orgCfg.BankAccount != "" {
		creds.BankAccount = orgCfg.BankAccount
	}
	return creds
}

// MpesaSTKResponse is the result of an STK push initiation.
type MpesaSTKResponse struct {
	Status              string `json:"status"`
	CheckoutRequestID   string `json:"checkout_request_id"`
	ResponseDescription string `json:"response_description"`
	IsMock              bool   `json:"is_mock"`
}

// getBaseURL returns the Daraja API base URL for the given environment.
func (s *MpesaService) getBaseURL(env string) string {
	if strings.ToLower(env) == "production" {
		return "https://api.safaricom.co.ke"
	}
	return "https://sandbox.safaricom.co.ke"
}

// GetAccessToken fetches the OAuth token from Daraja for the given
// credentials, caching it in-memory (keyed by consumer key) for its
// reported lifetime (Daraja tokens are valid ~1 hour) so we don't make a
// round trip to Safaricom on every STK push.
func (s *MpesaService) GetAccessToken(creds mpesaCreds) (string, error) {
	if creds.ConsumerKey == "" || creds.ConsumerKey == "mock_consumer_key" {
		return "mock_token", nil
	}

	s.tokenMu.Lock()
	if cached, ok := s.tokenCache[creds.ConsumerKey]; ok && cached.token != "" && time.Now().Before(cached.expiry) {
		s.tokenMu.Unlock()
		return cached.token, nil
	}
	s.tokenMu.Unlock()

	apiURL := s.getBaseURL(creds.Env) + "/oauth/v1/generate?grant_type=client_credentials"
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	basicAuth := base64.StdEncoding.EncodeToString([]byte(creds.ConsumerKey + ":" + creds.ConsumerSecret))
	req.Header.Set("Authorization", "Basic "+basicAuth)

	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("daraja auth failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read daraja auth response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daraja auth returned status %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to decode daraja auth response: %w", err)
	}
	token, ok := result["access_token"].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("no access_token in daraja response: %s", string(body))
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

	s.tokenMu.Lock()
	s.tokenCache[creds.ConsumerKey] = cachedToken{token: token, expiry: time.Now().Add(expiresIn)}
	s.tokenMu.Unlock()
	return token, nil
}

// InitiateSTKPush sends a payment prompt to the customer's phone, using
// zoneID to resolve whether that zone's Organization has its own Daraja
// app configured or should use the platform-wide default (see
// resolveMpesaCreds). Pass 0 for zoneID to force the platform default.
func (s *MpesaService) InitiateSTKPush(zoneID uint, phone string, amount float64, reference, description string) (*MpesaSTKResponse, error) {
	phone = utils.FormatPhone(phone)
	if len(phone) != 12 || (!strings.HasPrefix(phone, "2547") && !strings.HasPrefix(phone, "2541")) {
		return nil, fmt.Errorf("invalid phone number: must be 12 digits (e.g. 2547XXXXXXXX or 2541XXXXXXXX)")
	}
	if amount < 1 {
		return nil, fmt.Errorf("amount must be at least 1 KES")
	}

	creds := s.resolveMpesaCreds(zoneID)
	shortcode := creds.Shortcode
	passkey := creds.Passkey
	callbackURL := creds.CallbackURL
	env := creds.Env

	token, err := s.GetAccessToken(creds)
	if err != nil {
		if strings.ToLower(env) != "production" || config.Config.AppEnv == "local" {
			log.Printf("[M-Pesa] GetAccessToken failed (%v) — falling back to mock STK Push", err)
			token = "mock_token"
		} else {
			return nil, err
		}
	}

	isLocalCallback := callbackURL == "" ||
		strings.Contains(callbackURL, "localhost") ||
		strings.Contains(callbackURL, "127.0.0.1") ||
		strings.Contains(callbackURL, "192.168.") ||
		!strings.HasPrefix(callbackURL, "https://")

	if token == "mock_token" || strings.ToLower(env) == "mock" || (strings.ToLower(env) != "production" && isLocalCallback) {
		checkoutID := fmt.Sprintf("ws_CO_%d_%d", rand.Intn(999999)+100000, time.Now().Unix())
		log.Printf("[M-Pesa] Mock STK Push: phone=%s amount=%.0f ref=%s", phone, amount, reference)
		return &MpesaSTKResponse{
			Status:              "success",
			CheckoutRequestID:   checkoutID,
			ResponseDescription: "Mock STK Push initiated successfully",
			IsMock:              true,
		}, nil
	}

	transactionType := "CustomerPayBillOnline"
	partyB := creds.PaybillNumber
	if partyB == "" {
		partyB = shortcode
	}
	accountReference := creds.PaybillAccount
	if accountReference == "" {
		accountReference = reference
	}

	if creds.BillingType == "till" {
		transactionType = "CustomerBuyGoodsOnline"
		partyB = creds.TillNumber
		if partyB == "" {
			partyB = shortcode
		}
		accountReference = reference
	} else if creds.BillingType == "bank" {
		partyB = bankPaybill(creds.BankName)
		accountReference = creds.BankAccount
		if accountReference == "" {
			accountReference = reference
		}
	}

	// Sanitize to Daraja STK Push specification constraints:
	// AccountReference: Max 12 alphanumeric characters
	// TransactionDesc: Max 13 alphanumeric characters
	accountReference = sanitizeAccountReference(accountReference)
	description = sanitizeTransactionDesc(description)

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
	req, err := http.NewRequest(http.MethodPost, s.getBaseURL(env)+"/mpesa/stkpush/v1/processrequest", strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
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
		resultDesc, _ := stkCallback["ResultDesc"].(string)
		reason := friendlySTKFailureReason(rc, resultDesc)
		return s.ProcessPaymentFailure(&payment, reason)
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

	return s.ProcessPaymentSuccess(&payment, receiptNumber, phone)
}

// ProcessPaymentSuccess handles database and network/side-effects for a successful STK payment.
func (s *MpesaService) ProcessPaymentSuccess(payment *models.Payment, receiptNumber, phone string) error {
	res := config.DB.Model(&models.Payment{}).
		Where("id = ? AND status != ?", payment.ID, "completed").
		Updates(map[string]interface{}{
			"status":               "completed",
			"status_reason":        nil,
			"mpesa_receipt_number": receiptNumber,
		})
	if res.RowsAffected == 0 {
		log.Printf("[M-Pesa] Duplicate/late success callback/query for payment %d ignored (status already %s)", payment.ID, payment.Status)
		return nil
	}

	// update local struct so subsequent logic reads updated status if needed
	payment.Status = "completed"
	payment.MpesaReceiptNumber = &receiptNumber

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

				template := s.SMS.GetSetting("sms_template_credit", "Hi {name}, KES {amount} credited to your account. Your new balance is KES {balance}. Enjoy browsing!")
				msg := utils.RenderTemplate(template, map[string]string{
					"name":    customer.Name,
					"amount":  fmt.Sprintf("%.2f", payment.Amount),
					"balance": fmt.Sprintf("%.2f", newBalance),
				})
				if s.SMS.GetSetting("sms_enable_credit", "yes") != "no" {
					go s.SMS.SendForZone(payment.ZoneID, phone, msg)
				}
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

	var customer *models.Customer

	// 1. Resolve or create customer account
	if payment.CustomerID != nil {
		var c models.Customer
		if err := config.DB.First(&c, *payment.CustomerID).Error; err == nil {
			customer = &c
		}
	}

	if customer == nil && phone != "" {
		var c models.Customer
		if err := config.DB.Where("phone = ?", phone).First(&c).Error; err == nil {
			customer = &c
			payment.CustomerID = &c.ID
			config.DB.Model(&models.Payment{}).Where("id = ?", payment.ID).Update("customer_id", c.ID)
		}
	}

	if customer == nil && payment.MacAddress != "" {
		var dev models.CustomerDevice
		if err := config.DB.Preload("Customer").Where("mac_address = ?", payment.MacAddress).First(&dev).Error; err == nil && dev.Customer != nil {
			customer = dev.Customer
			payment.CustomerID = &customer.ID
			config.DB.Model(&models.Payment{}).Where("id = ?", payment.ID).Update("customer_id", customer.ID)
		}
	}

	// Auto-register customer if not found
	if customer == nil {
		cleanPhone := phone
		if cleanPhone == "" {
			cleanPhone = payment.Phone
		}
		displayName := "Customer " + cleanPhone[max(0, len(cleanPhone)-4):]
		pppoeUser := "user_" + cleanPhone[max(0, len(cleanPhone)-6):]
		newCust := models.Customer{
			Name:          displayName,
			Phone:         cleanPhone,
			ZoneID:        payment.ZoneID,
			PackageID:     pkg.ID,
			Type:          "hotspot",
			Status:        "active",
			PPPoEUsername: &pppoeUser,
		}
		if payment.MacAddress != "" {
			newCust.MacAddress = &payment.MacAddress
		}
		if err := config.DB.Create(&newCust).Error; err == nil {
			customer = &newCust
			payment.CustomerID = &newCust.ID
			config.DB.Model(&models.Payment{}).Where("id = ?", payment.ID).Update("customer_id", newCust.ID)
		}
	}

	// 2. Link Customer Device
	if customer != nil && payment.MacAddress != "" {
		customer.MacAddress = &payment.MacAddress
		var dev models.CustomerDevice
		if err := config.DB.Where("customer_id = ? AND mac_address = ?", customer.ID, payment.MacAddress).First(&dev).Error; err != nil {
			dev = models.CustomerDevice{
				CustomerID: customer.ID,
				MacAddress: payment.MacAddress,
				LastSeenAt: time.Now(),
			}
			if payment.IpAddress != "" {
				dev.IPAddress = &payment.IpAddress
			}
			config.DB.Create(&dev)
		} else {
			dev.LastSeenAt = time.Now()
			if payment.IpAddress != "" {
				dev.IPAddress = &payment.IpAddress
			}
			config.DB.Save(&dev)
		}
	}

	// 3. Update customer subscription status & expiry
	if customer != nil {
		expiresAt := utils.CalculateExpiry(pkg.BillingCycle, customer.ExpiresAt)
		custUpdates := map[string]interface{}{
			"status":      "active",
			"package_id":  pkg.ID,
			"zone_id":     pkg.ZoneID,
			"mac_address": payment.MacAddress,
			"expires_at":  expiresAt,
		}

		if phone != "" && (customer.Phone == "" || strings.HasPrefix(customer.Phone, "GUEST")) {
			customer.Phone = phone
			custUpdates["phone"] = phone
		}
		if strings.HasPrefix(customer.Name, "Guest") || strings.HasPrefix(customer.Name, "Customer_") || customer.Name == "" {
			var pastC2B models.UnmatchedC2BPayment
			cleanPhone := utils.FormatPhone(phone)
			phoneSuffix := cleanPhone
			if len(phoneSuffix) >= 9 {
				phoneSuffix = phoneSuffix[len(phoneSuffix)-9:]
			}
			if err := config.DB.Where("(phone LIKE ? OR bill_ref_number LIKE ?) AND first_name != ''", "%"+phoneSuffix, "%"+phoneSuffix).Order("id DESC").First(&pastC2B).Error; err == nil {
				nameParts := []string{}
				if pastC2B.FirstName != "" {
					nameParts = append(nameParts, strings.Title(strings.ToLower(strings.TrimSpace(pastC2B.FirstName))))
				}
				if pastC2B.LastName != "" {
					nameParts = append(nameParts, strings.Title(strings.ToLower(strings.TrimSpace(pastC2B.LastName))))
				}
				fullName := strings.Join(nameParts, " ")
				if fullName != "" {
					customer.Name = fullName
					custUpdates["name"] = fullName
				}
			} else if phone != "" {
				formatted := utils.FormatPhone(phone)
				if len(formatted) == 12 && strings.HasPrefix(formatted, "254") {
					customer.Name = "0" + formatted[3:]
				} else {
					customer.Name = phone
				}
				custUpdates["name"] = customer.Name
			}
		}

		config.DB.Model(customer).Updates(custUpdates)

		templateActive := s.SMS.GetSetting("sms_template_active", "Hi {name}, your account is active. Package: {package} Expires: {expiry}.")
		msg := utils.RenderTemplate(templateActive, map[string]string{
			"name":    customer.Name,
			"package": pkg.Name,
			"expiry":  expiresAt.Format("2006-01-02 15:04"),
		})
		if s.SMS.GetSetting("sms_enable_active", "yes") != "no" {
			go s.SMS.SendForZone(payment.ZoneID, phone, msg) //nolint:errcheck
		}
	} else if voucher != nil {
		template := s.SMS.GetSetting("sms_template_voucher", "Hi {name}, payment of KES {price} received. Your voucher code is {code}. Enjoy browsing!")
		msg := utils.RenderTemplate(template, map[string]string{
			"name":  "Guest",
			"price": fmt.Sprintf("%.0f", payment.Amount),
			"code":  voucher.Code,
		})
		if s.SMS.GetSetting("sms_enable_voucher", "yes") != "no" {
			go s.SMS.SendForZone(payment.ZoneID, phone, msg) //nolint:errcheck
		}
	}

	return nil
}

// ProcessPaymentFailure handles database updates for a failed STK payment.
// friendlySTKFailureReason maps a Daraja STK push ResultCode to a short,
// actionable message for the customer. Safaricom's own ResultDesc strings
// are internal/inconsistent wording (e.g. "DS timeout user cannot be
// reached", or terse codes with no real description at all) — showing them
// verbatim in the app ("Failed due to an unresolved reason type.") reads as
// broken rather than explaining what the customer should do next. Known
// codes get a clear message; anything unmapped falls back to a generic,
// still-actionable message instead of Safaricom's raw text.
func friendlySTKFailureReason(resultCode float64, resultDesc string) string {
	switch resultCode {
	case 1:
		return "Payment failed: insufficient M-Pesa balance."
	case 1032:
		return "Payment cancelled. You closed the M-Pesa prompt before approving it."
	case 1037:
		return "Payment timed out. You didn't enter your M-Pesa PIN in time — please try again."
	case 1025, 9999:
		return "We couldn't process this payment. Please try again."
	case 2001:
		return "Payment failed: wrong M-Pesa PIN entered."
	case 1001:
		return "Payment failed: you have another M-Pesa transaction in progress. Please finish or cancel it, then try again."
	case 1019:
		return "Payment request expired. Please try again."
	}
	if resultDesc != "" {
		log.Printf("[M-Pesa] Unmapped STK failure ResultCode=%.0f ResultDesc=%q — showing generic message to customer", resultCode, resultDesc)
	}
	return "Payment could not be completed. Please try again, or contact support if you were charged."
}

func (s *MpesaService) ProcessPaymentFailure(payment *models.Payment, reason string) error {
	res := config.DB.Model(&models.Payment{}).
		Where("id = ? AND (status = ? OR (status = ? AND status_reason = ?))", payment.ID, "pending", "failed", "The transaction is still under processing").
		Updates(map[string]interface{}{
			"status":        "failed",
			"status_reason": reason,
		})
	if res.RowsAffected == 0 {
		log.Printf("[M-Pesa] Duplicate/late failure callback/query for payment %d ignored (status already %s)", payment.ID, payment.Status)
		return nil
	}

	// update local struct
	payment.Status = "failed"
	payment.StatusReason = &reason

	log.Printf("[M-Pesa] Payment failed: %s", reason)
	return nil
}

// QuerySTKPushStatus queries Daraja for the status of an STK push
// transaction, using zoneID to resolve the same tenant credentials that
// initiated the push (see resolveMpesaCreds).
func (s *MpesaService) QuerySTKPushStatus(zoneID uint, checkoutRequestID string) (map[string]interface{}, error) {
	creds := s.resolveMpesaCreds(zoneID)
	shortcode := creds.Shortcode
	passkey := creds.Passkey
	env := creds.Env

	token, err := s.GetAccessToken(creds)
	if err != nil {
		return nil, err
	}

	if strings.ToLower(env) != "production" && token == "mock_token" {
		return map[string]interface{}{
			"ResponseCode": "0",
			"ResultCode":   "0",
			"ResultDesc":   "Mock STK Query Success",
		}, nil
	}

	timestamp := time.Now().Format("20060102150405")
	password := base64.StdEncoding.EncodeToString([]byte(shortcode + passkey + timestamp))

	payload := map[string]interface{}{
		"BusinessShortCode": shortcode,
		"Password":          password,
		"Timestamp":         timestamp,
		"CheckoutRequestID": checkoutRequestID,
	}

	bodyBytes, _ := json.Marshal(payload)
	apiURL := s.getBaseURL(env) + "/mpesa/stkpushquery/v1/query"
	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("STK push query request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	return result, nil
}

// QueryAndUpdateSTKStatus queries Safaricom to check the status of a pending payment and updates the database accordingly.
func (s *MpesaService) QueryAndUpdateSTKStatus(payment *models.Payment) (string, error) {
	checkoutID := ""
	if payment.MpesaTransactionID != nil {
		checkoutID = *payment.MpesaTransactionID
	}
	if checkoutID == "" {
		return "pending", fmt.Errorf("no checkout request ID found for payment %d", payment.ID)
	}

	// Give the customer a realistic window to actually see the STK prompt
	// on their phone and enter their PIN before we ask Safaricom for a
	// result. Querying within the first few seconds of the push doesn't
	// reflect a real user decision yet — Safaricom's own STK Query API
	// responds to a too-early query with ResultCode 2029 ("Failed due to
	// an unresolved reason type"), which looks exactly like a terminal
	// failure but isn't one. Without this guard, HotspotStatus's fast
	// (800ms) polling loop was firing the very first reconciliation query
	// ~1-3s after every push and permanently failing every real payment
	// before the customer had a chance to respond.
	if time.Since(payment.CreatedAt) < 12*time.Second {
		return "pending", nil
	}

	// Throttling check: only query Safaricom at most once every 5 seconds per checkout ID
	now := time.Now()
	if val, ok := s.queryThrottles.Load(checkoutID); ok {
		if lastTime, ok := val.(time.Time); ok && now.Sub(lastTime) < 5*time.Second {
			return payment.Status, nil
		}
	}
	s.queryThrottles.Store(checkoutID, now)

	log.Printf("[M-Pesa] Querying STK status for payment %d (CheckoutID: %s)", payment.ID, checkoutID)
	result, err := s.QuerySTKPushStatus(payment.ZoneID, checkoutID)
	if err != nil {
		return "pending", fmt.Errorf("failed to query status from M-Pesa: %w", err)
	}

	log.Printf("[M-Pesa] Query result for %s: %+v", checkoutID, result)

	if errCode, ok := result["errorCode"].(string); ok && (errCode == "500.001.1001" || errCode == "404.002.02") {
		return "pending", nil
	}

	responseCode, _ := result["ResponseCode"].(string)
	if responseCode != "0" {
		return "pending", nil
	}

	resultCodeVal := result["ResultCode"]
	if resultCodeVal == nil {
		return "pending", nil
	}

	var rc float64
	switch v := resultCodeVal.(type) {
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

	resultDesc, _ := result["ResultDesc"].(string)

	if rc == 0 {
		receiptNumber := fmt.Sprintf("QRY_%s", checkoutID)
		if meta, ok := result["CallbackMetadata"].(map[string]interface{}); ok {
			if items, ok := meta["Item"].([]interface{}); ok {
				for _, itemRaw := range items {
					item, _ := itemRaw.(map[string]interface{})
					name, _ := item["Name"].(string)
					val := item["Value"]
					if name == "MpesaReceiptNumber" && val != nil {
						if v, ok := val.(string); ok && v != "" {
							receiptNumber = v
						}
					}
				}
			}
		}

		err := s.ProcessPaymentSuccess(payment, receiptNumber, payment.Phone)
		if err != nil {
			return "pending", err
		}
		return "completed", nil
	}

	// ResultCode 4999 ("The transaction is still under processing") or 2029
	// indicates Safaricom has not resolved the transaction yet (customer is still entering PIN or prompt in flight).
	// We keep status as pending up to 150 seconds (2.5 minutes) to give the customer ample time to complete the prompt.
	if rc == 4999 || rc == 2029 || (resultDesc != "" && (strings.Contains(strings.ToLower(resultDesc), "unresolved") || strings.Contains(strings.ToLower(resultDesc), "under processing") || strings.Contains(strings.ToLower(resultDesc), "in progress"))) {
		if time.Since(payment.CreatedAt) < 150*time.Second {
			return "pending", nil
		}
		reason := "Payment timed out. You didn't enter your M-Pesa PIN in time — please try again."
		if err := s.ProcessPaymentFailure(payment, reason); err != nil {
			return "pending", err
		}
		return "failed", nil
	}

	resultDesc, _ = result["ResultDesc"].(string)
	reason := friendlySTKFailureReason(rc, resultDesc)
	err = s.ProcessPaymentFailure(payment, reason)
	if err != nil {
		return "pending", err
	}
	return "failed", nil
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

// loadMpesaSettingsMap loads all M-Pesa configuration settings in a single SQL query.
func (s *MpesaService) loadMpesaSettingsMap() map[string]string {
	keys := []string{
		"mpesa_consumer_key", "mpesa_consumer_secret", "mpesa_shortcode",
		"mpesa_passkey", "mpesa_callback_url", "mpesa_environment",
		"mpesa_billing_type", "mpesa_till_number", "mpesa_paybill_number",
		"mpesa_paybill_account", "mpesa_bank_name", "mpesa_bank_account",
	}
	var settings []models.Setting
	if err := config.DB.Where("`key` IN ?", keys).Find(&settings).Error; err != nil {
		return make(map[string]string)
	}
	res := make(map[string]string, len(settings))
	for _, st := range settings {
		if st.Value != nil {
			v := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return r
			}, *st.Value)
			if v != "" {
				res[st.Key] = v
			}
		}
	}
	return res
}

func getSettingFromMap(m map[string]string, key, defaultVal string) string {
	if val, ok := m[key]; ok && val != "" {
		return val
	}
	return strings.TrimSpace(defaultVal)
}

// sanitizeAccountReference formats AccountReference to Safaricom Daraja STK Push limits (max 12 alphanumeric characters).
func sanitizeAccountReference(raw string) string {
	var clean strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			clean.WriteRune(r)
		}
	}
	res := clean.String()
	if len(res) > 12 {
		res = res[:12]
	}
	if res == "" {
		return "ZyraNet"
	}
	return res
}

// sanitizeTransactionDesc formats TransactionDesc to Safaricom Daraja limits (max 13 characters, alphanumeric without weird characters).
func sanitizeTransactionDesc(raw string) string {
	var clean strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			clean.WriteRune(r)
		}
	}
	res := clean.String()
	if len(res) > 13 {
		res = res[:13]
	}
	if res == "" {
		return "Internet"
	}
	return res
}

// C2BRegisterResponse represents Safaricom's response when registering C2B URLs.
type C2BRegisterResponse struct {
	OriginatorConversationID string `json:"OriginatorConversationID"`
	ConversationID           string `json:"ConversationID"`
	ResponseDescription      string `json:"ResponseDescription"`
	ResponseCode             string `json:"ResponseCode"`
}

// RegisterC2BURLs registers the C2B ValidationURL and ConfirmationURL with Safaricom Daraja.
func (s *MpesaService) RegisterC2BURLs(zoneID uint, confirmationURL, validationURL, responseType string) (*C2BRegisterResponse, error) {
	creds := s.resolveMpesaCreds(zoneID)
	shortcode := creds.Shortcode
	if creds.BillingType == "paybill" && creds.PaybillNumber != "" {
		shortcode = creds.PaybillNumber
	}
	if shortcode == "" {
		if strings.ToLower(creds.Env) == "mock" || strings.ToLower(creds.Env) == "sandbox" || creds.Env == "" || config.Config.AppEnv == "test" || config.Config.AppEnv == "local" || config.Config.AppEnv == "" {
			shortcode = "600000"
		} else {
			return nil, fmt.Errorf("shortcode is not configured")
		}
	}

	if responseType == "" {
		responseType = "Completed"
	}
	if confirmationURL == "" {
		confirmationURL = creds.CallbackURL
	}
	if validationURL == "" {
		validationURL = creds.CallbackURL
	}

	token, err := s.GetAccessToken(creds)
	if err != nil {
		if strings.ToLower(creds.Env) != "production" || config.Config.AppEnv == "local" || config.Config.AppEnv == "test" {
			token = "mock_token"
		} else {
			return nil, fmt.Errorf("failed to obtain Daraja token: %w", err)
		}
	}

	isLocalURL := confirmationURL == "" ||
		strings.Contains(confirmationURL, "localhost") ||
		strings.Contains(confirmationURL, "127.0.0.1") ||
		strings.Contains(confirmationURL, "example.com") ||
		!strings.HasPrefix(confirmationURL, "https://")

	if token == "mock_token" || strings.ToLower(creds.Env) == "mock" || (strings.ToLower(creds.Env) != "production" && isLocalURL) {
		return &C2BRegisterResponse{
			OriginatorConversationID: "mock_orig_conv_id",
			ConversationID:           "mock_conv_id",
			ResponseDescription:      "Mock C2B URLs registered successfully",
			ResponseCode:             "0",
		}, nil
	}

	payload := map[string]interface{}{
		"ShortCode":       shortcode,
		"ResponseType":    responseType,
		"ConfirmationURL": confirmationURL,
		"ValidationURL":   validationURL,
	}

	bodyBytes, _ := json.Marshal(payload)
	apiURL := s.getBaseURL(creds.Env) + "/mpesa/c2b/v2/registerurl"
	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("C2B URL registration request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result C2BRegisterResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode C2B register response: %s", string(body))
	}

	if result.ResponseCode != "0" && !strings.Contains(strings.ToLower(result.ResponseDescription), "success") {
		return &result, fmt.Errorf("C2B registration failed: %s (code: %s)", result.ResponseDescription, result.ResponseCode)
	}

	return &result, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint:gosec
	return fmt.Sprintf("%X", b)
}
