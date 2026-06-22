package handlers

import (
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)


func PaymentIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var payments []models.Payment
	var total int64

	query := config.DB.Model(&models.Payment{}).Preload("Customer").Preload("Zone").Preload("Package")
	if z := c.Query("zone_id"); z != "" {
		query = query.Where("zone_id = ?", z)
	}
	if m := c.Query("method"); m != "" {
		query = query.Where("method = ?", m)
	}
	if s := c.Query("status"); s != "" {
		query = query.Where("status = ?", s)
	}
	if cid := c.Query("customer_id"); cid != "" {
		query = query.Where("customer_id = ?", cid)
	}
	if df := c.Query("date_from"); df != "" {
		query = query.Where("DATE(created_at) >= ?", df)
	}
	if dt := c.Query("date_to"); dt != "" {
		query = query.Where("DATE(created_at) <= ?", dt)
	}

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&payments)
	return utils.PaginatedResponse(c, payments, total, page, perPage)
}

// PaymentShow returns a single payment (public for polling status).
func PaymentShow(c *fiber.Ctx) error {
	var payment models.Payment
	if err := config.DB.Preload("Package").Preload("Zone").First(&payment, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Payment record not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, payment, "")
}

// PaymentRecordManual records a cash/manual payment.
func PaymentRecordManual(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	allowed := []string{"super_admin", "zone_manager", "finance"}
	isAllowed := false
	for _, r := range allowed {
		if r == claims.Role {
			isAllowed = true
			break
		}
	}
	if !isAllowed {
		return utils.ErrorResponse(c, "Unauthorized to record manual payments.", "", fiber.StatusForbidden)
	}

	var body struct {
		CustomerID *uint   `json:"customer_id"`
		ZoneID     uint    `json:"zone_id"`
		PackageID  uint    `json:"package_id"`
		Phone      string  `json:"phone"`
		Amount     float64 `json:"amount"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	var pkg models.Package
	if err := config.DB.First(&pkg, body.PackageID).Error; err != nil {
		return utils.ErrorResponse(c, "Package not found.", "", fiber.StatusNotFound)
	}

	var voucherID *uint

	if body.CustomerID == nil {
		// Hotspot voucher cash purchase
		voucher, err := voucherSvcGlobal.Generate(body.ZoneID, body.PackageID, "single_use", 1)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Failed to generate voucher.", fiber.StatusInternalServerError)
		}
		voucherID = &voucher.ID
		msg := "Hi, cash payment of KES " + formatAmount(body.Amount) + " received. Your voucher code is " + voucher.Code + ". Enjoy browsing!"
		go smsSvcGlobal.Send(body.Phone, msg) //nolint:errcheck
	} else {
		// Renewing an existing customer
		var customer models.Customer
		if err := config.DB.First(&customer, *body.CustomerID).Error; err != nil {
			return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
		}
		expiresAt := utils.CalculateExpiry(pkg.BillingCycle, nil)
		config.DB.Model(&customer).Updates(map[string]interface{}{
			"status":     "active",
			"package_id": pkg.ID,
			"expires_at": expiresAt,
		})
		msg := "Hi " + customer.Name + ", payment of KES " + formatAmount(body.Amount) + " received. Your account is active. Expires: " + expiresAt.Format("2006-01-02 15:04") + "."
		go smsSvcGlobal.Send(body.Phone, msg) //nolint:errcheck
	}

	payment := models.Payment{
		CustomerID: body.CustomerID,
		VoucherID:  voucherID,
		ZoneID:     body.ZoneID,
		PackageID:  &body.PackageID,
		Phone:      body.Phone,
		Amount:     body.Amount,
		Currency:   "KES",
		Method:     "manual",
		Status:     "completed",
	}
	config.DB.Create(&payment)
	return utils.SuccessResponse(c, payment, "Manual payment recorded successfully.", fiber.StatusCreated)
}

func formatAmount(amount float64) string {
	return fmt.Sprintf("%.0f", amount)
}
