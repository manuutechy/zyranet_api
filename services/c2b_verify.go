package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Paybill (C2B) confirmations for the live shared paybill arrive WITHOUT the
// callback token: its URLs were registered with Safaricom before the token
// existed, and production re-registration can't be relied on. Anyone who
// learns the confirmation URL could therefore POST a made-up "KES 1,000
// received" and mint credit. So a confirmation without a valid token is never
// credited on its own word: we ask Safaricom directly (Daraja Transaction
// Status, using the platform's B2B initiator) and credit only if Safaricom's
// own record of that TransID matches — completed, same amount, paid into the
// same shortcode, from a compatible phone. Anything that can't be checked is
// parked in unmatched_c2b_payments for a person to confirm against the M-Pesa
// statement. Nothing is ever credited "just in case".

// ErrC2BVerifyUnavailable means the Transaction Status check can't be made
// (no initiator configured, sandbox/mock credentials, an ISP's own shortcode we
// hold no initiator for, …). The payment must be parked for manual review.
var ErrC2BVerifyUnavailable = errors.New("automatic verification is not available")

// RequestC2BVerification asks Safaricom for its own record of a paybill
// transaction. The answer arrives asynchronously on /c2b/verify/result (with
// the callback token); the returned OriginatorConversationID ties it back.
func (s *MpesaService) RequestC2BVerification(transID, shortcode string) (string, error) {
	creds := s.ResolveMpesaCreds(0)
	platformCodes := map[string]bool{}
	for _, c := range []string{creds.Shortcode, creds.PaybillNumber, creds.TillNumber} {
		if c = strings.TrimSpace(c); c != "" {
			platformCodes[c] = true
		}
	}
	shortcode = strings.TrimSpace(shortcode)
	if !platformCodes[shortcode] {
		return "", fmt.Errorf("%w: shortcode %q is not the platform's (no initiator for it)", ErrC2BVerifyUnavailable, shortcode)
	}
	if strings.ToLower(creds.Env) != "production" || creds.ConsumerKey == "" || creds.ConsumerKey == "mock_consumer_key" {
		return "", fmt.Errorf("%w: the platform Daraja app is not in production", ErrC2BVerifyUnavailable)
	}
	cfg, err := (&PayoutService{}).b2bConfig()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrC2BVerifyUnavailable, err)
	}
	secCred, err := SecurityCredential(cfg.Password, cfg.CertPEM)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrC2BVerifyUnavailable, err)
	}
	resultURL, err := c2bVerifyCallbackURL(creds.CallbackURL, "result")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrC2BVerifyUnavailable, err)
	}
	timeoutURL, _ := c2bVerifyCallbackURL(creds.CallbackURL, "timeout")
	token, err := s.GetAccessToken(creds)
	if err != nil {
		return "", fmt.Errorf("daraja auth failed: %w", err)
	}
	if strings.HasPrefix(token, "mock_token") {
		return "", fmt.Errorf("%w: mock Daraja token", ErrC2BVerifyUnavailable)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"Initiator":          cfg.Initiator,
		"SecurityCredential": secCred,
		"CommandID":          "TransactionStatusQuery",
		"TransactionID":      transID,
		"PartyA":             shortcode,
		"IdentifierType":     "4",
		"ResultURL":          resultURL,
		"QueueTimeOutURL":    timeoutURL,
		"Remarks":            "C2B verification",
		"Occasion":           "",
	})
	req, err := http.NewRequest(http.MethodPost, s.getBaseURL(creds.Env)+"/mpesa/transactionstatus/v1/query", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("transaction status request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var r struct {
		OriginatorConversationID string `json:"OriginatorConversationID"`
		ResponseCode             string `json:"ResponseCode"`
		ResponseDescription      string `json:"ResponseDescription"`
		ErrorMessage             string `json:"errorMessage"`
	}
	if json.Unmarshal(respBody, &r) != nil || r.ResponseCode != "0" || r.OriginatorConversationID == "" {
		msg := r.ResponseDescription
		if msg == "" {
			msg = r.ErrorMessage
		}
		return "", fmt.Errorf("Safaricom did not accept the status query (HTTP %d): %s", resp.StatusCode, truncate(msg, 200))
	}
	return r.OriginatorConversationID, nil
}

// c2bVerifyCallbackURL derives the verification result URL on the same host
// as the STK callback, carrying the shared secret.
func c2bVerifyCallbackURL(stkCallbackURL, path string) (string, error) {
	if config.Config.MpesaCallbackSecret == "" {
		return "", errors.New("MPESA_CALLBACK_SECRET is not set")
	}
	u, err := url.Parse(strings.TrimSpace(stkCallbackURL))
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return "", errors.New("the platform Daraja callback URL is not a public https URL")
	}
	return appendCallbackToken(fmt.Sprintf("%s://%s/api/v1/c2b/verify/%s", u.Scheme, u.Host, path)), nil
}

// C2BStatusResult is Safaricom's Transaction Status answer for one TransID.
type C2BStatusResult struct {
	OriginatorConversationID string
	ResultCode               float64
	ResultDesc               string
	ReceiptNo                string
	TransactionStatus        string
	Amount                   float64
	HasAmount                bool
	CreditPartyName          string
	DebitPartyName           string
}

// ParseTransactionStatusResult reads a Transaction Status result callback.
func ParseTransactionStatusResult(payload map[string]interface{}) (C2BStatusResult, error) {
	var r C2BStatusResult
	result, ok := payload["Result"].(map[string]interface{})
	if !ok {
		return r, errors.New("invalid transaction status result: missing Result")
	}
	r.OriginatorConversationID = stringOf(result["OriginatorConversationID"])
	r.ResultCode, _ = numberOf(result["ResultCode"])
	r.ResultDesc = stringOf(result["ResultDesc"])
	params, _ := result["ResultParameters"].(map[string]interface{})
	var list []interface{}
	switch p := params["ResultParameter"].(type) {
	case []interface{}:
		list = p
	case map[string]interface{}:
		list = []interface{}{p}
	}
	for _, raw := range list {
		kv, _ := raw.(map[string]interface{})
		key := stringOf(kv["Key"])
		val := kv["Value"]
		switch key {
		case "ReceiptNo":
			r.ReceiptNo = stringOf(val)
		case "TransactionStatus":
			r.TransactionStatus = stringOf(val)
		case "Amount":
			if f, ok := numberOf(val); ok {
				r.Amount, r.HasAmount = f, true
			}
		case "CreditPartyName":
			r.CreditPartyName = stringOf(val)
		case "DebitPartyName":
			r.DebitPartyName = stringOf(val)
		}
	}
	return r, nil
}

// Matches reports whether Safaricom's record agrees with the confirmation we
// were sent. why explains the first disagreement.
func (r C2BStatusResult) Matches(l *models.C2BTransaction) (ok bool, why string) {
	if r.ResultCode != 0 {
		return false, fmt.Sprintf("Safaricom result %.0f: %s", r.ResultCode, r.ResultDesc)
	}
	if r.ReceiptNo != "" && !strings.EqualFold(strings.TrimSpace(r.ReceiptNo), l.TransID) {
		return false, fmt.Sprintf("receipt %s is not %s", r.ReceiptNo, l.TransID)
	}
	if !strings.EqualFold(strings.TrimSpace(r.TransactionStatus), "Completed") {
		return false, fmt.Sprintf("transaction status is %q, not Completed", r.TransactionStatus)
	}
	if !r.HasAmount || math.Abs(r.Amount-l.Amount) > 0.009 {
		return false, fmt.Sprintf("Safaricom says KES %.2f, the confirmation said KES %.2f", r.Amount, l.Amount)
	}
	if !strings.HasPrefix(strings.TrimSpace(r.CreditPartyName), strings.TrimSpace(l.BusinessShortCode)) {
		return false, fmt.Sprintf("paid to %q, not shortcode %s", r.CreditPartyName, l.BusinessShortCode)
	}
	if !phonesCompatible(l.MSISDN, r.DebitPartyName) {
		return false, fmt.Sprintf("payer %q doesn't match MSISDN %s", r.DebitPartyName, l.MSISDN)
	}
	return true, ""
}

// phonesCompatible compares the confirmation's MSISDN with Safaricom's
// "2547XXXXX123 - JANE DOE" debit party. Safaricom masks part of the number
// (and C2B v2 hashes the MSISDN entirely), so only digits visible on both sides
// are compared; if either side has no usable number the check passes and the
// amount/shortcode/receipt checks carry the decision.
func phonesCompatible(msisdn, debitParty string) bool {
	a := utils.FormatPhone(msisdn)
	if len(a) != 12 || len(msisdn) > 15 {
		return true // hashed or not a phone
	}
	party := strings.TrimSpace(strings.SplitN(debitParty, "-", 2)[0])
	party = strings.ReplaceAll(party, " ", "")
	if party == "" {
		return true
	}
	// Normalise a visible 07…/7… prefix to 254… without touching mask chars.
	switch {
	case strings.HasPrefix(party, "0"):
		party = "254" + party[1:]
	case len(party) == 9:
		party = "254" + party
	}
	if len(party) != len(a) {
		return true
	}
	for i := range party {
		ch := party[i]
		if ch >= '0' && ch <= '9' && ch != a[i] {
			return false
		}
	}
	return true
}

// ParkC2B holds a confirmation nobody has vouched for (or whose check failed)
// in unmatched_c2b_payments for a person to confirm. Nothing is credited.
func ParkC2B(l *models.C2BTransaction, note string) {
	note = truncate(note, 500)
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.C2BTransaction{}).
			Where("id = ? AND status IN ?", l.ID, []string{"received", "verifying"}).
			Updates(map[string]interface{}{"status": "parked", "verify_note": note})
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.UnmatchedC2BPayment{
			TransID:           l.TransID,
			Phone:             utils.FormatPhone(l.MSISDN),
			Amount:            l.Amount,
			BillRefNumber:     l.BillRefNumber,
			BusinessShortCode: l.BusinessShortCode,
			FirstName:         l.FirstName,
			LastName:          strings.TrimSpace(l.MiddleName + " " + l.LastName),
			Status:            "pending",
			Verification:      "unverified",
			Note:              note,
		}).Error
	})
	if err != nil {
		log.Printf("[C2B] could not park %s for review: %v", l.TransID, err)
		return
	}
	l.Status = "parked"
	log.Printf("[C2B] %s (KES %.2f) parked for manual review — not credited: %s", l.TransID, l.Amount, note)
}

// ParkStaleC2BVerifications parks confirmations whose Safaricom check never
// came back (lost callback, API restart mid-request).
func ParkStaleC2BVerifications(olderThan time.Duration) {
	var stale []models.C2BTransaction
	config.DB.Where("status IN ? AND updated_at < ?", []string{"received", "verifying"}, time.Now().Add(-olderThan)).
		Limit(100).Find(&stale)
	for i := range stale {
		ParkC2B(&stale[i], "Safaricom's verification answer never arrived — check the M-Pesa statement before crediting")
	}
}
