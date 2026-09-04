package handlers

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)


func PaymentIndex(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}

	page, perPage := utils.ParsePage(c)
	var payments []models.Payment
	var total int64

	query := config.DB.Model(&models.Payment{}).Preload("Customer").Preload("Zone").Preload("Package").Where("zone_id IN (?)", orgZoneIDs)
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

// PaymentShow returns a single payment, scoped to the caller's organization
// (admin-authenticated). Previously this had no auth and no org/zone
// filter at all, which let anyone enumerate payment IDs and read another
// tenant's customer PII (phone, receipt numbers, amounts). Status polling
// for the payer's own in-flight STK push is served by the customer JWT
// route (CustomerAuthPayments) / hotspot status endpoints instead.
func PaymentShow(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var payment models.Payment
	if err := config.DB.Preload("Package").Preload("Zone").Where("zone_id IN (?)", orgZoneIDs).First(&payment, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Payment record not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, payment, "")
}

// PaymentRecordManual records a cash, C2B, or offline manual payment.
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
		return utils.ErrorResponse(c, "Unauthorized to record manual/C2B payments.", "", fiber.StatusForbidden)
	}

	var body struct {
		CustomerID         *uint   `json:"customer_id"`
		ZoneID             uint    `json:"zone_id"`
		PackageID          *uint   `json:"package_id"`
		Phone              string  `json:"phone"`
		Amount             float64 `json:"amount"`
		Method             string  `json:"method"` // manual | mpesa_c2b | bank
		MpesaReceiptNumber string  `json:"mpesa_receipt_number"`
		Action             string  `json:"action"` // renew | credit | credit_and_renew
	}
	if err := c.BodyParser(&body); err != nil || body.Amount <= 0 {
		return utils.ErrorResponse(c, "Invalid request body or amount.", "", fiber.StatusBadRequest)
	}

	if body.Method == "" {
		body.Method = "manual"
	}
	if body.Action == "" {
		body.Action = "renew"
	}

	var targetZone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&targetZone, body.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid zone for this organization.", "", fiber.StatusUnprocessableEntity)
	}

	var pkg models.Package
	var packageIDPtr *uint
	if body.PackageID != nil && *body.PackageID > 0 {
		if err := config.DB.First(&pkg, *body.PackageID).Error; err == nil {
			packageIDPtr = &pkg.ID
		}
	}

	var voucherID *uint
	var receiptPtr *string
	if strings.TrimSpace(body.MpesaReceiptNumber) != "" {
		ref := strings.TrimSpace(body.MpesaReceiptNumber)
		receiptPtr = &ref
	}

	if body.CustomerID == nil || *body.CustomerID == 0 {
		// Hotspot voucher cash/C2B purchase
		if packageIDPtr == nil {
			return utils.ErrorResponse(c, "Package required for voucher generation.", "", fiber.StatusBadRequest)
		}
		voucher, err := voucherSvcGlobal.Generate(body.ZoneID, *packageIDPtr, "single_use", 1)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Failed to generate voucher.", fiber.StatusInternalServerError)
		}
		voucherID = &voucher.ID
		template := GetSetting("sms_template_voucher")
		msg := utils.RenderTemplate(template, map[string]string{
			"name":  "Guest",
			"price": formatAmount(body.Amount),
			"code":  voucher.Code,
		})
		if GetSetting("sms_enable_voucher") != "no" && body.Phone != "" {
			go smsSvcGlobal.Send(claims.OrganizationID, body.Phone, msg) //nolint:errcheck
		}
	} else {
		// Existing subscriber payment
		orgZoneIDs, err := middleware.OrgZoneIDs(c)
		if err != nil {
			return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
		}
		var customer models.Customer
		if err := config.DB.Where("zone_id IN (?)", orgZoneIDs).First(&customer, *body.CustomerID).Error; err != nil {
			return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
		}

		if body.Action == "credit" || body.Action == "credit_and_renew" {
			// Add amount as credit balance
			customer.CreditBalance += body.Amount
			config.DB.Model(&customer).Update("credit_balance", customer.CreditBalance)

			note := fmt.Sprintf("Offline/C2B payment added as credit (%s)", body.Method)
			if receiptPtr != nil {
				note = fmt.Sprintf("C2B/Offline Payment Ref: %s (%s)", *receiptPtr, body.Method)
			}
			config.DB.Create(&models.CreditLog{
				CustomerID: customer.ID,
				Amount:     body.Amount,
				Type:       "credit",
				Note:       &note,
				AddedBy:    &claims.UserID,
			})
		}

		if (body.Action == "renew" || body.Action == "credit_and_renew") && packageIDPtr != nil {
			// Check if auto-renewing from credit balance
			if body.Action == "credit_and_renew" && customer.CreditBalance >= pkg.Price {
				customer.CreditBalance -= pkg.Price
				config.DB.Model(&customer).Update("credit_balance", customer.CreditBalance)

				debitNote := fmt.Sprintf("Package %s auto-renewed from credit", pkg.Name)
				config.DB.Create(&models.CreditLog{
					CustomerID: customer.ID,
					Amount:     pkg.Price,
					Type:       "debit",
					Note:       &debitNote,
					AddedBy:    &claims.UserID,
				})
			}

			expiresAt := utils.CalculateExpiry(pkg.BillingCycle, nil)
			config.DB.Model(&customer).Updates(map[string]interface{}{
				"status":     "active",
				"package_id": pkg.ID,
				"expires_at": expiresAt,
			})

			templateActive := GetSetting("sms_template_active")
			msg := utils.RenderTemplate(templateActive, map[string]string{
				"name":    customer.Name,
				"package": pkg.Name,
				"expiry":  expiresAt.Format("2006-01-02 15:04"),
			})
			if GetSetting("sms_enable_active") != "no" && body.Phone != "" {
				go smsSvcGlobal.Send(claims.OrganizationID, body.Phone, msg) //nolint:errcheck
			}
		} else if body.Action == "credit" {
			// Send credit notification SMS
			msg := fmt.Sprintf("Hi %s, KES %.2f has been credited to your Zyra Net account. New Balance: KES %.2f.",
				customer.Name, body.Amount, customer.CreditBalance)
			if GetSetting("sms_enable_active") != "no" && body.Phone != "" {
				go smsSvcGlobal.Send(claims.OrganizationID, body.Phone, msg) //nolint:errcheck
			}
		}
	}

	payment := models.Payment{
		CustomerID:         body.CustomerID,
		VoucherID:          voucherID,
		ZoneID:             body.ZoneID,
		PackageID:          packageIDPtr,
		Phone:              body.Phone,
		Amount:             body.Amount,
		Currency:           "KES",
		Method:             body.Method,
		MpesaReceiptNumber: receiptPtr,
		Status:             "completed",
	}
	config.DB.Create(&payment)
	return utils.SuccessResponse(c, payment, "Payment and credit recorded successfully.", fiber.StatusCreated)
}

func formatAmount(amount float64) string {
	return fmt.Sprintf("%.0f", amount)
}

var emailSvcGlobal = services.NewEmailService()

// PaymentInvoice generates an HTML view of the invoice, scoped to the
// caller's organization (admin-authenticated). Previously this had no
// auth and no org/zone filter, leaking full customer PII (name, phone,
// account number, package/zone) across tenants to anyone who could guess
// a payment ID. Emailing/SMS-ing the invoice to the actual customer is
// still handled by PaymentInvoiceEmail/PaymentInvoiceSMS.
func PaymentInvoice(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var payment models.Payment
	if err := config.DB.Preload("Customer").Preload("Zone").Preload("Package").Where("zone_id IN (?)", orgZoneIDs).First(&payment, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Payment not found.", "", fiber.StatusNotFound)
	}

	html := renderInvoiceHTML(c, &payment)
	c.Set("Content-Type", "text/html")
	return c.SendString(html)
}

// PaymentInvoiceEmail sends the invoice to the customer via email.
func PaymentInvoiceEmail(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var payment models.Payment
	if err := config.DB.Preload("Customer").Preload("Zone").Preload("Package").Where("zone_id IN (?)", orgZoneIDs).First(&payment, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Payment not found.", "", fiber.StatusNotFound)
	}

	var body struct {
		Email string `json:"email"`
	}
	if err := c.BodyParser(&body); err != nil || body.Email == "" {
		return utils.ErrorResponse(c, "Invalid email address.", "", fiber.StatusBadRequest)
	}

	html := renderInvoiceHTML(c, &payment)
	subject := fmt.Sprintf("Invoice Paid - INV-%d", payment.ID)

	go func() {
		_ = emailSvcGlobal.Send(body.Email, subject, html)
	}()

	return utils.SuccessResponse(c, nil, "Invoice email sent successfully.")
}

// PaymentInvoiceSMS sends the invoice summary and link via SMS.
func PaymentInvoiceSMS(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var payment models.Payment
	if err := config.DB.Preload("Customer").Preload("Zone").Preload("Package").Where("zone_id IN (?)", orgZoneIDs).First(&payment, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Payment not found.", "", fiber.StatusNotFound)
	}

	var body struct {
		Phone string `json:"phone"`
	}
	if err := c.BodyParser(&body); err != nil || body.Phone == "" {
		return utils.ErrorResponse(c, "Invalid phone number.", "", fiber.StatusBadRequest)
	}

	settings := loadAllSettings()
	companyName := settings["company_name"]
	if companyName == "" {
		companyName = "Zyra Net"
	}

	invoiceURL := fmt.Sprintf("%s/api/v1/payments/%d/invoice", c.BaseURL(), payment.ID)
	msg := fmt.Sprintf("Hi, your %s invoice INV-%d of KES %.0f is ready. View/Print it here: %s",
		companyName, payment.ID, payment.Amount, invoiceURL)

	go func() {
		_, _ = smsSvcGlobal.SendForZone(payment.ZoneID, body.Phone, msg)
	}()

	return utils.SuccessResponse(c, nil, "Invoice SMS sent successfully.")
}

func renderInvoiceHTML(c *fiber.Ctx, payment *models.Payment) string {
	settings := loadAllSettings()
	companyName := settings["company_name"]
	if companyName == "" {
		companyName = "Zyra Net ISP"
	}
	logoURL := settings["logo"]
	if logoURL != "" && !strings.HasPrefix(logoURL, "http") {
		logoURL = c.BaseURL() + "/" + strings.TrimLeft(logoURL, "/")
	}

	primaryColor := settings["primary_color"]
	if primaryColor == "" {
		primaryColor = "#f97316"
	}

	logoImgTag := ""
	if logoURL != "" {
		logoImgTag = fmt.Sprintf(`<img src="%s" class="logo" alt="Logo"><br>`, logoURL)
	}

	customerName := "Voucher Ticket Purchase"
	customerPhone := payment.Phone
	accountNumber := "N/A"
	if payment.Customer != nil {
		customerName = payment.Customer.Name
		customerPhone = payment.Customer.Phone
		accountNumber = payment.Customer.AccountNumber
	}

	receiptNumber := "N/A"
	if payment.MpesaReceiptNumber != nil && *payment.MpesaReceiptNumber != "" {
		receiptNumber = *payment.MpesaReceiptNumber
	} else if payment.MpesaTransactionID != nil && *payment.MpesaTransactionID != "" {
		receiptNumber = *payment.MpesaTransactionID
	}

	packageName := "Hotspot Voucher Ticket"
	speedUpload := "N/A"
	speedDownload := "N/A"
	billingCycle := "One-Time"
	if payment.Package != nil {
		packageName = payment.Package.Name
		billingCycle = payment.Package.BillingCycle
		if payment.Package.SpeedUploadKbps >= 1024 {
			speedUpload = fmt.Sprintf("%.1f Mbps", float64(payment.Package.SpeedUploadKbps)/1024)
		} else {
			speedUpload = fmt.Sprintf("%d Kbps", payment.Package.SpeedUploadKbps)
		}
		if payment.Package.SpeedDownloadKbps >= 1024 {
			speedDownload = fmt.Sprintf("%.1f Mbps", float64(payment.Package.SpeedDownloadKbps)/1024)
		} else {
			speedDownload = fmt.Sprintf("%d Kbps", payment.Package.SpeedDownloadKbps)
		}
	}

	zoneName := "All Zones"
	if payment.Zone != nil {
		zoneName = payment.Zone.Name
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Invoice #INV-%d</title>
    <style>
        body { font-family: 'Helvetica Neue', Helvetica, Arial, sans-serif; color: #333; margin: 0; padding: 20px; background-color: #f8fafc; }
        .invoice-box { max-width: 800px; margin: auto; padding: 40px; border: 1px solid #e2e8f0; font-size: 14px; line-height: 24px; border-radius: 12px; background: #fff; box-shadow: 0 4px 6px -1px rgb(0 0 0 / 0.05); }
        .invoice-box table { width: 100%%; line-height: inherit; text-align: left; border-collapse: collapse; }
        .invoice-box table td { padding: 8px; vertical-align: top; }
        .invoice-box table tr td:nth-child(2) { text-align: right; }
        .invoice-header { border-bottom: 2px solid %s; padding-bottom: 25px; margin-bottom: 25px; }
        .logo { max-height: 60px; margin-bottom: 10px; }
        .company-name { font-size: 24px; font-weight: bold; color: %s; margin: 0; }
        .title { font-size: 32px; font-weight: 800; color: #0f172a; margin: 0; tracking-wide: true; }
        .details-table { margin-bottom: 30px; }
        .details-table td { padding: 12px 8px; }
        .items-table th { background: #f8fafc; border-bottom: 2px solid #e2e8f0; font-weight: bold; padding: 12px 10px; text-align: left; color: #475569; }
        .items-table th:nth-child(2), .items-table td:nth-child(2) { text-align: right; }
        .items-table td { border-bottom: 1px solid #f1f5f9; padding: 16px 10px; }
        .total-section { margin-top: 30px; padding-top: 20px; }
        .total-amount { font-size: 20px; font-weight: bold; color: %s; }
        .footer { text-align: center; font-size: 12px; color: #94a3b8; margin-top: 50px; border-top: 1px solid #f1f5f9; padding-top: 20px; }
        .print-btn { display: inline-block; background: %s; color: #fff; padding: 10px 20px; border-radius: 8px; text-decoration: none; font-weight: 600; margin-bottom: 20px; border: none; cursor: pointer; font-size: 13px; transition: opacity 0.2s; }
        .print-btn:hover { opacity: 0.9; }
        @media print { .print-btn { display: none; } body { padding: 0; background-color: #fff; } .invoice-box { border: none; box-shadow: none; padding: 0; } }
    </style>
</head>
<body>
    <div style="max-width: 800px; margin: auto; text-align: right;">
        <button onclick="window.print()" class="print-btn">Print Invoice</button>
    </div>
    <div class="invoice-box">
        <table class="invoice-header">
            <tr>
                <td>
                    %s
                    <p class="company-name">%s</p>
                </td>
                <td>
                    <p class="title">INVOICE</p>
                    <strong>Invoice #:</strong> INV-%d<br>
                    <strong>Date:</strong> %s<br>
                    <strong>Status:</strong> <span style="color: #10b981; font-weight: bold;">PAID</span>
                </td>
            </tr>
        </table>

        <table class="details-table">
            <tr>
                <td>
                    <strong style="color: #475569;">BILL TO</strong><br>
                    <span style="font-size: 15px; font-weight: 600; color: #0f172a;">%s</span><br>
                    Phone: %s<br>
                    Account: %s
                </td>
                <td>
                    <strong style="color: #475569;">PAYMENT DETAILS</strong><br>
                    Method: %s<br>
                    Receipt: %s
                </td>
            </tr>
        </table>

        <table class="items-table">
            <thead>
                <tr>
                    <th>Item Description</th>
                    <th>Amount</th>
                </tr>
            </thead>
            <tbody>
                <tr>
                    <td>
                        <strong style="font-size: 15px; color: #0f172a;">%s</strong> (Speed: Up %s / Down %s)<br>
                        <span style="color: #64748b; font-size: 12px;">Billing Cycle: %s • Router Zone: %s</span>
                    </td>
                    <td style="font-size: 15px; font-weight: 600; color: #0f172a;">KES %.2f</td>
                </tr>
            </tbody>
        </table>

        <table class="total-section">
            <tr>
                <td style="width: 60%%;"></td>
                <td>
                    <table style="width: 100%%;">
                        <tr>
                            <td style="color: #475569; padding: 6px 0;">Subtotal:</td>
                            <td style="font-weight: 600; color: #0f172a; padding: 6px 0;">KES %.2f</td>
                        </tr>
                        <tr style="border-top: 2px solid #e2e8f0;">
                            <td style="padding: 12px 0;"><span class="total-amount">Total Paid:</span></td>
                            <td style="padding: 12px 0;"><span class="total-amount">KES %.2f</span></td>
                        </tr>
                    </table>
                </td>
            </tr>
        </table>

        <div class="footer">
            Thank you for your business!<br>
            For inquiries or support, contact us at <strong>%s</strong>.
        </div>
    </div>
</body>
</html>`,
		payment.ID,
		primaryColor,
		primaryColor,
		primaryColor,
		primaryColor,
		logoImgTag,
		companyName,
		payment.ID,
		payment.CreatedAt.Format("2006-01-02 15:04"),
		customerName,
		customerPhone,
		accountNumber,
		strings.ToUpper(payment.Method),
		receiptNumber,
		packageName,
		speedUpload,
		speedDownload,
		billingCycle,
		zoneName,
		payment.Amount,
		payment.Amount,
		payment.Amount,
		settings["support_phone"],
	)
}

// PaymentVerifyCode allows a customer or portal user to self-verify an M-Pesa transaction code
// (e.g. from an STK push or direct Paybill payment) and instantly activate their internet session.
func PaymentVerifyCode(c *fiber.Ctx) error {
	var body struct {
		Code string `json:"code"`
		Mac  string `json:"mac"`
		IP   string `json:"ip"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	code := strings.ToUpper(strings.TrimSpace(body.Code))
	if code == "" || len(code) < 6 {
		return utils.ErrorResponse(c, "Please enter a valid M-Pesa transaction code.", "", fiber.StatusUnprocessableEntity)
	}

	// 1. Check completed or pending Payment records
	var payment models.Payment
	if err := config.DB.Preload("Package").Preload("Zone").Preload("Customer").
		Where("UPPER(mpesa_receipt_number) = ? OR UPPER(mpesa_transaction_id) = ?", code, code).
		Order("id DESC").
		First(&payment).Error; err == nil {

		if payment.Status == "completed" {
			// Link MAC / device if provided and not yet associated
			if body.Mac != "" && payment.Customer != nil {
				payment.Customer.MacAddress = &body.Mac
				config.DB.Model(payment.Customer).Update("mac_address", body.Mac)
				if mikrotikSvcGlobal != nil && payment.Zone != nil && payment.Package != nil {
					go mikrotikSvcGlobal.WhitelistMAC(payment.Zone, body.Mac, payment.Package)
				}
			}
			return utils.SuccessResponse(c, payment, "Payment verified successfully.")
		}

		if payment.Status == "pending" {
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"success": false,
				"pending": true,
				"message": "Payment is still processing with Safaricom. Give it a minute and try again.",
			})
		}

		if payment.Status == "failed" {
			reason := "Payment was not completed."
			if payment.StatusReason != nil && *payment.StatusReason != "" {
				reason = *payment.StatusReason
			}
			return utils.ErrorResponse(c, reason, "Payment failed.", fiber.StatusBadRequest)
		}
	}

	// 2. Check UnmatchedC2BPayment (Paybill confirmation received, waiting to be claimed)
	var unmatched models.UnmatchedC2BPayment
	if err := config.DB.Where("UPPER(trans_id) = ?", code).First(&unmatched).Error; err == nil {
		if unmatched.Status == "resolved" {
			if unmatched.ResolvedPaymentID != nil {
				var resPayment models.Payment
				if err := config.DB.Preload("Package").Preload("Zone").First(&resPayment, *unmatched.ResolvedPaymentID).Error; err == nil {
					return utils.SuccessResponse(c, resPayment, "Payment already verified.")
				}
			}
			return utils.SuccessResponse(c, fiber.Map{"code": code}, "Payment already verified.")
		}

		// Pending unmatched payment! Reconcile immediately with the customer claiming it.
		claims := middleware.OptionalCustomerClaims(c)
		var customer models.Customer
		foundCust := false

		if claims != nil && claims.CustomerID > 0 {
			if err := config.DB.First(&customer, claims.CustomerID).Error; err == nil {
				foundCust = true
			}
		}

		if !foundCust && body.Mac != "" {
			if err := config.DB.Where("mac_address = ?", body.Mac).Order("id DESC").First(&customer).Error; err == nil {
				foundCust = true
			}
		}

		if !foundCust && unmatched.Phone != "" {
			phoneSuffix := unmatched.Phone
			if len(phoneSuffix) >= 9 {
				phoneSuffix = phoneSuffix[len(phoneSuffix)-9:]
			}
			if err := config.DB.Where("phone LIKE ?", "%"+phoneSuffix).First(&customer).Error; err == nil {
				foundCust = true
			}
		}

		// Fallback: create fresh customer or link to latest guest
		if !foundCust {
			var guest models.Customer
			if err := config.DB.Where("phone LIKE 'GUEST%'").Order("id DESC").First(&guest).Error; err == nil {
				customer = guest
				foundCust = true
			} else {
				customer = models.Customer{
					Name:          "Customer " + code,
					Phone:         unmatched.Phone,
					ZoneID:        1,
					Type:          "hotspot",
					Status:        "active",
					CreditBalance: 0,
				}
				if body.Mac != "" {
					customer.MacAddress = &body.Mac
				}
				config.DB.Create(&customer)
				foundCust = true
			}
		}

		if body.Mac != "" {
			customer.MacAddress = &body.Mac
		}

		// Credit customer balance
		customer.CreditBalance += unmatched.Amount
		config.DB.Model(&customer).Updates(map[string]interface{}{
			"credit_balance": customer.CreditBalance,
			"mac_address":    customer.MacAddress,
		})

		noteStr := fmt.Sprintf("M-Pesa C2B Paybill %s (Self-claimed via code verification)", unmatched.TransID)
		config.DB.Create(&models.CreditLog{
			CustomerID: customer.ID,
			Amount:     unmatched.Amount,
			Type:       "credit",
			Note:       &noteStr,
		})

		// Auto-match package by exact amount in customer's zone
		var pkg models.Package
		if err := config.DB.Where("zone_id = ? AND price = ? AND status = 'active'", customer.ZoneID, unmatched.Amount).First(&pkg).Error; err != nil {
			config.DB.First(&pkg, customer.PackageID)
		}

		var packageIDPtr *uint
		if pkg.ID > 0 && customer.CreditBalance >= pkg.Price {
			packageIDPtr = &pkg.ID
			customer.PackageID = pkg.ID
			customer.CreditBalance -= pkg.Price

			durationMinutes := 30 * 24 * 60
			if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
				durationMinutes = *pkg.TimeLimitMinutes
			} else if pkg.BillingCycle == "hourly" {
				durationMinutes = 60
			} else if pkg.BillingCycle == "daily" {
				durationMinutes = 24 * 60
			} else if pkg.BillingCycle == "weekly" {
				durationMinutes = 7 * 24 * 60
			}

			newExpiry := time.Now().Add(time.Duration(durationMinutes) * time.Minute)
			if customer.ExpiresAt != nil && customer.ExpiresAt.After(time.Now()) {
				newExpiry = customer.ExpiresAt.Add(time.Duration(durationMinutes) * time.Minute)
			}

			config.DB.Model(&customer).Updates(map[string]interface{}{
				"package_id":     pkg.ID,
				"credit_balance": customer.CreditBalance,
				"expires_at":     newExpiry,
				"status":         "active",
			})

			// Reactivate on MikroTik router
			if mikrotikSvcGlobal != nil && customer.ZoneID > 0 {
				var zone models.Zone
				if err := config.DB.First(&zone, customer.ZoneID).Error; err == nil {
					customer.Package = &pkg
					go mikrotikSvcGlobal.ReactivateCustomer(&zone, &customer)
				}
			}
		}

		transIDStr := unmatched.TransID
		custIDPtr := &customer.ID
		newPayment := models.Payment{
			CustomerID:         custIDPtr,
			ZoneID:             customer.ZoneID,
			PackageID:          packageIDPtr,
			Phone:              unmatched.Phone,
			Amount:             unmatched.Amount,
			Currency:           "KES",
			Method:             "mpesa_c2b",
			Status:             "completed",
			MpesaReceiptNumber: &transIDStr,
			MpesaTransactionID: &transIDStr,
		}
		if body.Mac != "" {
			newPayment.MacAddress = body.Mac
		}
		if body.IP != "" {
			newPayment.IpAddress = body.IP
		}
		config.DB.Create(&newPayment)

		now := time.Now()
		config.DB.Model(&unmatched).Updates(map[string]interface{}{
			"status":               "resolved",
			"resolved_zone_id":     customer.ZoneID,
			"resolved_customer_id": custIDPtr,
			"resolved_payment_id":  newPayment.ID,
			"resolved_at":          now,
		})

		return utils.SuccessResponse(c, newPayment, "Payment verified and service activated!")
	}

	// 3. Not found in records
	return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
		"success": false,
		"pending": true,
		"message": "We could not find an M-Pesa payment matching that code yet. If you just sent it, please give it a moment and try again.",
	})
}

