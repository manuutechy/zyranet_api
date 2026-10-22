package services

import (
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// AdjustCredit changes a customer's credit balance in one atomic UPDATE, so
// two requests touching the same balance at once can't overwrite each other
// (the old read-add-write pattern lost money that way). A positive delta adds;
// a negative delta subtracts only if the balance covers it — ok is false when
// it doesn't (or the customer doesn't exist), and nothing changes. Pass a
// transaction (tx) to keep it together with the matching CreditLog/Payment.
func AdjustCredit(tx *gorm.DB, customerID uint, delta float64) (ok bool, err error) {
	q := tx.Model(&models.Customer{}).Where("id = ?", customerID)
	if delta >= 0 {
		res := q.Update("credit_balance", gorm.Expr("credit_balance + ?", delta))
		return res.RowsAffected == 1, res.Error
	}
	res := q.Where("credit_balance >= ?", -delta).Update("credit_balance", gorm.Expr("credit_balance - ?", -delta))
	return res.RowsAffected == 1, res.Error
}

// CreditBalanceOf reads a customer's current balance (after AdjustCredit, for
// receipts and SMS).
func CreditBalanceOf(tx *gorm.DB, customerID uint) float64 {
	var c models.Customer
	tx.Select("credit_balance").First(&c, customerID)
	return c.CreditBalance
}
