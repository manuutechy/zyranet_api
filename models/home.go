package models

import (
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// HomeRenewal marks an STK payment made from the public renew page for a
// home (fibre / PPPoE) account. When the payment succeeds the money goes on
// the customer's credit and as many whole periods of their plan as the credit
// covers are bought from it (at least Periods, since Amount = price × Periods).
type HomeRenewal struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	PaymentID  uint      `gorm:"not null;uniqueIndex" json:"payment_id"`
	CustomerID uint      `gorm:"not null;index" json:"customer_id"`
	Periods    int       `gorm:"not null" json:"periods"`
	CreatedAt  time.Time `json:"created_at"`
}

func (HomeRenewal) TableName() string { return "home_renewals" }

// customerNoBase: customer numbers are (customerNoBase + id) followed by a
// Luhn check digit, so customer 1 is 10017 and numbers stay short.
const customerNoBase = 1000

// luhnDigit is the Luhn check digit for a string of digits.
func luhnDigit(digits string) uint {
	sum := 0
	double := true // the digit next to the check digit is doubled
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return uint((10 - sum%10) % 10)
}

// CustomerNoFor is the customer number of a customer id.
func CustomerNoFor(id uint) uint {
	base := uint(customerNoBase) + id
	return base*10 + luhnDigit(strconv.FormatUint(uint64(base), 10))
}

// ParseCustomerNo reads a customer number the way people type it on the
// M-Pesa menu or a web form: spaces, lower case, a leading "#", or a "ZYR" /
// "Z" prefix (ZYR#10017, z 10017, #10017) are all fine. ok is false when it
// isn't a valid number (including a wrong check digit).
func ParseCustomerNo(raw string) (uint, bool) {
	s := strings.ToUpper(strings.Join(strings.Fields(raw), ""))
	s = strings.TrimPrefix(s, "#")
	if strings.HasPrefix(s, "ZYR") {
		s = s[3:]
	} else if strings.HasPrefix(s, "Z") {
		s = s[1:]
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "#"), "-")
	if len(s) < 5 || len(s) > 8 { // phone numbers (9+ digits) are never customer numbers
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	if luhnDigit(s[:len(s)-1]) != uint(s[len(s)-1]-'0') {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint(n), true
}

// AfterCreate gives every new customer its customer number. It is derived
// from the id, so two customers created at once can never get the same one.
func (c *Customer) AfterCreate(tx *gorm.DB) error {
	if c.CustomerNo != nil || c.ID == 0 {
		return nil
	}
	no := CustomerNoFor(c.ID)
	if err := tx.Session(&gorm.Session{NewDB: true}).Model(&Customer{}).Where("id = ?", c.ID).
		UpdateColumn("customer_no", no).Error; err != nil {
		return err
	}
	c.CustomerNo = &no
	return nil
}

// BackfillCustomerNumbers gives customers made before customer numbers
// existed their number. Idempotent: only rows still without one are touched.
func BackfillCustomerNumbers(db *gorm.DB) (int, error) {
	done := 0
	for {
		var ids []uint
		if err := db.Unscoped().Model(&Customer{}).Where("customer_no IS NULL").
			Order("id ASC").Limit(500).Pluck("id", &ids).Error; err != nil {
			return done, err
		}
		if len(ids) == 0 {
			return done, nil
		}
		for _, id := range ids {
			if err := db.Unscoped().Model(&Customer{}).Where("id = ? AND customer_no IS NULL", id).
				UpdateColumn("customer_no", CustomerNoFor(id)).Error; err != nil {
				return done, err
			}
			done++
		}
	}
}
