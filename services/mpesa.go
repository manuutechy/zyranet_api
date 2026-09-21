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
	// Radius, when set, is told about a customer as soon as they pay so a
	// reconnecting device is accepted without waiting for the next reconcile.
	Radius *RadiusService

	httpClient *http.Client

	tokenMu sync.Mutex
	// tokenCache is keyed by consumer key rather than a single shared field,
	// since different Organizations can now use different Daraja apps (see
	// resolveMpesaCreds) — a single cached token would otherwise leak one
	// tenant's OAuth token into another tenant's requests.
	tokenCache map[string]cachedToken

	// Map to throttle STK status queries per CheckoutRequestID
	queryThrottles sync.Map

	// baseURLOverride points every Daraja call at another host. Tests only.
	baseURLOverride string
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
	// Own is true when these came from an ISP's own Daraja app (mode "own")
	// rather than Zyra Net's shared one. Own creds are never mixed with the
	// platform's, and are never allowed to fall back to a mock/simulated
	// payment outside local dev/test — see mockAllowed.
	Own bool
	// Direct* are set when the zone's ISP uses Zyra's credentials with direct
	// settlement: the push is still signed by Zyra's app but pays the ISP's
	// own till (DirectTill) or paybill + account (DirectPaybill/DirectAccount).
	DirectTill    string
	DirectPaybill string
	DirectAccount string
}

// Direct reports whether pushes go straight to the ISP's own destination.
func (c mpesaCreds) Direct() bool { return c.DirectTill != "" || c.DirectPaybill != "" }

// normalizeBillingType maps a stored billing type onto the two collection
// types Daraja actually supports. "bank" used to be a third option, but an
// STK push can only settle to the shortcode/till the credentials belong to —
// a bank is a *settlement* destination, not something a customer can be
// charged into directly — so legacy "bank" rows collect like "paybill".
func normalizeBillingType(t string) string {
	if strings.EqualFold(strings.TrimSpace(t), "till") {
		return "till"
	}
	return "paybill"
}

// missingFields lists what an own-mode Daraja config still needs before it
// can collect a payment. Only meaningful for Own creds — the platform's
// creds are allowed to be blank (mock/demo mode).
func (c mpesaCreds) missingFields() []string {
	var missing []string
	if c.ConsumerKey == "" {
		missing = append(missing, "consumer key")
	}
	if c.ConsumerSecret == "" {
		missing = append(missing, "consumer secret")
	}
	if c.Shortcode == "" {
		missing = append(missing, "shortcode")
	}
	if c.Passkey == "" {
		missing = append(missing, "passkey")
	}
	if c.BillingType == "till" && c.TillNumber == "" {
		missing = append(missing, "till number")
	}
	return missing
}

// mockAllowed reports whether a payment may be faked instead of sent to
// Safaricom. Platform creds keep the existing demo behavior. An ISP's own
// creds must never fake success on a deployed server: if their environment
// is mis-set or their keys are wrong, the customer would get connected for
// free while no money moves.
func (c mpesaCreds) mockAllowed() bool {
	return !c.Own || config.Config.AppEnv == "local" || config.Config.AppEnv == "test"
}

// stkRoute is the Daraja routing for one STK push.
type stkRoute struct {
	TransactionType string
	PartyB          string
}

// route returns where an STK push settles. Paybill: PartyB is the same
// shortcode the request is signed with. Till (Buy Goods): the request is
// signed with the store/head-office shortcode and PartyB is the till.
func (c mpesaCreds) route() stkRoute {
	if c.DirectTill != "" {
		return stkRoute{TransactionType: "CustomerBuyGoodsOnline", PartyB: c.DirectTill}
	}
	if c.DirectPaybill != "" {
		return stkRoute{TransactionType: "CustomerPayBillOnline", PartyB: c.DirectPaybill}
	}
	if c.BillingType == "till" && c.TillNumber != "" && c.TillNumber != c.Shortcode {
		return stkRoute{TransactionType: "CustomerBuyGoodsOnline", PartyB: c.TillNumber}
	}
	return stkRoute{TransactionType: "CustomerPayBillOnline", PartyB: c.Shortcode}
}

// c2bShortcode is the number customers pay manually (and the one C2B URLs
// must be registered against): the till for Buy Goods, otherwise the paybill.
func (c mpesaCreds) c2bShortcode() string {
	if c.BillingType == "till" && c.TillNumber != "" {
		return c.TillNumber
	}
	if c.Shortcode != "" {
		return c.Shortcode
	}
	return c.PaybillNumber
}

// errPaymentsUnavailable is what a customer sees when their ISP's Daraja
// setup can't take payments. The specifics are logged, not shown.
var errPaymentsUnavailable = fmt.Errorf("payments are temporarily unavailable on this network — please contact support")

// NewMpesaService constructs an MpesaService with an optimized, connection-pooled HTTP client.
func NewMpesaService(sms *SmsService, voucher *VoucherService, mikrotik *MikroTikService) *MpesaService {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &MpesaService{
		SMS:      sms,
		Voucher:  voucher,
		MikroTik: mikrotik,
		httpClient: &http.Client{
			Timeout:   25 * time.Second,
			Transport: tr,
		},
		tokenCache: make(map[string]cachedToken),
	}
}

// ResolveMpesaCreds returns the Daraja credentials/billing routing to use
// for a payment tied to zoneID.
//
// If that zone's Organization is in "own" mode, the ISP's own Daraja app is
// used *exclusively*: a blank field stays blank (and is reported by
// missingFields) rather than borrowing the platform's value. Mixing them —
// e.g. an ISP's consumer key with Zyra's passkey/till — either fails at
// Safaricom or, worse, sends the ISP's customers' money into Zyra's account.
// The one exception is CallbackURL, which is Zyra's own endpoint, so it
// defaults to the platform's. A zoneID of 0, or an org with no config row
// (the default), uses the platform-wide credentials.
func (s *MpesaService) ResolveMpesaCreds(zoneID uint) mpesaCreds {
	if c, ok := cachedCredsFor(zoneID); ok {
		return c
	}
	c := s.resolveMpesaCreds(zoneID)
	storeCreds(zoneID, c)
	return c
}

func (s *MpesaService) resolveMpesaCreds(zoneID uint) mpesaCreds {
	settingsMap := s.loadMpesaSettingsMap()

	platform := mpesaCreds{
		ConsumerKey:    getSettingFromMap(settingsMap, "mpesa_consumer_key", config.Config.MpesaConsumerKey),
		ConsumerSecret: getSettingFromMap(settingsMap, "mpesa_consumer_secret", config.Config.MpesaConsumerSecret),
		Shortcode:      getSettingFromMap(settingsMap, "mpesa_shortcode", config.Config.MpesaShortcode),
		Passkey:        getSettingFromMap(settingsMap, "mpesa_passkey", config.Config.MpesaPasskey),
		CallbackURL:    getSettingFromMap(settingsMap, "mpesa_callback_url", config.Config.MpesaCallbackURL),
		Env:            getSettingFromMap(settingsMap, "mpesa_environment", config.Config.MpesaEnv),
		BillingType:    normalizeBillingType(getSettingFromMap(settingsMap, "mpesa_billing_type", "paybill")),
		TillNumber:     getSettingFromMap(settingsMap, "mpesa_till_number", ""),
		PaybillNumber:  getSettingFromMap(settingsMap, "mpesa_paybill_number", ""),
		PaybillAccount: getSettingFromMap(settingsMap, "mpesa_paybill_account", ""),
	}
	if zoneID == 0 {
		return platform
	}

	var zone models.Zone
	if err := config.DB.Select("organization_id").First(&zone, zoneID).Error; err != nil {
		return platform
	}
	var orgCfg models.OrganizationMpesaConfig
	if err := config.DB.Where("organization_id = ? AND mode = ?", zone.OrganizationID, "own").First(&orgCfg).Error; err != nil {
		return withDirectSettlement(platform, zone.OrganizationID)
	}

	trim := strings.TrimSpace
	own := mpesaCreds{
		Own:            true,
		ConsumerKey:    trim(orgCfg.ConsumerKey),
		ConsumerSecret: trim(orgCfg.ConsumerSecret),
		Shortcode:      trim(orgCfg.Shortcode),
		Passkey:        trim(orgCfg.Passkey),
		CallbackURL:    trim(orgCfg.CallbackURL),
		Env:            trim(orgCfg.Env),
		BillingType:    normalizeBillingType(orgCfg.BillingType),
		TillNumber:     trim(orgCfg.TillNumber),
		PaybillNumber:  trim(orgCfg.PaybillNumber),
		PaybillAccount: trim(orgCfg.PaybillAccount),
	}
	if own.CallbackURL == "" {
		own.CallbackURL = platform.CallbackURL
	}
	if own.Env == "" {
		own.Env = "sandbox"
	}
	return own
}

// withDirectSettlement points platform creds at the ISP's own till/paybill
// when that ISP has direct settlement switched on and a complete destination.
func withDirectSettlement(c mpesaCreds, orgID uint) mpesaCreds {
	var org models.Organization
	if err := config.DB.Select("id", "direct_settlement", "settlement_type", "settlement_till_number",
		"settlement_paybill_number", "settlement_account_number").First(&org, orgID).Error; err != nil || !org.DirectSettlement {
		return c
	}
	till := strings.TrimSpace(org.SettlementTillNumber)
	paybill := strings.TrimSpace(org.SettlementPaybillNumber)
	account := strings.TrimSpace(org.SettlementAccountNumber)
	switch {
	case org.SettlementType == "till" && till != "":
		c.DirectTill = till
	case org.SettlementType == "paybill" && paybill != "" && account != "":
		c.DirectPaybill, c.DirectAccount = paybill, account
	}
	return c
}

// OrgForShortcode returns the ISP whose own Daraja app owns the given
// shortcode/till/paybill, as seen in a C2B confirmation's BusinessShortCode.
// ok is false when no ISP in "own" mode claims it — i.e. it's the shared
// platform paybill (or unknown).
func OrgForShortcode(code string) (orgID uint, ok bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		return 0, false
	}
	var cfg models.OrganizationMpesaConfig
	err := config.DB.
		Where("mode = ? AND (shortcode = ? OR till_number = ? OR paybill_number = ?)", "own", code, code, code).
		First(&cfg).Error
	if err != nil {
		return 0, false
	}
	return cfg.OrganizationID, true
}

// MpesaSTKResponse is the result of an STK push initiation.
type MpesaSTKResponse struct {
	Status              string `json:"status"`
	CheckoutRequestID   string `json:"checkout_request_id"`
	ResponseDescription string `json:"response_description"`
	IsMock              bool   `json:"is_mock"`
}

// paybillFor is the paybill/shortcode customers pay manually for the given
// creds (the STK push settles to the same number). An ISP on its own Daraja
// app with nothing configured gets "" — never Zyra's paybill, so their
// customers aren't told to pay someone else. Platform default is the shared
// paybill.
func paybillFor(creds mpesaCreds) string {
	if creds.Shortcode != "" {
		return creds.Shortcode
	}
	if creds.PaybillNumber != "" {
		return creds.PaybillNumber
	}
	if creds.Own {
		return ""
	}
	return "7289306"
}

// GetPaybillNumber returns the paybill customers of a zone pay manually.
func (s *MpesaService) GetPaybillNumber(zoneID uint) string {
	return paybillFor(s.ResolveMpesaCreds(zoneID))
}

// GetPaymentInfo returns how customers of a zone pay manually — the billing
// type ("paybill" | "till"), the paybill number and the till number — from a
// single credential resolution (this runs on every captive-portal load, so it
// must not resolve more than once).
func (s *MpesaService) GetPaymentInfo(zoneID uint) (billingType, paybill, till string) {
	creds := s.ResolveMpesaCreds(zoneID)
	if creds.DirectTill != "" {
		return "till", "", creds.DirectTill
	}
	if creds.DirectPaybill != "" {
		return "paybill", creds.DirectPaybill, ""
	}
	paybill = paybillFor(creds)
	if creds.BillingType == "till" && creds.TillNumber != "" {
		return "till", paybill, creds.TillNumber
	}
	return "paybill", paybill, ""
}

// getBaseURL returns the Daraja API base URL for the given environment.
func (s *MpesaService) getBaseURL(env string) string {
	if s.baseURLOverride != "" {
		return s.baseURLOverride
	}
	if strings.ToLower(env) == "production" {
		return "https://api.safaricom.co.ke"
	}
	return "https://sandbox.safaricom.co.ke"
}

// GetAccessToken fetches the OAuth token from Daraja for the given
// credentials, caching it in-memory (keyed by consumer key) for its
// GetAccessToken fetches the OAuth token from Daraja for the given
// credentials, caching it in-memory with automatic retry and transient 503 recovery.
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
	basicAuth := base64.StdEncoding.EncodeToString([]byte(creds.ConsumerKey + ":" + creds.ConsumerSecret))

	client := s.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	var lastErr error
	var lastStatus int
	var lastBody string

	// Safaricom Daraja proxy/upstream gateways frequently encounter transient 502/503 errors.
	// Perform up to 3 rapid retry attempts with backoff before failing.
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequest(http.MethodGet, apiURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Basic "+basicAuth)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(300*attempt) * time.Millisecond)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(300*attempt) * time.Millisecond)
			continue
		}

		lastStatus = resp.StatusCode
		lastBody = strings.TrimSpace(string(body))

		if resp.StatusCode == http.StatusOK {
			var result map[string]interface{}
			if err := json.Unmarshal(body, &result); err != nil {
				return "", fmt.Errorf("failed to decode daraja auth response: %w", err)
			}
			token, ok := result["access_token"].(string)
			if !ok || token == "" {
				return "", fmt.Errorf("no access_token in daraja response: %s", string(body))
			}

			expiresIn := 3500 * time.Second // safe default (~1h)
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

		// If transient upstream error (502/503/504), wait briefly and retry
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
			log.Printf("[Daraja Auth] Attempt %d: Safaricom returned %d (%s). Retrying...", attempt, resp.StatusCode, lastBody)
			time.Sleep(time.Duration(500*attempt) * time.Millisecond)
			continue
		}

		// For 400/401 auth errors, retry will not help; break early
		break
	}

	// In local development mode, fallback to mock token if Safaricom gateway is unreachable
	if config.Config.AppEnv == "local" {
		log.Printf("[Daraja Auth] APP_ENV=local fallback: using mock access token (upstream was %d: %s)", lastStatus, lastBody)
		return "mock_token_local_dev", nil
	}

	if lastErr != nil {
		return "", fmt.Errorf("daraja auth network connection failed: %w", lastErr)
	}

	if lastStatus == http.StatusServiceUnavailable || strings.Contains(lastBody, "no healthy upstream") {
		return "", fmt.Errorf("Safaricom Daraja M-Pesa is temporarily unreachable (503: no healthy upstream on %s). Verify if Daraja Environment in Settings matches your credentials (Sandbox vs Production) or retry shortly", creds.Env)
	}

	return "", fmt.Errorf("daraja auth failed (status %d on %s): %s", lastStatus, creds.Env, lastBody)
}

// appendCallbackToken adds ?token=<MPESA_CALLBACK_SECRET> (or &token= if the
// URL already has a query string) so mpesaCallbackAuthorized accepts the
// inbound webhook. No-op if the URL already carries a token.
func appendCallbackToken(url string) string {
	if url == "" || strings.Contains(url, "token=") {
		return url
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%stoken=%s", url, sep, config.Config.MpesaCallbackSecret)
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

	creds := s.ResolveMpesaCreds(zoneID)
	if creds.Own && !creds.mockAllowed() {
		if missing := creds.missingFields(); len(missing) > 0 {
			log.Printf("[M-Pesa] Zone %d: own Daraja config incomplete, refusing STK push (missing: %s)", zoneID, strings.Join(missing, ", "))
			return nil, errPaymentsUnavailable
		}
	}
	shortcode := creds.Shortcode
	passkey := creds.Passkey
	callbackURL := creds.CallbackURL
	env := creds.Env

	// Ensure callback URL carries secret token if configured so Safaricom webhooks are never rejected
	if config.Config.MpesaCallbackSecret != "" {
		callbackURL = appendCallbackToken(callbackURL)
	}

	token, err := s.GetAccessToken(creds)
	if err != nil {
		if creds.mockAllowed() && (strings.ToLower(env) != "production" || config.Config.AppEnv == "local") {
			log.Printf("[M-Pesa] GetAccessToken failed (%v) — falling back to mock STK Push", err)
			token = "mock_token"
		} else {
			log.Printf("[M-Pesa] Zone %d: Daraja auth failed: %v", zoneID, err)
			if creds.Own {
				return nil, errPaymentsUnavailable
			}
			return nil, err
		}
	}

	isLocalCallback := callbackURL == "" ||
		strings.Contains(callbackURL, "localhost") ||
		strings.Contains(callbackURL, "127.0.0.1") ||
		strings.Contains(callbackURL, "192.168.") ||
		!strings.HasPrefix(callbackURL, "https://")

	if token == "mock_token" || strings.ToLower(env) == "mock" || (strings.ToLower(env) != "production" && isLocalCallback) {
		if !creds.mockAllowed() {
			log.Printf("[M-Pesa] Zone %d: refusing to mock an own-Daraja payment on a deployed server (env=%q callback=%q)", zoneID, env, callbackURL)
			return nil, errPaymentsUnavailable
		}
		checkoutID := fmt.Sprintf("ws_CO_%d_%d", rand.Intn(999999)+100000, time.Now().Unix())
		log.Printf("[M-Pesa] Mock STK Push: phone=%s amount=%.0f ref=%s", phone, amount, reference)
		return &MpesaSTKResponse{
			Status:              "success",
			CheckoutRequestID:   checkoutID,
			ResponseDescription: "Mock STK Push initiated successfully",
			IsMock:              true,
		}, nil
	}

	if !creds.Direct() && creds.BillingType != "till" && creds.PaybillNumber != "" && creds.PaybillNumber != shortcode {
		log.Printf("[M-Pesa] Zone %d: paybill number %s differs from shortcode %s — STK push settles to the shortcode (Daraja requires PartyB to match it)", zoneID, creds.PaybillNumber, shortcode)
	}

	route := creds.route()
	transactionType := route.TransactionType
	partyB := route.PartyB
	// For Paybill STK prompt, display customer phone (e.g. 0758335592) as AccountReference
	accountReference := reference
	if phone != "" {
		cleanPhone := utils.FormatPhone(phone)
		if len(cleanPhone) == 12 && strings.HasPrefix(cleanPhone, "254") {
			accountReference = "0" + cleanPhone[3:]
		} else {
			accountReference = cleanPhone
		}
	}
	if creds.PaybillAccount != "" && creds.PaybillAccount != "ZYR_" {
		accountReference = creds.PaybillAccount
	}
	if creds.DirectPaybill != "" {
		accountReference = creds.DirectAccount // e.g. the ISP's bank account number
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
		client = &http.Client{Timeout: 10 * time.Second}
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
			"collected_via":        s.CollectionChannel(payment.ZoneID, receiptNumber),
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
			normalizedMac := strings.ToUpper(strings.ReplaceAll(payment.MacAddress, "-", ":"))
			go func() {
				err := s.whitelistWithRetry(&zone, normalizedMac, &pkg, 1)
				if err != nil {
					log.Printf("[M-Pesa] Async WhitelistMAC failed for %s (normal for routers behind NAT): %v", normalizedMac, err)
				} else {
					log.Printf("[M-Pesa] Successfully whitelisted MAC %s on router", normalizedMac)
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
		expiresAt := utils.CalculateExpiry(pkg.BillingCycle, customer.ExpiresAt, pkg.TimeLimitMinutes)
		custUpdates := map[string]interface{}{
			"status":     "active",
			"package_id": pkg.ID,
			"expires_at": expiresAt,
		}
		if payment.MacAddress != "" {
			custUpdates["mac_address"] = payment.MacAddress
		}
		if payment.ZoneID > 0 {
			custUpdates["zone_id"] = payment.ZoneID
		} else if pkg.ZoneID > 0 {
			custUpdates["zone_id"] = pkg.ZoneID
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
			}
		}

		config.DB.Model(customer).Updates(custUpdates)
		if s.Radius != nil {
			s.Radius.SyncCustomerAsync(customer.ID)
		}

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
	creds := s.ResolveMpesaCreds(zoneID)
	shortcode := creds.Shortcode
	passkey := creds.Passkey
	env := creds.Env

	if creds.Own && !creds.mockAllowed() && len(creds.missingFields()) > 0 {
		return nil, errPaymentsUnavailable
	}

	token, err := s.GetAccessToken(creds)
	if err != nil {
		return nil, err
	}

	if strings.ToLower(env) != "production" && token == "mock_token" && creds.mockAllowed() {
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
		client = &http.Client{Timeout: 6 * time.Second}
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
	if time.Since(payment.CreatedAt) < 3*time.Second {
		return "pending", nil
	}

	// Throttling check: only query Safaricom at most once every 2 seconds per checkout ID
	now := time.Now()
	if val, ok := s.queryThrottles.Load(checkoutID); ok {
		if lastTime, ok := val.(time.Time); ok && now.Sub(lastTime) < 2*time.Second {
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

// CollectionChannel says where a just-completed M-Pesa payment's money landed,
// for the payout ledger (see models.Payment.CollectedVia): "platform" for
// Zyra Net's shared shortcode in production, "own" for an ISP's own Daraja app,
// and "" for anything that isn't real settled money (mock/sandbox).
func (s *MpesaService) CollectionChannel(zoneID uint, receipt string) string {
	if strings.HasPrefix(strings.ToUpper(receipt), "MOCK") {
		return ""
	}
	creds := s.ResolveMpesaCreds(zoneID)
	if creds.Own {
		return "own"
	}
	if creds.Direct() {
		return "direct" // paid straight to the ISP — never part of a payout
	}
	if strings.ToLower(creds.Env) != "production" {
		return ""
	}
	return "platform"
}

// PlatformIsProduction reports whether the shared Daraja app is live (as
// opposed to sandbox/mock), i.e. whether money on it is real.
func (s *MpesaService) PlatformIsProduction() bool {
	return strings.ToLower(s.ResolveMpesaCreds(0).Env) == "production"
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

// loadMpesaSettingsMap loads all M-Pesa configuration settings in a single SQL query.
func (s *MpesaService) loadMpesaSettingsMap() map[string]string {
	if m, ok := cachedSettings(); ok {
		return m
	}
	m := s.queryMpesaSettingsMap()
	storeSettings(m)
	return m
}

func (s *MpesaService) queryMpesaSettingsMap() map[string]string {
	keys := []string{
		"mpesa_consumer_key", "mpesa_consumer_secret", "mpesa_shortcode",
		"mpesa_passkey", "mpesa_callback_url", "mpesa_environment",
		"mpesa_billing_type", "mpesa_till_number", "mpesa_paybill_number",
		"mpesa_paybill_account",
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
	creds := s.ResolveMpesaCreds(zoneID)
	if creds.Own && !creds.mockAllowed() && len(creds.missingFields()) > 0 {
		return nil, fmt.Errorf("your Daraja settings are incomplete (missing: %s)", strings.Join(creds.missingFields(), ", "))
	}
	shortcode := creds.c2bShortcode()
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

	// Match InitiateSTKPush: append the shared secret so mpesaCallbackAuthorized
	// (handlers/mpesa.go) doesn't reject the C2B validation/confirmation hits
	// Safaricom sends to these URLs.
	if config.Config.MpesaCallbackSecret != "" {
		confirmationURL = appendCallbackToken(confirmationURL)
		validationURL = appendCallbackToken(validationURL)
	}

	token, err := s.GetAccessToken(creds)
	if err != nil {
		if creds.mockAllowed() && (strings.ToLower(creds.Env) != "production" || config.Config.AppEnv == "local" || config.Config.AppEnv == "test") {
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
		if !creds.mockAllowed() {
			return nil, fmt.Errorf("C2B URL registration was not sent to Safaricom: check your environment is set to production and the URLs are public https")
		}
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
