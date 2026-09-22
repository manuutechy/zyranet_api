package handlers

import (
	"strings"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// phoneSuffix9 returns the last 9 digits of s if s looks like a phone number
// (all digits after stripping "+", spaces and dashes; 9-13 long), else "".
// Daraja's C2B v2 hashes the MSISDN into a hex string, and a paybill account
// reference can be anything a customer types — neither should ever be used
// as a phone lookup key, or a short/garbled value would match arbitrary
// customers.
func phoneSuffix9(s string) string {
	s = strings.NewReplacer("+", "", " ", "", "-", "").Replace(strings.TrimSpace(s))
	if len(s) < 9 || len(s) > 13 {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s[len(s)-9:]
}

// c2bCustomerScope restricts a customer query to one ISP when the payment
// was made to that ISP's own shortcode. On the shared platform paybill
// (scoped == false) every ISP's customers are candidates, so matching there
// must be unambiguous instead.
func c2bCustomerScope(orgID uint, scoped bool) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if !scoped {
			return db
		}
		return db.Where("zone_id IN (?)",
			config.DB.Model(&models.Zone{}).Select("id").Where("organization_id = ?", orgID))
	}
}

// matchC2BCustomer finds the customer a C2B paybill payment belongs to.
//
// It tries, in order: the account number (unique), the PPPoE username, then
// the phone number from the account reference, then from the payer's MSISDN.
// A step that finds exactly one customer wins; a step that finds several
// (e.g. the same phone registered with two ISPs) means the payment can't be
// attributed safely, so it returns ambiguous=true and the caller must queue
// it for manual reconciliation rather than guess. No match at all returns
// (nil, false).
func matchC2BCustomer(orgID uint, scoped bool, billRef, msisdn string) (customer *models.Customer, ambiguous bool) {
	scope := c2bCustomerScope(orgID, scoped)
	billRef = strings.TrimSpace(billRef)

	lookups := make([]func(*gorm.DB) *gorm.DB, 0, 4)
	if billRef != "" {
		lookups = append(lookups,
			func(db *gorm.DB) *gorm.DB { return db.Where("account_number = ?", billRef) },
			func(db *gorm.DB) *gorm.DB { return db.Where("pppoe_username = ?", billRef) },
		)
	}
	for _, raw := range []string{billRef, msisdn} {
		if suffix := phoneSuffix9(raw); suffix != "" {
			suffix := suffix
			lookups = append(lookups, func(db *gorm.DB) *gorm.DB {
				return db.Where("phone LIKE ?", "%"+suffix)
			})
		}
	}

	for _, where := range lookups {
		var found []models.Customer
		if err := where(scope(config.DB.Model(&models.Customer{}))).Limit(2).Find(&found).Error; err != nil {
			continue
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return &found[0], false
		default:
			return nil, true
		}
	}
	return nil, false
}

// autoCreateC2BCustomer registers a first-time payer as a new hotspot
// customer, but only when the payment went to an ISP's own shortcode and
// that ISP has exactly one zone. Anywhere else (the shared paybill, or an ISP
// with several zones) which zone the customer belongs to can't be inferred,
// and guessing puts them — and the revenue — under the wrong ISP; those
// payments go to the unmatched queue instead.
func autoCreateC2BCustomer(orgID uint, cleanPhone, payerName string, amount float64) (*models.Customer, bool) {
	if phoneSuffix9(cleanPhone) == "" {
		return nil, false
	}

	var zones []models.Zone
	if err := config.DB.Where("organization_id = ?", orgID).Limit(2).Find(&zones).Error; err != nil || len(zones) != 1 {
		return nil, false
	}
	zone := zones[0]

	var pkg models.Package
	if err := config.DB.Where("zone_id = ? AND price = ? AND status = ?", zone.ID, amount, "active").First(&pkg).Error; err != nil {
		if err := config.DB.Where("zone_id = ? AND status = ?", zone.ID, "active").Order("price ASC").First(&pkg).Error; err != nil {
			return nil, false
		}
	}

	c := models.Customer{
		Name:      payerName,
		Phone:     cleanPhone,
		ZoneID:    zone.ID,
		PackageID: pkg.ID,
		Type:      "hotspot",
		Status:    "active",
	}
	if err := config.DB.Create(&c).Error; err != nil {
		return nil, false
	}
	return &c, true
}

// c2bAlreadyRecorded reports whether a C2B transaction was already handled —
// as a completed payment (matched, or an STK push whose receipt is the same
// TransID) or as a queued unmatched one. Safaricom re-delivers confirmations
// it thinks weren't acknowledged, and without this each retry would credit
// the customer again.
func c2bAlreadyRecorded(transID string) bool {
	var n int64
	config.DB.Model(&models.Payment{}).
		Where("mpesa_receipt_number = ? OR mpesa_transaction_id = ?", transID, transID).
		Count(&n)
	if n > 0 {
		return true
	}
	config.DB.Model(&models.UnmatchedC2BPayment{}).Where("trans_id = ?", transID).Count(&n)
	return n > 0
}

// c2bCollectionChannel is where a paybill payment's money landed: an ISP's own
// shortcode ("own"), or Zyra Net's shared paybill ("platform"), the latter only
// when the shared Daraja app is live (a sandbox payment isn't real money).
func c2bCollectionChannel(scopedToOwnShortcode bool) string {
	if scopedToOwnShortcode {
		return "own"
	}
	if mpesaSvcGlobal != nil && !mpesaSvcGlobal.PlatformIsProduction() {
		return ""
	}
	return "platform"
}
