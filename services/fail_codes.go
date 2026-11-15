package services

import (
	"strings"

	"github.com/zyranet/zyranet-api/config"
)

// Machine codes the captive portal reads next to the English message (which
// stays, so older portals keep working).
//
// Payment failures (field "code" on /hotspot/status and the home pay status):
// cancelled | pin | funds | no_response | unconfirmed | other.
const (
	PayFailCancelled   = "cancelled"
	PayFailPIN         = "pin"
	PayFailFunds       = "funds"
	PayFailNoResponse  = "no_response"
	PayFailUnconfirmed = "unconfirmed"
	PayFailOther       = "other"
)

// PaymentFailureCode turns a failed payment's stored reason into its code.
func PaymentFailureCode(reason string) string {
	r := strings.ToLower(reason)
	if strings.Contains(r, "didn't confirm") || strings.Contains(r, "couldn't confirm") ||
		strings.Contains(r, "could not confirm") || strings.Contains(r, "under processing") {
		return PayFailUnconfirmed
	}
	switch ClassifyFailureReason(reason) {
	case FailCancelled:
		return PayFailCancelled
	case FailWrongPIN:
		return PayFailPIN
	case FailLowBalance:
		return PayFailFunds
	case FailNoResponse:
		return PayFailNoResponse
	}
	return PayFailOther
}

// Voucher refusals (field "code" on /vouchers/redeem):
// invalid | used | expired | other.
const (
	VoucherFailInvalid = "invalid"
	VoucherFailUsed    = "used"
	VoucherFailExpired = "expired"
	VoucherFailOther   = "other"
)

// VoucherFailureCode maps a voucher service error to its code.
func VoucherFailureCode(err error) string {
	if err == nil {
		return ""
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "not found"):
		return VoucherFailInvalid
	case strings.Contains(m, "depleted"), strings.Contains(m, "already redeemed"):
		return VoucherFailUsed
	case strings.Contains(m, "expired"):
		return VoucherFailExpired
	}
	return VoucherFailOther
}

// mockSTKOutcomes: in MPESA_ENV=mock ONLY, these test phones make the
// simulated prompt fail in a chosen way, so the portal's failure screens can
// be tested end to end. Real Daraja/Cyzora never see them.
var mockSTKOutcomes = map[string]struct {
	Code float64
	Desc string
}{
	"254700000001": {1032, "Request cancelled by user"},
	"254700000002": {2001, "The initiator information is invalid."},
	"254700000003": {1, "The balance is insufficient for the transaction."},
	"254700000004": {1037, "DS timeout user cannot be reached"},
	"254700000005": {4999, "The transaction is still under processing"},
}

// MockSTKFailure returns the simulated Daraja result for a magic test phone.
// It is false unless the server's own MPESA_ENV is "mock" and it is not a
// production server — impossible to trigger anywhere else.
func MockSTKFailure(phone string) (code float64, desc string, ok bool) {
	if !strings.EqualFold(strings.TrimSpace(config.Config.MpesaEnv), "mock") || config.Config.AppEnv == "production" {
		return 0, "", false
	}
	p := strings.TrimSpace(phone)
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, p)
	if strings.HasPrefix(digits, "0") && len(digits) == 10 {
		digits = "254" + digits[1:]
	}
	o, found := mockSTKOutcomes[digits]
	return o.Code, o.Desc, found
}
