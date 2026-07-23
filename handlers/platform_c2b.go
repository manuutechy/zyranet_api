package handlers

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// PlatformC2BUnmatchedIndex lists C2B payments Safaricom confirmed on the
// shared paybill that couldn't be auto-matched to a customer (see
// MpesaC2BConfirmation), pending manual assignment to the right ISP.
func PlatformC2BUnmatchedIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	status := c.Query("status", "pending")

	var payments []models.UnmatchedC2BPayment
	var total int64
	query := config.DB.Model(&models.UnmatchedC2BPayment{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&payments)
	return utils.PaginatedResponse(c, payments, total, page, perPage)
}

// PlatformCustomerSearch lets SA look up a customer by phone/account/PPPoE
// username within a specific zone, when resolving an unmatched C2B payment
// to a specific customer rather than just a zone.
func PlatformCustomerSearch(c *fiber.Ctx) error {
	zoneID := c.Query("zone_id")
	q := c.Query("q")
	if zoneID == "" || q == "" {
		return utils.SuccessResponse(c, []models.Customer{}, "")
	}

	var customers []models.Customer
	config.DB.Where("zone_id = ? AND (phone LIKE ? OR account_number LIKE ? OR pppoe_username LIKE ? OR name LIKE ?)",
		zoneID, "%"+q+"%", "%"+q+"%", "%"+q+"%", "%"+q+"%").
		Limit(10).Find(&customers)
	return utils.SuccessResponse(c, customers, "")
}

// PlatformC2BUnmatchedResolve assigns an unmatched C2B payment to a zone
// (and optionally a specific customer, crediting their account exactly
// like the auto-matched path in MpesaC2BConfirmation does), converting it
// into a real Payment row.
func PlatformC2BUnmatchedResolve(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)

	var unmatched models.UnmatchedC2BPayment
	if err := config.DB.First(&unmatched, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Unmatched payment not found.", "", fiber.StatusNotFound)
	}
	if unmatched.Status != "pending" {
		return utils.ErrorResponse(c, "This payment has already been resolved.", "", fiber.StatusConflict)
	}

	var body struct {
		ZoneID     uint  `json:"zone_id"`
		CustomerID *uint `json:"customer_id"`
	}
	if err := c.BodyParser(&body); err != nil || body.ZoneID == 0 {
		return utils.ErrorResponse(c, "zone_id is required.", "", fiber.StatusUnprocessableEntity)
	}

	var zone models.Zone
	if err := config.DB.First(&zone, body.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusUnprocessableEntity)
	}

	var packageID *uint
	if body.CustomerID != nil {
		var customer models.Customer
		if err := config.DB.Where("zone_id = ?", zone.ID).First(&customer, *body.CustomerID).Error; err != nil {
			return utils.ErrorResponse(c, "Customer not found in that zone.", "", fiber.StatusUnprocessableEntity)
		}

		customer.CreditBalance += unmatched.Amount
		config.DB.Model(&customer).Update("credit_balance", customer.CreditBalance)

		// AddedBy is left nil rather than set to claims.PlatformUserID: it's a
		// foreign key into `users` (ISP staff), and platform staff have no
		// row there — the platform user ID is recorded in the note instead.
		noteStr := fmt.Sprintf("M-Pesa C2B Paybill %s (manually reconciled by platform staff #%d)", unmatched.TransID, claims.PlatformUserID)
		config.DB.Create(&models.CreditLog{
			CustomerID: customer.ID,
			Amount:     unmatched.Amount,
			Type:       "credit",
			Note:       &noteStr,
		})

		if customer.PackageID > 0 {
			packageID = &customer.PackageID
		}

		if smsSvcGlobal != nil && customer.Phone != "" {
			msg := fmt.Sprintf("Payment Received: KES %.2f via M-Pesa (%s). Account balance: KES %.2f.", unmatched.Amount, unmatched.TransID, customer.CreditBalance)
			go smsSvcGlobal.Send(customer.Phone, msg)
		}
	}

	transIDStr := unmatched.TransID
	payment := models.Payment{
		CustomerID:         body.CustomerID,
		ZoneID:             zone.ID,
		PackageID:          packageID,
		Phone:              unmatched.Phone,
		Amount:             unmatched.Amount,
		Currency:           "KES",
		Method:             "mpesa_c2b",
		Status:             "completed",
		MpesaReceiptNumber: &transIDStr,
		MpesaTransactionID: &transIDStr,
	}
	if err := config.DB.Create(&payment).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to record payment.", fiber.StatusInternalServerError)
	}

	now := time.Now()
	config.DB.Model(&unmatched).Updates(map[string]interface{}{
		"status":                       "resolved",
		"resolved_zone_id":             zone.ID,
		"resolved_customer_id":         body.CustomerID,
		"resolved_payment_id":          payment.ID,
		"resolved_by_platform_user_id": claims.PlatformUserID,
		"resolved_at":                  now,
	})

	return utils.SuccessResponse(c, payment, "Payment reconciled successfully.")
}
