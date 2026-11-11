package services

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// Payment providers — who sends the STK prompt (models.Payment.Provider,
// models.Organization.PaymentProvider).
const (
	ProviderDaraja = "daraja"
	ProviderCyzora = "cyzora"
)

// STKStart is the outcome of starting a prompt through StartSTK.
type STKStart struct {
	Provider string // daraja | cyzora
	// Reference is what the payer polls /hotspot/status/:reference with and
	// what is stored in mpesa_transaction_id: Daraja's CheckoutRequestID, or
	// Cyzora's charge id (a random UUID — never guessable, which matters
	// because that endpoint signs the payer in).
	Reference string
	Message   string
	IsMock    bool // Daraja mock/sandbox: the caller simulates the callback
}

// stkNotSentError marks a Daraja failure where we know no prompt reached the
// phone (auth/config failure, Safaricom refused the request, the connection
// was never made). Only these may fall back to Cyzora: a timeout after the
// request went out may already have put a prompt on the phone, and a second
// prompt could charge the customer twice.
type stkNotSentError struct{ err error }

func (e *stkNotSentError) Error() string { return e.err.Error() }
func (e *stkNotSentError) Unwrap() error { return e.err }

func notSent(err error) error {
	if err == nil {
		return nil
	}
	return &stkNotSentError{err: err}
}

// IsSTKNotSent reports whether err says the Daraja prompt was never sent.
func IsSTKNotSent(err error) bool {
	var ns *stkNotSentError
	return errors.As(err, &ns)
}

// isDialError: the connection was never made (DNS, refused, unreachable), so
// the request cannot have reached the other side.
func isDialError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// PaymentRouting returns which provider sends prompts for a zone's ISP and
// whether a Daraja send failure may fall back to Cyzora.
func PaymentRouting(zoneID uint) (provider string, fallback bool) {
	var zone models.Zone
	if zoneID == 0 || config.DB.Select("organization_id").First(&zone, zoneID).Error != nil {
		return ProviderDaraja, false
	}
	var org models.Organization
	if config.DB.Select("id", "payment_provider", "cyzora_fallback").First(&org, zone.OrganizationID).Error != nil {
		return ProviderDaraja, false
	}
	if strings.EqualFold(strings.TrimSpace(org.PaymentProvider), ProviderCyzora) {
		return ProviderCyzora, false
	}
	return ProviderDaraja, org.CyzoraFallback
}

// StartSTK sends the prompt for a pending payment through the ISP's provider
// and records on the row which provider was used and its reference. Daraja
// behaves exactly as before (InitiateSTKPush); Cyzora goes through
// startCyzora. Every STK purchase path (portal plan, hotspot pay, credit
// top-up) calls this instead of InitiateSTKPush.
func (s *MpesaService) StartSTK(p *models.Payment, reference, description string) (*STKStart, error) {
	provider, fallback := PaymentRouting(p.ZoneID)
	if provider == ProviderCyzora {
		return s.startCyzora(p)
	}

	resp, err := s.InitiateSTKPush(p.ZoneID, p.Phone, p.Amount, reference, description)
	if err != nil {
		if fallback && IsSTKNotSent(err) && CyzoraConfigured() {
			log.Printf("[paygate] payment %d: Daraja prompt not sent (%v) — retrying once through Cyzora", p.ID, err)
			return s.startCyzora(p)
		}
		return nil, err
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("Failed to initiate M-Pesa STK Push payment.")
	}
	config.DB.Model(&models.Payment{}).Where("id = ?", p.ID).Updates(map[string]interface{}{
		"provider":             ProviderDaraja,
		"mpesa_transaction_id": resp.CheckoutRequestID,
	})
	p.Provider = ProviderDaraja
	ref := resp.CheckoutRequestID
	p.MpesaTransactionID = &ref
	return &STKStart{Provider: ProviderDaraja, Reference: ref, Message: resp.ResponseDescription, IsMock: resp.IsMock}, nil
}

func (s *MpesaService) cyzoraClient() *CyzoraService {
	if s.Cyzora == nil {
		s.Cyzora = NewCyzoraService()
	}
	return s.Cyzora
}

// cyzoraLiveAllowed: a Cyzora test key simulates payments, so it may only
// sell WiFi on a local/test server — on a deployed one it would give WiFi
// away with no money moving (same rule as Daraja mocks for ISP-own creds).
func cyzoraLiveAllowed() bool {
	if CyzoraKeyMode(CyzoraSecretKey()) != "test" {
		return true
	}
	return config.Config.AppEnv == "local" || config.Config.AppEnv == "test"
}

// startCyzora creates a Cyzora charge for the payment. The Cyzora charge id
// becomes the payment's reference (mpesa_transaction_id) and provider_ref.
func (s *MpesaService) startCyzora(p *models.Payment) (*STKStart, error) {
	if !CyzoraConfigured() {
		log.Printf("[cyzora] payment %d: Cyzora is selected but no secret key is saved", p.ID)
		return nil, errPaymentsUnavailable
	}
	if !cyzoraLiveAllowed() {
		log.Printf("[cyzora] payment %d: refusing to sell on a Cyzora TEST key on a deployed server", p.ID)
		return nil, errPaymentsUnavailable
	}
	phone := utils.FormatPhone(p.Phone)
	if len(phone) != 12 || (!strings.HasPrefix(phone, "2547") && !strings.HasPrefix(phone, "2541")) {
		return nil, fmt.Errorf("invalid phone number: must be 12 digits (e.g. 2547XXXXXXXX or 2541XXXXXXXX)")
	}
	// Whole shillings, like the Daraja prompt (int(amount)), so the customer
	// pays the same whichever provider sends it.
	kes := math.Floor(p.Amount)
	if kes < 1 {
		return nil, fmt.Errorf("amount must be at least 1 KES")
	}

	var orgID uint
	var zone models.Zone
	if config.DB.Select("organization_id").First(&zone, p.ZoneID).Error == nil {
		orgID = zone.OrganizationID
	}
	meta := map[string]string{
		"zyra_payment_id": strconv.FormatUint(uint64(p.ID), 10),
		"org_id":          strconv.FormatUint(uint64(orgID), 10),
		"zone_id":         strconv.FormatUint(uint64(p.ZoneID), 10),
	}
	// Mark the row as Cyzora's before the call, so that even if the answer is
	// lost, a later webhook can find it by metadata.zyra_payment_id.
	config.DB.Model(&models.Payment{}).Where("id = ?", p.ID).Update("provider", ProviderCyzora)
	p.Provider = ProviderCyzora
	idem := fmt.Sprintf("zyranet-payment-%d", p.ID)
	accountRef := sanitizeAccountReference(fmt.Sprintf("ZYR%d", p.ID))

	charge, accepted, err := s.cyzoraClient().CreateCharge(idem, int64(kes)*100, phone, accountRef, meta)
	if err != nil {
		log.Printf("[cyzora] payment %d: charge not started: %v", p.ID, err)
		var ce *CyzoraError
		if errors.As(err, &ce) && !ce.NotSent {
			// No clear answer: the prompt may be on the phone. If it is paid,
			// the webhook finds this payment by metadata and completes it
			// (a late success overrides a failure).
			return nil, fmt.Errorf("We couldn't confirm the M-Pesa prompt was sent. If one pops up, you can still pay it; otherwise please try again.")
		}
		return nil, errPaymentsUnavailable
	}

	updates := map[string]interface{}{"provider": ProviderCyzora}
	ref := ""
	if charge != nil {
		ref = charge.ID
		updates["provider_ref"] = charge.ID
		p.ProviderRef = &ref
	} else {
		// 202 without a charge: keep the payment pending under an unguessable
		// reference; the webhook matches it by metadata.zyra_payment_id.
		ref = "cyz_" + randomToken(16)
	}
	updates["mpesa_transaction_id"] = ref
	config.DB.Model(&models.Payment{}).Where("id = ?", p.ID).Updates(updates)
	p.Provider = ProviderCyzora
	p.MpesaTransactionID = &ref

	msg := "Check your phone and enter your M-Pesa PIN."
	if accepted {
		log.Printf("[cyzora] payment %d: charge %s accepted with outcome unknown (202) — kept pending", p.ID, ref)
	}
	// A charge that already settled (e.g. idempotent replay) completes now.
	if charge != nil && !accepted && charge.Status != "pending" && charge.Status != "awaiting_payment" {
		if _, err := s.ApplyCyzoraCharge(p, cyzoraOutcomeFromCharge(charge)); err != nil {
			log.Printf("[cyzora] payment %d: %v", p.ID, err)
		}
	}
	return &STKStart{Provider: ProviderCyzora, Reference: ref, Message: msg}, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
