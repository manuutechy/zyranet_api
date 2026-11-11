package services

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// CyzoraOutcome is what Cyzora says about a charge — from a webhook or from
// GET /charges/{id}. Amount is in cents.
type CyzoraOutcome struct {
	ChargeID    string
	Status      string
	AmountCents int64
	Receipt     string
	Error       string
	ResultCode  string
	Livemode    *bool
}

func cyzoraOutcomeFromCharge(ch *CyzoraCharge) CyzoraOutcome {
	o := CyzoraOutcome{ChargeID: ch.ID, Status: ch.Status, AmountCents: ch.Amount, Livemode: ch.Livemode}
	if ch.MpesaReceipt != nil {
		o.Receipt = *ch.MpesaReceipt
	}
	if ch.ResultCode != nil {
		o.ResultCode = *ch.ResultCode
	}
	return o
}

// CyzoraWebhookEvent is the body Cyzora posts for charge events.
type CyzoraWebhookEvent struct {
	Event        string            `json:"event"`
	ChargeID     string            `json:"charge_id"`
	Reference    string            `json:"reference"`
	Amount       int64             `json:"amount"` // cents
	Currency     string            `json:"currency"`
	Status       string            `json:"status"`
	MpesaReceipt string            `json:"mpesa_receipt"`
	Error        string            `json:"error"`
	ResultCode   string            `json:"result_code"`
	Livemode     *bool             `json:"livemode"`
	Metadata     map[string]string `json:"metadata"`
	// Some deliveries may carry the metadata flattened.
	ZyraPaymentID string `json:"zyra_payment_id"`
}

// cyzoraCollectedVia: Cyzora collects into Zyra Net's own Cyzora wallet, so a
// Cyzora payment is platform-collected money — exactly like the shared
// paybill — that the ISP is owed through the payout ledger (collected_via =
// "platform"), whatever the ISP's Daraja mode or direct-settlement setting.
// A Cyzora test key only simulates payments: no real money, never paid out.
func cyzoraCollectedVia() string {
	if CyzoraKeyMode(CyzoraSecretKey()) == "test" {
		return ""
	}
	return "platform"
}

// collectedVia says where a completing payment's money landed (see
// models.Payment.CollectedVia), for either provider.
func (s *MpesaService) collectedVia(p *models.Payment, receipt string) string {
	if p.Provider == ProviderCyzora {
		return cyzoraCollectedVia()
	}
	return s.CollectionChannel(p.ZoneID, receipt)
}

// FindCyzoraPayment finds the payment a Cyzora charge belongs to: by charge
// id, else by metadata.zyra_payment_id (only among Cyzora payments, and only
// when that payment isn't already tied to a different charge).
func FindCyzoraPayment(chargeID, zyraPaymentID string) (*models.Payment, string) {
	chargeID = strings.TrimSpace(chargeID)
	var p models.Payment
	if chargeID != "" {
		if config.DB.Where("provider_ref = ?", chargeID).First(&p).Error == nil {
			return &p, ""
		}
		if config.DB.Where("provider = ? AND mpesa_transaction_id = ?", ProviderCyzora, chargeID).First(&p).Error == nil {
			return &p, ""
		}
	}
	id, err := strconv.ParseUint(strings.TrimSpace(zyraPaymentID), 10, 64)
	if err != nil || id == 0 {
		return nil, "unknown charge"
	}
	if config.DB.Where("id = ? AND provider = ?", id, ProviderCyzora).First(&p).Error != nil {
		return nil, "unknown charge"
	}
	if p.ProviderRef != nil && *p.ProviderRef != "" && chargeID != "" && *p.ProviderRef != chargeID {
		return nil, fmt.Sprintf("payment %d belongs to charge %s, not %s", p.ID, *p.ProviderRef, chargeID)
	}
	if (p.ProviderRef == nil || *p.ProviderRef == "") && chargeID != "" {
		config.DB.Model(&models.Payment{}).Where("id = ?", p.ID).Update("provider_ref", chargeID)
		p.ProviderRef = &chargeID
	}
	return &p, ""
}

// HandleCyzoraWebhook applies a signature-checked Cyzora event. Repeats are
// harmless: completion and failure only ever move a row once. note explains
// a no-op (logged; the webhook is still answered 200).
func (s *MpesaService) HandleCyzoraWebhook(evt CyzoraWebhookEvent) (note string, err error) {
	if !strings.HasPrefix(evt.Event, "charge.") {
		return "ignored event " + evt.Event, nil
	}
	status := strings.ToLower(strings.TrimSpace(evt.Status))
	if status == "" {
		status = strings.TrimPrefix(evt.Event, "charge.")
	}
	payID := evt.ZyraPaymentID
	if evt.Metadata != nil && evt.Metadata["zyra_payment_id"] != "" {
		payID = evt.Metadata["zyra_payment_id"]
	}
	p, why := FindCyzoraPayment(evt.ChargeID, payID)
	if p == nil {
		log.Printf("[cyzora] webhook %s for charge %s (ref %s): %s — ignored", evt.Event, evt.ChargeID, evt.Reference, why)
		return why, nil
	}
	_, err = s.ApplyCyzoraCharge(p, CyzoraOutcome{
		ChargeID: evt.ChargeID, Status: status, AmountCents: evt.Amount, Receipt: evt.MpesaReceipt,
		Error: evt.Error, ResultCode: evt.ResultCode, Livemode: evt.Livemode,
	})
	return "", err
}

// cyzoraFailureReason turns Cyzora's failure into the same customer wording
// the Daraja path uses.
func cyzoraFailureReason(o CyzoraOutcome) string {
	if rc, err := strconv.ParseFloat(strings.TrimSpace(o.ResultCode), 64); err == nil {
		return friendlySTKFailureReason(rc, o.Error)
	}
	switch o.Status {
	case "timeout", "expired":
		return STKNoResponseReason
	}
	e := strings.ToLower(o.Error)
	switch {
	case strings.Contains(e, "cancel"):
		return friendlySTKFailureReason(1032, "")
	case strings.Contains(e, "insufficient"):
		return friendlySTKFailureReason(1, "")
	case strings.Contains(e, "timeout"), strings.Contains(e, "no response"), strings.Contains(e, "did not respond"):
		return STKNoResponseReason
	}
	return friendlySTKFailureReason(-1, o.Error)
}

// ApplyCyzoraCharge completes or fails a Cyzora payment through the very same
// paths a Daraja callback uses (ProcessPaymentSuccess / ProcessUnderpayment /
// ProcessPaymentFailure), so credit, vouchers, session activation, router
// actions, SMS and the payout ledger behave identically. It returns the
// payment's status afterwards.
func (s *MpesaService) ApplyCyzoraCharge(p *models.Payment, o CyzoraOutcome) (string, error) {
	var fresh models.Payment
	if err := config.DB.First(&fresh, p.ID).Error; err != nil {
		return p.Status, err
	}
	*p = fresh
	status := strings.ToLower(strings.TrimSpace(o.Status))

	switch status {
	case "succeeded", "success", "completed", "paid":
		if o.Livemode != nil && !*o.Livemode && config.Config.AppEnv != "local" && config.Config.AppEnv != "test" {
			log.Printf("[cyzora] payment %d: charge %s is a TEST (simulated) charge — not granting WiFi on a deployed server", p.ID, o.ChargeID)
			return p.Status, nil
		}
		receipt := strings.TrimSpace(o.Receipt)
		if receipt == "" {
			receipt = "CYZ_" + o.ChargeID
		}
		// One M-Pesa receipt pays for one payment, never two.
		if !strings.HasPrefix(receipt, "CYZ_") {
			var other models.Payment
			if config.DB.Select("id").Where("mpesa_receipt_number = ? AND id <> ?", receipt, p.ID).First(&other).Error == nil {
				reason := fmt.Sprintf("Refused: M-Pesa receipt %s is already used by payment %d — check with Cyzora before crediting.", receipt, other.ID)
				res := config.DB.Model(&models.Payment{}).Where("id = ? AND status <> ?", p.ID, "completed").
					Updates(map[string]interface{}{"status": "failed", "status_reason": reason})
				if res.RowsAffected > 0 {
					log.Printf("[cyzora] DUPLICATE RECEIPT %s on payment %d (charge %s) — already on payment %d; refused", receipt, p.ID, o.ChargeID, other.ID)
					return "failed", nil
				}
				return p.Status, nil
			}
		}
		if o.AmountCents <= 0 {
			log.Printf("[cyzora] payment %d: success for charge %s carries no amount — left for the status check", p.ID, o.ChargeID)
			return p.Status, nil
		}
		paid := float64(o.AmountCents) / 100
		requested := math.Floor(p.Amount) // the prompt asked for whole shillings
		wasCompleted := p.Status == "completed"
		if paid+0.001 < requested {
			// Same handling as a Daraja underpayment: no package, the money is
			// kept as credit (or flagged for staff) and the reason says so.
			if wasCompleted {
				return "completed", nil
			}
			if err := s.ProcessUnderpayment(p, paid, receipt, p.Phone); err != nil {
				return "pending", err
			}
			return "completed", nil
		}
		if err := s.ProcessPaymentSuccess(p, receipt, p.Phone); err != nil {
			return "pending", err
		}
		if !wasCompleted && paid > requested+0.001 {
			note := fmt.Sprintf("Overpaid: KES %.2f received for KES %.2f — check with the customer.", paid, p.Amount)
			config.DB.Model(&models.Payment{}).Where("id = ? AND status_reason IS NULL", p.ID).Update("status_reason", note)
			log.Printf("[cyzora] payment %d: overpaid KES %.2f for KES %.2f", p.ID, paid, p.Amount)
		}
		return "completed", nil

	case "failed", "timeout", "expired", "cancelled", "canceled":
		if err := s.ProcessPaymentFailure(p, cyzoraFailureReason(CyzoraOutcome{
			Status: status, Error: o.Error, ResultCode: o.ResultCode,
		})); err != nil {
			return "pending", err
		}
		var after models.Payment
		config.DB.Select("status").First(&after, p.ID)
		return after.Status, nil
	}
	return p.Status, nil
}

// Polling windows for Cyzora payments still pending.
const (
	cyzoraPollAfter  = 20 * time.Second // give the webhook a chance first
	cyzoraPollEvery  = 10 * time.Second // at most one GET per payment per 10s
	cyzoraSweepAfter = time.Minute
	cyzoraSweepUntil = 60 * time.Minute
)

// QueryAndUpdateCyzoraStatus asks Cyzora about a still-pending Cyzora
// payment (GET /charges/{id}) and applies the answer. Throttled per payment.
func (s *MpesaService) QueryAndUpdateCyzoraStatus(p *models.Payment) (string, error) {
	if p.Status != "pending" {
		return p.Status, nil
	}
	if time.Since(p.CreatedAt) < cyzoraPollAfter {
		return "pending", nil
	}
	if p.ProviderRef == nil || *p.ProviderRef == "" {
		return "pending", nil // no charge id yet: the webhook will match it by metadata
	}
	now := time.Now()
	if v, ok := s.cyzoraPolls.Load(p.ID); ok {
		if last, ok := v.(time.Time); ok && now.Sub(last) < cyzoraPollEvery {
			return "pending", nil
		}
	}
	s.cyzoraPolls.Store(p.ID, now)

	ch, err := s.cyzoraClient().GetCharge(*p.ProviderRef)
	if err != nil {
		return "pending", fmt.Errorf("cyzora status check for payment %d: %w", p.ID, err)
	}
	if ch.ID != *p.ProviderRef {
		return "pending", fmt.Errorf("cyzora returned charge %s for %s", ch.ID, *p.ProviderRef)
	}
	st, err := s.ApplyCyzoraCharge(p, cyzoraOutcomeFromCharge(ch))
	if st == "completed" || st == "failed" {
		s.cyzoraPolls.Delete(p.ID)
	}
	return st, err
}

// ReconcilePendingCyzora checks Cyzora payments still pending 1–60 minutes
// after the prompt (the webhook may have been lost). After 60 minutes without
// a final answer they are failed with the usual reason — a late success
// webhook still completes them. Exported for tests.
func (j *MoneyJobs) ReconcilePendingCyzora() {
	now := time.Now()
	var pending []models.Payment
	if err := config.DB.
		Where("provider = ? AND status = ? AND created_at <= ? AND created_at >= ?",
			ProviderCyzora, "pending", now.Add(-cyzoraSweepAfter), now.Add(-cyzoraSweepUntil-5*time.Minute)).
		Order("id ASC").Limit(100).Find(&pending).Error; err != nil {
		log.Printf("[money-jobs] could not load pending Cyzora payments: %v", err)
		return
	}
	for i := range pending {
		p := &pending[i]
		status, err := j.mpesa.QueryAndUpdateCyzoraStatus(p)
		if err != nil {
			log.Printf("[money-jobs] payment %d: %v", p.ID, err)
		}
		if status == "pending" && now.Sub(p.CreatedAt) >= cyzoraSweepUntil {
			if err := j.mpesa.ProcessPaymentFailure(p, STKUnconfirmedReason); err == nil {
				log.Printf("[money-jobs] Cyzora payment %d: no final answer after 60 minutes — marked failed (a late success still completes it)", p.ID)
			}
		} else if status != "pending" {
			log.Printf("[money-jobs] Cyzora payment %d resolved by status check: %s", p.ID, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
