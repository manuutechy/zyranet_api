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

// GeneratePlatformInvoices creates one Draft PlatformInvoice per active
// Organization with a configured BillingRatePerCustomer, for the given
// period. It's shared between the on-demand HTTP endpoint and the
// cmd/generate_platform_invoices cron job so both paths bill identically.
// An org that already has an invoice for this exact period is skipped
// rather than double-billed.
func GeneratePlatformInvoices(periodStart, periodEnd time.Time) ([]models.PlatformInvoice, error) {
	var orgs []models.Organization
	if err := config.DB.Where("status = ? AND billing_rate_per_customer > 0", "active").Find(&orgs).Error; err != nil {
		return nil, err
	}

	created := make([]models.PlatformInvoice, 0, len(orgs))
	for _, org := range orgs {
		var existing int64
		config.DB.Model(&models.PlatformInvoice{}).
			Where("organization_id = ? AND period_start = ? AND period_end = ?", org.ID, periodStart, periodEnd).
			Count(&existing)
		if existing > 0 {
			continue
		}

		var zoneIDs []uint
		config.DB.Model(&models.Zone{}).Where("organization_id = ?", org.ID).Pluck("id", &zoneIDs)

		var activeCustomers int64
		if len(zoneIDs) > 0 {
			config.DB.Model(&models.Customer{}).
				Where("zone_id IN (?) AND status = ? AND created_at <= ?", zoneIDs, "active", periodEnd).
				Count(&activeCustomers)
		}

		invoice := models.PlatformInvoice{
			OrganizationID:      org.ID,
			PeriodStart:         periodStart,
			PeriodEnd:           periodEnd,
			ActiveCustomerCount: activeCustomers,
			RatePerCustomer:     org.BillingRatePerCustomer,
			Total:               float64(activeCustomers) * org.BillingRatePerCustomer,
			Status:              "draft",
		}
		if err := config.DB.Create(&invoice).Error; err != nil {
			continue
		}
		created = append(created, invoice)
	}
	return created, nil
}

// PreviousCalendarMonth returns the [start, end) of the calendar month
// before the current one, in UTC. Exported so cmd/generate_platform_invoices
// can compute the same default period as the on-demand HTTP endpoint.
func PreviousCalendarMonth() (time.Time, time.Time) {
	now := time.Now().UTC()
	firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	firstOfLastMonth := firstOfThisMonth.AddDate(0, -1, 0)
	return firstOfLastMonth, firstOfThisMonth
}

// PlatformInvoiceGenerate triggers invoice generation on demand (SA clicks
// "Generate Invoices" in the platform app). Defaults to the previous
// calendar month if no period is given.
func PlatformInvoiceGenerate(c *fiber.Ctx) error {
	var body struct {
		PeriodStart string `json:"period_start"`
		PeriodEnd   string `json:"period_end"`
	}
	c.BodyParser(&body) //nolint:errcheck

	periodStart, periodEnd := PreviousCalendarMonth()
	if body.PeriodStart != "" && body.PeriodEnd != "" {
		ps, err1 := time.Parse("2006-01-02", body.PeriodStart)
		pe, err2 := time.Parse("2006-01-02", body.PeriodEnd)
		if err1 != nil || err2 != nil {
			return utils.ErrorResponse(c, "period_start and period_end must be YYYY-MM-DD.", "", fiber.StatusUnprocessableEntity)
		}
		periodStart, periodEnd = ps, pe
	}

	invoices, err := GeneratePlatformInvoices(periodStart, periodEnd)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Invoice generation failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, invoices, "Generated invoices for the billing period.", fiber.StatusCreated)
}

// PlatformInvoiceIndex lists invoices across all tenants, optionally
// filtered by organization or status.
func PlatformInvoiceIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var invoices []models.PlatformInvoice
	var total int64

	query := config.DB.Model(&models.PlatformInvoice{}).Preload("Organization")
	if orgID := c.Query("organization_id"); orgID != "" {
		query = query.Where("organization_id = ?", orgID)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	query.Count(&total)
	query.Order("period_start DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&invoices)
	return utils.PaginatedResponse(c, invoices, total, page, perPage)
}

// PlatformInvoiceUpdate transitions an invoice's status (issue it / mark
// paid / void it).
func PlatformInvoiceUpdate(c *fiber.Ctx) error {
	var invoice models.PlatformInvoice
	if err := config.DB.First(&invoice, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Invoice not found.", "", fiber.StatusNotFound)
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	allowed := map[string]bool{"draft": true, "issued": true, "paid": true, "overdue": true, "void": true}
	if !allowed[body.Status] {
		return utils.ErrorResponse(c, "Invalid status.", "", fiber.StatusUnprocessableEntity)
	}

	updates := map[string]interface{}{"status": body.Status}
	now := time.Now()
	switch body.Status {
	case "issued":
		updates["issued_at"] = now
		due := now.AddDate(0, 0, 14)
		updates["due_at"] = due
	case "paid":
		updates["paid_at"] = now
	}

	if err := config.DB.Model(&invoice).Updates(updates).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Organization").First(&invoice, invoice.ID)
	return utils.SuccessResponse(c, invoice, "Invoice updated successfully.")
}

// AdminPlatformInvoiceIndex is the ISP-facing (read-only) view of what
// Zyra Net has invoiced this tenant — mounted under the regular admin API
// (not /platform/*) and scoped to the caller's own Organization, so one
// ISP can never see another's platform invoices.
func AdminPlatformInvoiceIndex(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	page, perPage := utils.ParsePage(c)

	var invoices []models.PlatformInvoice
	var total int64
	query := config.DB.Model(&models.PlatformInvoice{}).Where("organization_id = ?", claims.OrganizationID)
	query.Count(&total)
	query.Order("period_start DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&invoices)
	return utils.PaginatedResponse(c, invoices, total, page, perPage)
}

func renderPlatformInvoiceHTML(invoice *models.PlatformInvoice, org *models.Organization) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Zyra Net Platform Invoice #%d</title>
    <style>
        body { font-family: 'Helvetica Neue', Helvetica, Arial, sans-serif; color: #333; margin: 0; padding: 20px; background-color: #f8fafc; }
        .invoice-box { max-width: 700px; margin: auto; padding: 40px; border: 1px solid #e2e8f0; font-size: 14px; line-height: 24px; border-radius: 12px; background: #fff; }
        .invoice-box table { width: 100%%; line-height: inherit; text-align: left; border-collapse: collapse; }
        .invoice-box table td { padding: 8px; vertical-align: top; }
        .title { font-size: 28px; font-weight: 800; color: #0f172a; }
        .total-amount { font-size: 20px; font-weight: bold; color: #2563eb; }
    </style>
</head>
<body>
    <div class="invoice-box">
        <p class="title">Zyra Net Platform Invoice</p>
        <p><strong>Invoice #:</strong> PLAT-%d<br>
        <strong>Billed To:</strong> %s<br>
        <strong>Period:</strong> %s &ndash; %s<br>
        <strong>Status:</strong> %s</p>
        <table>
            <tr><td>Active Customers</td><td>%d</td></tr>
            <tr><td>Rate per Customer</td><td>KES %.2f</td></tr>
            <tr><td><span class="total-amount">Total Due</span></td><td><span class="total-amount">KES %.2f</span></td></tr>
        </table>
    </div>
</body>
</html>`,
		invoice.ID, invoice.ID, org.Name,
		invoice.PeriodStart.Format("2006-01-02"), invoice.PeriodEnd.Format("2006-01-02"),
		invoice.Status, invoice.ActiveCustomerCount, invoice.RatePerCustomer, invoice.Total)
}

// PlatformInvoiceSendEmail emails the platform invoice to the ISP's
// contact email using the existing EmailService.
func PlatformInvoiceSendEmail(c *fiber.Ctx) error {
	var invoice models.PlatformInvoice
	if err := config.DB.Preload("Organization").First(&invoice, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Invoice not found.", "", fiber.StatusNotFound)
	}
	if invoice.Organization == nil || invoice.Organization.ContactEmail == "" {
		return utils.ErrorResponse(c, "This organization has no contact email on file.", "", fiber.StatusUnprocessableEntity)
	}

	html := renderPlatformInvoiceHTML(&invoice, invoice.Organization)
	subject := fmt.Sprintf("Zyra Net Platform Invoice PLAT-%d", invoice.ID)
	if err := emailSvcGlobal.Send(invoice.Organization.ContactEmail, subject, html); err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to send invoice email.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Invoice emailed successfully.")
}

// PlatformInvoiceSendSMS texts an invoice summary to the ISP's contact
// phone via the existing Hostpinnacle-backed SmsService.
func PlatformInvoiceSendSMS(c *fiber.Ctx) error {
	var invoice models.PlatformInvoice
	if err := config.DB.Preload("Organization").First(&invoice, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Invoice not found.", "", fiber.StatusNotFound)
	}
	if invoice.Organization == nil || invoice.Organization.ContactPhone == nil || *invoice.Organization.ContactPhone == "" {
		return utils.ErrorResponse(c, "This organization has no contact phone on file.", "", fiber.StatusUnprocessableEntity)
	}

	msg := fmt.Sprintf("Zyra Net Platform Invoice PLAT-%d: KES %.2f due for %s. %d active customers @ KES %.2f each.",
		invoice.ID, invoice.Total, invoice.PeriodStart.Format("Jan 2006"), invoice.ActiveCustomerCount, invoice.RatePerCustomer)

	if _, err := smsSvcGlobal.Send(*invoice.Organization.ContactPhone, msg); err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to send invoice SMS.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Invoice SMS sent successfully.")
}
