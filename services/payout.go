package services

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	"gorm.io/gorm"
)

// Payouts move an ISP's share out of Zyra Net's shared shortcode. The design
// rule throughout: money is never sent twice and never lost track of.
//
//   - A payout *claims* payments (Payment.PayoutID) with a single conditional
//     UPDATE, so concurrent payouts can't both take the same payment.
//   - Every status change is a conditional UPDATE (…WHERE status = <expected>),
//     so a double-click, retry or duplicate callback is a harmless no-op.
//   - A B2B request whose outcome is unknown (timeout, 5xx) is left as
//     "processing" for a human to reconcile against the M-Pesa statement —
//     it is never retried automatically.

var (
	ErrPayoutNoDestination    = errors.New("this ISP has not set a settlement destination yet")
	ErrPayoutOpen             = errors.New("this ISP already has a payout in progress — finish or cancel it first")
	ErrPayoutNothingOwed      = errors.New("nothing is owed to this ISP right now")
	ErrPayoutBelowMinimum     = errors.New("the amount owed is below the minimum payout")
	ErrPayoutNotInState       = errors.New("the payout is not in a state that allows this action")
	ErrPayoutB2BNotConfigured = errors.New("M-Pesa B2B is not configured")
	// Without MPESA_CALLBACK_SECRET the result callback URL is unauthenticated
	// and anyone could forge a "payout completed" message.
	ErrPayoutCallbackSecretMissing = errors.New("set MPESA_CALLBACK_SECRET on the server before sending payouts: without it Safaricom's result callback can't be authenticated")
)

// PayoutService creates, prices and sends payouts.
type PayoutService struct {
	Mpesa *MpesaService
}

func NewPayoutService(m *MpesaService) *PayoutService { return &PayoutService{Mpesa: m} }

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Price applies a commission percentage to a gross amount and returns the
// commission, the net owed, and the whole-shilling amount to send (net rounded
// down; Daraja B2B takes integers).
func Price(gross, commissionPercent float64) (commission, net float64, payout int64) {
	if commissionPercent < 0 {
		commissionPercent = 0
	}
	if commissionPercent > 100 {
		commissionPercent = 100
	}
	commission = round2(gross * commissionPercent / 100)
	net = round2(gross - commission)
	payout = int64(math.Floor(net + 1e-9))
	return commission, net, payout
}

// EffectiveCommission is the ISP's own override if set, else the platform default.
func EffectiveCommission(org *models.Organization, platformDefault float64) float64 {
	pct := platformDefault
	if org.CommissionPercent != nil {
		pct = *org.CommissionPercent
	}
	return math.Min(100, math.Max(0, pct))
}

// Destination validates and returns where an ISP's payouts go.
func Destination(org *models.Organization) (typ, till, paybill, account string, err error) {
	switch org.SettlementType {
	case "till":
		if strings.TrimSpace(org.SettlementTillNumber) == "" {
			return "", "", "", "", ErrPayoutNoDestination
		}
		return "till", strings.TrimSpace(org.SettlementTillNumber), "", "", nil
	case "paybill":
		if strings.TrimSpace(org.SettlementPaybillNumber) == "" || strings.TrimSpace(org.SettlementAccountNumber) == "" {
			return "", "", "", "", ErrPayoutNoDestination
		}
		return "paybill", "", strings.TrimSpace(org.SettlementPaybillNumber), strings.TrimSpace(org.SettlementAccountNumber), nil
	}
	return "", "", "", "", ErrPayoutNoDestination
}

// UnsettledPayments is the ledger query: completed payments collected on the
// shared shortcode for an ISP's zones that no payout has claimed yet. (Zones
// deleted since still count — the money was collected.)
func UnsettledPayments(db *gorm.DB, orgID uint) *gorm.DB {
	zoneIDs := db.Unscoped().Model(&models.Zone{}).Select("id").Where("organization_id = ?", orgID)
	return db.Model(&models.Payment{}).
		Where("status = ? AND collected_via = ? AND payout_id IS NULL AND zone_id IN (?)", "completed", "platform", zoneIDs)
}

// Unsettled returns how many payments and how much gross money is owed to an ISP.
func Unsettled(orgID uint) (count int64, gross float64) {
	UnsettledPayments(config.DB, orgID).Count(&count)
	UnsettledPayments(config.DB, orgID).Select("COALESCE(SUM(amount), 0)").Scan(&gross)
	return count, round2(gross)
}

// CreatePayout claims everything currently owed to an ISP into a new pending
// payout, priced with the commission and snapshotting the destination.
func (s *PayoutService) CreatePayout(orgID, staffID uint, defaultCommission float64, minAmount int64) (*models.Payout, error) {
	var org models.Organization
	if err := config.DB.First(&org, orgID).Error; err != nil {
		return nil, err
	}
	typ, till, paybill, account, err := Destination(&org)
	if err != nil {
		return nil, err
	}

	var open int64
	config.DB.Model(&models.Payout{}).
		Where("organization_id = ? AND status IN ?", orgID, []string{"pending", "processing"}).Count(&open)
	if open > 0 {
		return nil, ErrPayoutOpen
	}

	payout := models.Payout{
		OrganizationID: orgID, Status: "pending",
		DestType: typ, DestTill: till, DestPaybill: paybill, DestAccount: account,
		CreatedByPlatformUserID: staffID,
	}
	err = config.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&payout).Error; err != nil {
			return err
		}
		// Claim: one conditional UPDATE, so payments taken by a concurrent
		// payout (payout_id no longer NULL) are simply not matched.
		res := UnsettledPayments(tx, orgID).Update("payout_id", payout.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrPayoutNothingOwed
		}
		var gross float64
		if err := tx.Model(&models.Payment{}).Where("payout_id = ?", payout.ID).
			Select("COALESCE(SUM(amount), 0)").Scan(&gross).Error; err != nil {
			return err
		}
		gross = round2(gross)
		pct := EffectiveCommission(&org, defaultCommission)
		commission, net, pay := Price(gross, pct)
		if pay < 1 || pay < minAmount {
			return ErrPayoutBelowMinimum // rolls back the claim too
		}
		payout.GrossAmount, payout.CommissionPercent, payout.CommissionAmount = gross, pct, commission
		payout.NetAmount, payout.PayoutAmount, payout.PaymentCount = net, pay, int(res.RowsAffected)
		return tx.Model(&payout).Updates(map[string]interface{}{
			"gross_amount": gross, "commission_percent": pct, "commission_amount": commission,
			"net_amount": net, "payout_amount": pay, "payment_count": int(res.RowsAffected),
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return &payout, nil
}

// transition moves a payout between statuses only if it is currently in one of
// the expected states. It reports whether this call made the change.
func transition(tx *gorm.DB, id uint, from []string, updates map[string]interface{}) (bool, error) {
	res := tx.Model(&models.Payout{}).Where("id = ? AND status IN ?", id, from).Updates(updates)
	return res.RowsAffected == 1, res.Error
}

func releasePayments(tx *gorm.DB, payoutID uint) error {
	return tx.Model(&models.Payment{}).Where("payout_id = ?", payoutID).Update("payout_id", nil).Error
}

// Cancel abandons a pending payout and returns its payments to the ledger.
func (s *PayoutService) Cancel(id uint) error {
	return config.DB.Transaction(func(tx *gorm.DB) error {
		ok, err := transition(tx, id, []string{"pending"}, map[string]interface{}{"status": "cancelled"})
		if err != nil {
			return err
		}
		if !ok {
			return ErrPayoutNotInState
		}
		return releasePayments(tx, id)
	})
}

// Fail declares a stuck "processing" payout as not paid (staff checked the
// M-Pesa statement) and returns its payments to the ledger.
func (s *PayoutService) Fail(id uint, reason string) error {
	return config.DB.Transaction(func(tx *gorm.DB) error {
		ok, err := transition(tx, id, []string{"processing"}, map[string]interface{}{
			"status": "failed", "failure_reason": truncate(reason, 500),
		})
		if err != nil {
			return err
		}
		if !ok {
			return ErrPayoutNotInState
		}
		return releasePayments(tx, id)
	})
}

// MarkPaid completes a payout that was paid outside the system (or confirmed
// against the statement), recording the M-Pesa/bank reference.
func (s *PayoutService) MarkPaid(id, staffID uint, reference string) error {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("a payment reference is required")
	}
	now := time.Now()
	ok, err := transition(config.DB, id, []string{"pending", "processing"}, map[string]interface{}{
		"status": "completed", "reference": truncate(reference, 100), "completed_at": now,
		"sent_by_platform_user_id": staffID,
		"method":                   gorm.Expr("CASE WHEN method = '' OR method IS NULL THEN 'manual' ELSE method END"),
	})
	if err != nil {
		return err
	}
	if !ok {
		return ErrPayoutNotInState
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------- B2B ----

type b2bConfig struct {
	Initiator, Password, CertPEM string
}

func (s *PayoutService) b2bConfig() (b2bConfig, error) {
	get := func(key string) string {
		var st models.Setting
		if err := config.DB.Where("`key` = ?", key).First(&st).Error; err == nil && st.Value != nil {
			return strings.TrimSpace(*st.Value)
		}
		return ""
	}
	c := b2bConfig{Initiator: get("mpesa_b2b_initiator"), Password: get("mpesa_b2b_password"), CertPEM: get("mpesa_b2b_cert")}
	var missing []string
	if c.Initiator == "" {
		missing = append(missing, "initiator name")
	}
	if c.Password == "" {
		missing = append(missing, "initiator password")
	}
	if c.CertPEM == "" {
		missing = append(missing, "Safaricom certificate")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("%w: missing %s", ErrPayoutB2BNotConfigured, strings.Join(missing, ", "))
	}
	return c, nil
}

// SecurityCredential encrypts the initiator password with Safaricom's public
// certificate (RSA PKCS#1 v1.5, base64) as Daraja's B2B API requires.
func SecurityCredential(password, certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return "", errors.New("the Safaricom certificate is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("could not parse the Safaricom certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("the Safaricom certificate does not hold an RSA key")
	}
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(password))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// b2bPayload builds the Daraja B2B paymentrequest body for a payout.
func b2bPayload(p *models.Payout, initiator, securityCredential, partyA, resultURL, timeoutURL string) map[string]interface{} {
	command, partyB, account := "BusinessPayBill", p.DestPaybill, p.DestAccount
	if p.DestType == "till" {
		command, partyB, account = "BusinessBuyGoods", p.DestTill, ""
	}
	payload := map[string]interface{}{
		"Initiator":              initiator,
		"SecurityCredential":     securityCredential,
		"CommandID":              command,
		"SenderIdentifierType":   "4",
		"RecieverIdentifierType": "4", // (sic) Daraja's spelling
		"Amount":                 fmt.Sprintf("%d", p.PayoutAmount),
		"PartyA":                 partyA,
		"PartyB":                 partyB,
		"Remarks":                fmt.Sprintf("Zyra Net payout %d", p.ID),
		"QueueTimeOutURL":        timeoutURL,
		"ResultURL":              resultURL,
	}
	if account != "" {
		payload["AccountReference"] = account
	}
	return payload
}

type b2bOutcome int

const (
	b2bAccepted b2bOutcome = iota // Safaricom queued it; the result arrives by callback
	b2bRejected                   // definitely not sent — safe to retry later
	b2bUnknown                    // may or may not have been sent — a human must check
)

// classifyB2BResponse decides what a synchronous Daraja reply means. Only a
// clear rejection may be retried; anything ambiguous must not be.
func classifyB2BResponse(status int, body []byte) (out b2bOutcome, originatorID, conversationID, msg string) {
	var r struct {
		OriginatorConversationID string `json:"OriginatorConversationID"`
		ConversationID           string `json:"ConversationID"`
		ResponseCode             string `json:"ResponseCode"`
		ResponseDescription      string `json:"ResponseDescription"`
		ErrorMessage             string `json:"errorMessage"`
	}
	parsed := json.Unmarshal(body, &r) == nil
	msg = r.ResponseDescription
	if msg == "" {
		msg = r.ErrorMessage
	}
	switch {
	// A "0" (success) reply is only trusted when it names the conversation the
	// result callback will refer to; "0" without one falls through to unknown.
	case status >= 200 && status < 300 && parsed && r.ResponseCode == "0" && r.OriginatorConversationID != "":
		return b2bAccepted, r.OriginatorConversationID, r.ConversationID, msg
	case status >= 200 && status < 300 && parsed && r.ResponseCode != "" && r.ResponseCode != "0":
		return b2bRejected, "", "", msg
	case status >= 400 && status < 500:
		if msg == "" {
			msg = fmt.Sprintf("Daraja rejected the request (HTTP %d)", status)
		}
		return b2bRejected, "", "", msg
	}
	return b2bUnknown, "", "", msg
}

// payoutCallbackURL derives a callback URL on the same host as the configured
// STK callback, carrying the shared secret so mpesaCallbackAuthorized accepts it.
func payoutCallbackURL(stkCallbackURL, path string) (string, error) {
	if config.Config.MpesaCallbackSecret == "" {
		return "", ErrPayoutCallbackSecretMissing
	}
	u, err := url.Parse(strings.TrimSpace(stkCallbackURL))
	if err != nil || u.Host == "" || u.Scheme != "https" {
		return "", errors.New("set a public https Callback URL in the platform Daraja settings first")
	}
	return appendCallbackToken(fmt.Sprintf("%s://%s/api/v1/payouts/b2b/%s", u.Scheme, u.Host, path)), nil
}

// SendB2B sends a pending payout through Daraja B2B. It returns the payout as
// it stands afterwards; an error means it was either not sent (payout back to
// pending, safe to retry) or its outcome is unknown (payout left processing —
// reconcile manually, never retry).
func (s *PayoutService) SendB2B(id, staffID uint) (*models.Payout, error) {
	var payout models.Payout
	if err := config.DB.First(&payout, id).Error; err != nil {
		return nil, err
	}
	if payout.Status != "pending" {
		return &payout, ErrPayoutNotInState
	}

	cfg, err := s.b2bConfig()
	if err != nil {
		return &payout, err
	}
	secCred, err := SecurityCredential(cfg.Password, cfg.CertPEM)
	if err != nil {
		return &payout, err
	}
	creds := s.Mpesa.ResolveMpesaCreds(0)
	if creds.ConsumerKey == "" || creds.ConsumerKey == "mock_consumer_key" || creds.Shortcode == "" {
		return &payout, errors.New("the shared Daraja app (consumer key and shortcode) is not configured")
	}
	resultURL, err := payoutCallbackURL(creds.CallbackURL, "result")
	if err != nil {
		return &payout, err
	}
	timeoutURL, _ := payoutCallbackURL(creds.CallbackURL, "timeout")
	token, err := s.Mpesa.GetAccessToken(creds)
	if err != nil {
		return &payout, fmt.Errorf("could not authenticate with Daraja: %w", err)
	}
	if strings.HasPrefix(token, "mock_token") {
		return &payout, errors.New("refusing to send a payout with mock Daraja credentials")
	}

	// From here a request may leave the building: take the payout out of
	// "pending" first so nothing else can send it.
	ok, err := transition(config.DB, id, []string{"pending"}, map[string]interface{}{
		"status": "processing", "method": "b2b", "sent_by_platform_user_id": staffID, "failure_reason": "",
	})
	if err != nil {
		return &payout, err
	}
	if !ok {
		return &payout, ErrPayoutNotInState
	}
	config.DB.First(&payout, id)

	body, _ := json.Marshal(b2bPayload(&payout, cfg.Initiator, secCred, creds.Shortcode, resultURL, timeoutURL))
	req, err := http.NewRequest(http.MethodPost, s.Mpesa.getBaseURL(creds.Env)+"/mpesa/b2b/v1/paymentrequest", bytes.NewReader(body))
	if err != nil {
		s.revertToPending(id, "could not build the request: "+err.Error())
		config.DB.First(&payout, id)
		return &payout, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.Mpesa.httpClient.Do(req)
	if err != nil {
		// The request may have reached Safaricom. Do NOT revert or retry.
		s.noteUnknown(id, "the request outcome is unknown ("+err.Error()+") — check the M-Pesa statement, then mark it paid or failed")
		config.DB.First(&payout, id)
		return &payout, fmt.Errorf("outcome unknown: %w", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	outcome, orig, conv, msg := classifyB2BResponse(resp.StatusCode, respBody)
	log.Printf("[payout %d] B2B response HTTP %d outcome=%d msg=%q", id, resp.StatusCode, outcome, msg)
	switch outcome {
	case b2bAccepted:
		config.DB.Model(&models.Payout{}).Where("id = ?", id).Updates(map[string]interface{}{
			"originator_conversation_id": orig, "conversation_id": conv, "result_desc": truncate(msg, 255),
		})
		config.DB.First(&payout, id)
		return &payout, nil
	case b2bRejected:
		s.revertToPending(id, "Safaricom rejected the request: "+msg)
		config.DB.First(&payout, id)
		return &payout, fmt.Errorf("Safaricom rejected the payout: %s", msg)
	default:
		s.noteUnknown(id, fmt.Sprintf("Safaricom replied HTTP %d with an unclear result (%s) — check the M-Pesa statement, then mark it paid or failed", resp.StatusCode, msg))
		config.DB.First(&payout, id)
		return &payout, errors.New("outcome unknown: Safaricom's reply was unclear")
	}
}

func (s *PayoutService) revertToPending(id uint, reason string) {
	transition(config.DB, id, []string{"processing"}, map[string]interface{}{
		"status": "pending", "method": "", "sent_by_platform_user_id": nil, "failure_reason": truncate(reason, 500),
	})
}

func (s *PayoutService) noteUnknown(id uint, reason string) {
	config.DB.Model(&models.Payout{}).Where("id = ? AND status = ?", id, "processing").
		Update("failure_reason", truncate(reason, 500))
}

func numberOf(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func stringOf(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	}
	return ""
}

// HandleB2BResult applies Safaricom's asynchronous result to a payout. It is
// idempotent: a duplicate callback finds the payout no longer "processing" and
// changes nothing.
func (s *PayoutService) HandleB2BResult(payload map[string]interface{}) error {
	result, ok := payload["Result"].(map[string]interface{})
	if !ok {
		return errors.New("invalid B2B result: missing Result")
	}
	orig, conv := stringOf(result["OriginatorConversationID"]), stringOf(result["ConversationID"])

	var payout models.Payout
	q := config.DB.Where("1 = 0")
	switch {
	case orig != "":
		q = config.DB.Where("originator_conversation_id = ?", orig)
	case conv != "":
		q = config.DB.Where("conversation_id = ?", conv)
	}
	if err := q.First(&payout).Error; err != nil {
		return fmt.Errorf("no payout matches conversation %q/%q", orig, conv)
	}

	code, _ := numberOf(result["ResultCode"])
	desc := stringOf(result["ResultDesc"])
	if code == 0 {
		now := time.Now()
		_, err := transition(config.DB, payout.ID, []string{"processing"}, map[string]interface{}{
			"status": "completed", "reference": truncate(stringOf(result["TransactionID"]), 100),
			"result_desc": truncate(desc, 255), "completed_at": now, "failure_reason": "",
		})
		return err
	}
	return config.DB.Transaction(func(tx *gorm.DB) error {
		ok, err := transition(tx, payout.ID, []string{"processing"}, map[string]interface{}{
			"status": "failed", "result_desc": truncate(desc, 255),
			"failure_reason": truncate(fmt.Sprintf("Safaricom result %.0f: %s", code, desc), 500),
		})
		if err != nil || !ok {
			return err
		}
		return releasePayments(tx, payout.ID)
	})
}

// HandleB2BTimeout records that Safaricom's queue timed out. The payout stays
// "processing": the transfer may still complete, so staff must check.
func (s *PayoutService) HandleB2BTimeout(payload map[string]interface{}) {
	result, _ := payload["Result"].(map[string]interface{})
	orig := stringOf(result["OriginatorConversationID"])
	if orig == "" {
		return
	}
	config.DB.Model(&models.Payout{}).Where("originator_conversation_id = ? AND status = ?", orig, "processing").
		Update("failure_reason", "Safaricom's queue timed out — the transfer may still complete. Check the M-Pesa statement, then mark it paid or failed.")
}
