package handlers

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
)

// parseCustomerDateTime attempts to parse user-provided date/time strings.
func parseCustomerDateTime(val string, endOfDay bool) (time.Time, error) {
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, val); err == nil {
			if layout == "2006-01-02" && endOfDay {
				t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date format: %s", val)
}

// CustomerIndex lists customers with filters.
func CustomerIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var customers []models.Customer
	var total int64

	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}

	query := config.DB.Model(&models.Customer{}).Where("zone_id IN (?)", orgZoneIDs)
	claims := middleware.GetClaims(c)
	if claims.Role == "zone_manager" && claims.ZoneID != nil {
		query = query.Where("zone_id = ?", *claims.ZoneID)
	} else if z := c.Query("zone_id"); z != "" {
		query = query.Where("zone_id = ?", z)
	}
	if t := c.Query("type"); t != "" {
		query = query.Where("type = ?", t)
	}
	// Card totals for this zone/type (all pages, before the card/status/search
	// filters) — the page used to count only the 15 rows on screen.
	summary := customerSummary(query.Session(&gorm.Session{}))
	if s := c.Query("status"); s != "" {
		query = query.Where("status = ?", s)
	}
	if search := c.Query("search"); search != "" {
		// Phones are stored as 2547…; staff type 07…, so match that form too.
		phoneLike := "%" + search + "%"
		if p := phoneSearchForm(search); p != "" {
			phoneLike = "%" + p + "%"
		}
		// Customer number (homes): exact match, however it was typed.
		customerNo, _ := models.ParseCustomerNo(search)
		query = query.Where("name LIKE ? OR phone LIKE ? OR phone LIKE ? OR pppoe_username LIKE ? OR account_number LIKE ? OR mac_address LIKE ? OR customer_no = ?",
			"%"+search+"%", "%"+search+"%", phoneLike, "%"+search+"%", "%"+search+"%", "%"+search+"%", customerNo)
	}

	// Active Now filter (active subscription that has not expired)
	if c.Query("active_now") == "true" || c.Query("active_now") == "1" {
		now := time.Now()
		query = query.Where("status = ? AND (expires_at > ? OR expires_at IS NULL)", "active", now)
	}

	// Date and time range filters (tailored for active subscribers)
	activeMode := c.Query("active_mode", "window") // window | activated | expired
	dateParam := c.Query("date")
	fromParam := c.Query("date_from")
	if fromParam == "" {
		fromParam = c.Query("from")
	}
	toParam := c.Query("date_to")
	if toParam == "" {
		toParam = c.Query("to")
	}

	// Single date shortcut (e.g. ?date=2026-09-08)
	if dateParam != "" {
		if startDate, err := parseCustomerDateTime(dateParam, false); err == nil {
			endDate, _ := parseCustomerDateTime(dateParam, true)
			if activeMode == "activated" {
				query = query.Where("created_at >= ? AND created_at <= ?", startDate, endDate)
			} else if activeMode == "expired" {
				query = query.Where("expires_at >= ? AND expires_at <= ?", startDate, endDate)
			} else {
				// Active during this day: customer was activated before end-of-day AND (expires after start-of-day OR is unexpired)
				query = query.Where("created_at <= ? AND (expires_at >= ? OR expires_at IS NULL)", endDate, startDate)
			}
		}
	} else if fromParam != "" || toParam != "" {
		var fromTime, toTime time.Time
		hasFrom := false
		hasTo := false
		if fromParam != "" {
			if parsed, err := parseCustomerDateTime(fromParam, false); err == nil {
				fromTime = parsed
				hasFrom = true
			}
		}
		if toParam != "" {
			if parsed, err := parseCustomerDateTime(toParam, true); err == nil {
				toTime = parsed
				hasTo = true
			}
		}

		if hasFrom && hasTo {
			if activeMode == "activated" {
				query = query.Where("created_at >= ? AND created_at <= ?", fromTime, toTime)
			} else if activeMode == "expired" {
				query = query.Where("expires_at >= ? AND expires_at <= ?", fromTime, toTime)
			} else {
				// Was active during window: activated on/before window end, expires on/after window start
				query = query.Where("created_at <= ? AND (expires_at >= ? OR expires_at IS NULL)", toTime, fromTime)
			}
		} else if hasFrom {
			if activeMode == "activated" {
				query = query.Where("created_at >= ?", fromTime)
			} else if activeMode == "expired" {
				query = query.Where("expires_at >= ?", fromTime)
			} else {
				query = query.Where("expires_at >= ? OR expires_at IS NULL OR created_at >= ?", fromTime, fromTime)
			}
		} else if hasTo {
			if activeMode == "activated" {
				query = query.Where("created_at <= ?", toTime)
			} else if activeMode == "expired" {
				query = query.Where("expires_at <= ?", toTime)
			} else {
				query = query.Where("created_at <= ?", toTime)
			}
		}
	}

	query.Count(&total)

	// Sorting options
	orderClause := "created_at DESC"
	switch c.Query("sort") {
	case "created_asc":
		orderClause = "created_at ASC"
	case "created_desc":
		orderClause = "created_at DESC"
	case "expires_asc":
		orderClause = "expires_at ASC"
	case "expires_desc":
		orderClause = "expires_at DESC"
	case "name_asc":
		orderClause = "name ASC"
	case "name_desc":
		orderClause = "name DESC"
	}

	query.Preload("Zone").Preload("Package").Order(orderClause).Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&customers)
	return c.JSON(fiber.Map{
		"success": true,
		"data":    customers,
		"meta":    fiber.Map{"total": total, "page": page, "per_page": perPage},
		"summary": summary,
		"message": "",
	})
}

// customerCounts are the customers page's summary cards.
type customerCounts struct {
	Total        int64   `json:"total"`
	Active       int64   `json:"active"`
	ActiveNow    int64   `json:"active_now"`
	Today        int64   `json:"today"`
	ExpiringSoon int64   `json:"expiring_soon"`
	Expired      int64   `json:"expired"`
	Suspended    int64   `json:"suspended"`
	Hotspot      int64   `json:"hotspot"`
	PPPoE        int64   `json:"pppoe" gorm:"column:pppoe"`
	TotalCredit  float64 `json:"total_credit"`
}

func customerSummary(q *gorm.DB) customerCounts {
	now := time.Now()
	eat := utils.Kenya(now)
	dayStart := time.Date(eat.Year(), eat.Month(), eat.Day(), 0, 0, 0, 0, utils.EAT).In(time.Local)
	dayEnd := dayStart.AddDate(0, 0, 1)
	var out customerCounts
	soon := now.Add(2 * time.Hour)
	q.Select(`COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0) AS active,
		COALESCE(SUM(CASE WHEN status = 'active' AND (expires_at IS NULL OR expires_at > ?) THEN 1 ELSE 0 END), 0) AS active_now,
		COALESCE(SUM(CASE WHEN (created_at >= ? AND created_at < ?) OR (expires_at >= ? AND expires_at < ?) THEN 1 ELSE 0 END), 0) AS today,
		COALESCE(SUM(CASE WHEN status = 'active' AND expires_at > ? AND expires_at <= ? THEN 1 ELSE 0 END), 0) AS expiring_soon,
		COALESCE(SUM(CASE WHEN status = 'expired' OR expires_at <= ? THEN 1 ELSE 0 END), 0) AS expired,
		COALESCE(SUM(CASE WHEN status = 'suspended' THEN 1 ELSE 0 END), 0) AS suspended,
		COALESCE(SUM(CASE WHEN type = 'hotspot' THEN 1 ELSE 0 END), 0) AS hotspot,
		COALESCE(SUM(CASE WHEN type = 'pppoe' THEN 1 ELSE 0 END), 0) AS pppoe,
		COALESCE(SUM(credit_balance), 0) AS total_credit`,
		now, dayStart, dayEnd, dayStart, dayEnd, now, soon, now).
		Scan(&out)
	return out
}

// CustomerStore creates a new customer.
func CustomerStore(c *fiber.Ctx) error {
	var customer models.Customer
	if err := c.BodyParser(&customer); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" && claims.Role != "zone_manager" {
		return utils.ErrorResponse(c, "Unauthorized to register customers.", "", fiber.StatusForbidden)
	}
	if claims.Role == "zone_manager" && claims.ZoneID != nil && customer.ZoneID != *claims.ZoneID {
		return utils.ErrorResponse(c, "Unauthorized to add customer to this zone.", "", fiber.StatusForbidden)
	}
	var targetZone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&targetZone, customer.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid zone for this organization.", "", fiber.StatusUnprocessableEntity)
	}
	if customer.Type == "pppoe" && (customer.PPPoEPassword == nil || *customer.PPPoEPassword == "") {
		pw := generatePPPoEPassword()
		customer.PPPoEPassword = &pw
	}
	if customer.Status == "active" && customer.ExpiresAt == nil && customer.PackageID > 0 {
		var pkg models.Package
		if err := config.DB.First(&pkg, customer.PackageID).Error; err == nil {
			exp := utils.CalculateExpiry(pkg.BillingCycle, nil, pkg.TimeLimitMinutes)
			customer.ExpiresAt = &exp
		}
	}
	if err := config.DB.Create(&customer).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create customer.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zone").Preload("Package").First(&customer, customer.ID)
	return utils.SuccessResponse(c, customer, "Customer created successfully.", fiber.StatusCreated)
}

// generatePPPoEPassword creates a random PPPoE secret so customers created
// without an explicit password don't all end up sharing a guessable default.
func generatePPPoEPassword() string {
	b := make([]byte, 6)
	crand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

// customerInScope reports whether the requesting admin is allowed to act on
// this customer — zone_managers are restricted to their own zone. Tenant
// isolation itself is enforced earlier, by findCustomerOrFail scoping the
// lookup to the caller's Organization.
func customerInScope(c *fiber.Ctx, customer *models.Customer) bool {
	claims := middleware.GetClaims(c)
	if claims.Role == "zone_manager" && claims.ZoneID != nil && customer.ZoneID != *claims.ZoneID {
		return false
	}
	return true
}

// findCustomerOrFail loads a customer by ID, scoped to the caller's
// Organization via its zone, so no admin can reach another tenant's
// customer by guessing an ID.
func findCustomerOrFail(c *fiber.Ctx, preload ...string) (*models.Customer, error) {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
		return nil, fiber.ErrInternalServerError
	}
	query := config.DB.Where("zone_id IN (?)", orgZoneIDs)
	for _, p := range preload {
		query = query.Preload(p)
	}
	var customer models.Customer
	if err := query.First(&customer, c.Params("id")).Error; err != nil {
		utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
		return nil, fiber.ErrNotFound
	}
	return &customer, nil
}

// CustomerShow returns a single customer.
func CustomerShow(c *fiber.Ctx) error {
	customer, err := findCustomerOrFail(c, "Zone", "Package")
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to view this customer.", "", fiber.StatusForbidden)
	}
	return utils.SuccessResponse(c, customer, "")
}

// customerEditableFields are the keys PUT /customers/:id may change.
var customerEditableFields = map[string]bool{
	"name": true, "phone": true, "email": true, "zone_id": true, "package_id": true,
	"type": true, "pppoe_username": true, "pppoe_password": true, "mac_address": true,
	"status": true, "expires_at": true,
}

// CustomerUpdate updates a customer.
func CustomerUpdate(c *fiber.Ctx) error {
	customerPtr, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	customer := *customerPtr
	if !customerInScope(c, &customer) {
		return utils.ErrorResponse(c, "Unauthorized to update this customer.", "", fiber.StatusForbidden)
	}
	// Same roles that can register customers: an edit can set a plan, status
	// and expiry, i.e. give internet time.
	if claims := middleware.GetClaims(c); claims.Role != "super_admin" && claims.Role != "zone_manager" {
		return utils.ErrorResponse(c, "Unauthorized to edit customers.", "", fiber.StatusForbidden)
	}
	var raw map[string]interface{}
	if err := c.BodyParser(&raw); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	// Only profile/plan fields. credit_balance in particular is money and
	// only moves through add-credit (owner-only, logged); it used to be
	// settable here by any staff role.
	body := map[string]interface{}{}
	for k, v := range raw {
		if customerEditableFields[k] {
			body[k] = v
		}
	}
	if z, ok := body["zone_id"]; ok {
		zf, _ := z.(float64)
		var zone models.Zone
		claims := middleware.GetClaims(c)
		if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, uint(zf)).Error; err != nil || zf <= 0 {
			return utils.ErrorResponse(c, "Invalid zone for this organization.", "", fiber.StatusUnprocessableEntity)
		}
		if claims.Role == "zone_manager" && claims.ZoneID != nil && zone.ID != *claims.ZoneID {
			return utils.ErrorResponse(c, "Unauthorized to move customer to this zone.", "", fiber.StatusForbidden)
		}
	}
	if p, ok := body["package_id"]; ok {
		pf, _ := p.(float64)
		orgZoneIDs, _ := middleware.OrgZoneIDs(c)
		var pkg models.Package
		if err := config.DB.Where("zone_id IN (?)", orgZoneIDs).First(&pkg, uint(pf)).Error; err != nil || pf <= 0 {
			return utils.ErrorResponse(c, "That package does not belong to your organization.", "", fiber.StatusUnprocessableEntity)
		}
	}
	if len(body) == 0 {
		return utils.ErrorResponse(c, "Nothing to update.", "", fiber.StatusUnprocessableEntity)
	}
	if statusVal, ok := body["status"].(string); ok && statusVal == "active" {
		if _, hasExp := body["expires_at"]; !hasExp {
			if customer.ExpiresAt == nil || customer.ExpiresAt.Before(time.Now()) {
				targetPkgID := customer.PackageID
				if pkgVal, okPkg := body["package_id"].(float64); okPkg && uint(pkgVal) > 0 {
					targetPkgID = uint(pkgVal)
				}
				var pkg models.Package
				if err := config.DB.First(&pkg, targetPkgID).Error; err == nil {
					exp := utils.CalculateExpiry(pkg.BillingCycle, nil, pkg.TimeLimitMinutes)
					body["expires_at"] = exp
				}
			}
		}
	}
	if err := config.DB.Model(&customer).Updates(body).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zone").Preload("Package").First(&customer, customer.ID)
	return utils.SuccessResponse(c, customer, "Customer updated successfully.")
}

// CustomerDestroy soft-deletes a customer (super_admin only).
func CustomerDestroy(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to delete customers.", "", fiber.StatusForbidden)
	}
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if err := config.DB.Delete(&models.Customer{}, customer.ID).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Customer deleted successfully.")
}

// CustomerSuspend suspends a customer's account.
func CustomerSuspend(c *fiber.Ctx) error {
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to suspend this customer.", "", fiber.StatusForbidden)
	}
	if err := config.DB.Model(customer).Update("status", "suspended").Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	customer.Status = "suspended"
	return utils.SuccessResponse(c, customer, "Customer account suspended successfully.")
}

// CustomerActivate reactivates a customer's account.
func CustomerActivate(c *fiber.Ctx) error {
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to activate this customer.", "", fiber.StatusForbidden)
	}
	if claims := middleware.GetClaims(c); claims.Role != "super_admin" && claims.Role != "zone_manager" {
		return utils.ErrorResponse(c, "Unauthorized to activate customers.", "", fiber.StatusForbidden)
	}
	if err := config.DB.Model(customer).Update("status", "active").Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	customer.Status = "active"
	return utils.SuccessResponse(c, customer, "Customer account activated successfully.")
}

// CustomerPayments returns payment history for a customer.
func CustomerPayments(c *fiber.Ctx) error {
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to view this customer.", "", fiber.StatusForbidden)
	}
	var payments []models.Payment
	config.DB.Where("customer_id = ?", customer.ID).Order("created_at DESC").Find(&payments)
	return utils.SuccessResponse(c, payments, "")
}

// CustomerSessions returns internet sessions for a customer.
func CustomerSessions(c *fiber.Ctx) error {
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to view this customer.", "", fiber.StatusForbidden)
	}
	var sessions []models.Session
	config.DB.Where("customer_id = ?", customer.ID).Order("created_at DESC").Find(&sessions)
	return utils.SuccessResponse(c, sessions, "")
}

// CustomerAddCredit adjusts a customer's credit balance. Credit is money, so
// only the ISP's owner (super_admin) may mint or remove it by hand.
func CustomerAddCredit(c *fiber.Ctx) error {
	if claims := middleware.GetClaims(c); claims == nil || claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Only the account owner can adjust credit balances.", "Forbidden.", fiber.StatusForbidden)
	}
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to adjust credit for this customer.", "", fiber.StatusForbidden)
	}

	var body struct {
		Amount float64 `json:"amount"`
		Type   string  `json:"type"` // credit | debit
		Note   string  `json:"note"`
	}
	if err := c.BodyParser(&body); err != nil || body.Amount <= 0 {
		return utils.ErrorResponse(c, "Invalid amount.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if body.Type != "credit" && body.Type != "debit" {
		return utils.ErrorResponse(c, "Type must be 'credit' or 'debit'.", "", fiber.StatusUnprocessableEntity)
	}
	// One atomic UPDATE (and the log row) in a transaction: concurrent
	// adjustments can't overwrite each other, and a debit can't overdraw.
	delta := body.Amount
	if body.Type == "debit" {
		delta = -body.Amount
	}
	claims := middleware.GetClaims(c)
	errInsufficient := fmt.Errorf("insufficient")
	err = config.DB.Transaction(func(tx *gorm.DB) error {
		ok, err := services.AdjustCredit(tx, customer.ID, delta)
		if err != nil {
			return err
		}
		if !ok {
			return errInsufficient
		}
		note := body.Note
		return tx.Create(&models.CreditLog{
			CustomerID: customer.ID,
			Amount:     body.Amount,
			Type:       body.Type,
			Note:       &note,
			AddedBy:    &claims.UserID,
		}).Error
	})
	if err == errInsufficient {
		return utils.ErrorResponse(c, "Insufficient credit balance for debit.", "", fiber.StatusUnprocessableEntity)
	}
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to adjust credit.", fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"credit_balance": services.CreditBalanceOf(config.DB, customer.ID),
	}, "Credit "+body.Type+" applied successfully.")
}

// CustomerPayments_Admin is the admin-facing alias for customer payment history.
func CustomerPayments_Admin(c *fiber.Ctx) error {
	return CustomerPayments(c)
}

// CustomerCreditLogs returns credit/debit history for a customer.
func CustomerCreditLogs(c *fiber.Ctx) error {
	customer, err := findCustomerOrFail(c)
	if err != nil {
		return err
	}
	if !customerInScope(c, customer) {
		return utils.ErrorResponse(c, "Unauthorized to view this customer.", "", fiber.StatusForbidden)
	}
	page, perPage := utils.ParsePage(c)
	var logs []models.CreditLog
	var total int64
	config.DB.Model(&models.CreditLog{}).Where("customer_id = ?", customer.ID).Count(&total)
	config.DB.Preload("Admin").Where("customer_id = ?", customer.ID).
		Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&logs)
	return utils.PaginatedResponse(c, logs, total, page, perPage)
}

// PurgeInactiveCustomers cleans up abandoned guest/voucher test accounts older than 7 days.
// Genuine customers (even with expired plans) are retained so their accounts, history, and device sessions remain valid upon renewal.
// The startup sweep covers every ISP; PurgeInactiveCustomersInZones limits it
// to one ISP's zones.
func PurgeInactiveCustomers() (int64, error) {
	return purgeInactiveCustomers(nil, false)
}

// PurgeInactiveCustomersInZones is PurgeInactiveCustomers restricted to the
// given zones (an ISP's own), so one ISP's admin can never delete another's
// customers.
func PurgeInactiveCustomersInZones(zoneIDs []uint) (int64, error) {
	return purgeInactiveCustomers(zoneIDs, true)
}

func purgeInactiveCustomers(zoneIDs []uint, scoped bool) (int64, error) {
	if config.DB == nil {
		return 0, nil
	}
	if scoped && len(zoneIDs) == 0 {
		return 0, nil
	}
	now := time.Now()
	cutoff := now.Add(-7 * 24 * time.Hour)
	var inactiveIDs []uint
	q := config.DB.Model(&models.Customer{}).
		Where("(phone LIKE 'GUEST%' OR phone LIKE 'VCHR%' OR account_number LIKE 'ZYR#GUEST%' OR account_number LIKE 'ZYR#VCHR%') AND status != 'active' AND created_at < ?", cutoff).
		// Never purge an account still holding credit — that money is owed to the customer.
		Where("credit_balance <= 0")
	if scoped {
		q = q.Where("zone_id IN ?", zoneIDs)
	}
	err := q.Pluck("id", &inactiveIDs).Error
	if err != nil {
		log.Printf("[PurgeInactiveCustomers] Error querying inactive customers: %v", err)
		return 0, err
	}

	if len(inactiveIDs) > 0 {
		// 1. Delete associated customer devices for inactive
		config.DB.Where("customer_id IN ?", inactiveIDs).Delete(&models.CustomerDevice{})
		// 2. Delete associated sessions
		config.DB.Where("customer_id IN ?", inactiveIDs).Delete(&models.Session{})
		// 3. Delete credit logs associated with purged guest/test accounts
		config.DB.Where("customer_id IN ?", inactiveIDs).Delete(&models.CreditLog{})
		// 4. Clear customer_id on payments & tickets to preserve financial audit logs
		config.DB.Model(&models.Payment{}).Where("customer_id IN ?", inactiveIDs).Update("customer_id", nil)
		config.DB.Model(&models.Ticket{}).Where("customer_id IN ?", inactiveIDs).Update("customer_id", nil)
		config.DB.Model(&models.Voucher{}).Where("used_by IN ?", inactiveIDs).Update("used_by", nil)
		// 5. Hard delete the abandoned guest accounts
		res := config.DB.Unscoped().Where("id IN ?", inactiveIDs).Delete(&models.Customer{})
		log.Printf("[PurgeInactiveCustomers] Successfully purged %d abandoned guest customer records.", res.RowsAffected)
	}

	log.Printf("[PurgeInactiveCustomers] Customer accounts validated.")
	return int64(len(inactiveIDs)), nil
}

// CustomerCleanupInactive lets an ISP's owner (super_admin) purge their own
// abandoned guest/voucher accounts — never another ISP's.
func CustomerCleanupInactive(c *fiber.Ctx) error {
	if claims := middleware.GetClaims(c); claims == nil || claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Only the account owner can clean up customers.", "Forbidden.", fiber.StatusForbidden)
	}
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	deleted, err := PurgeInactiveCustomersInZones(orgZoneIDs)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to purge inactive customers: "+err.Error(), "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, fiber.Map{
		"deleted_count": deleted,
	}, fmt.Sprintf("Successfully removed %d inactive/guest customers. Only active subscribers retained.", deleted))
}

// phoneSearchForm turns a typed local number ("0712", "+254 712") into the
// stored 254… form for LIKE searches; "" when the text isn't a phone fragment.
func phoneSearchForm(search string) string {
	t := strings.TrimSpace(search)
	if len(t) < 2 || (t[0] != '0' && t[0] != '+') {
		return ""
	}
	for _, ch := range t[1:] {
		if (ch < '0' || ch > '9') && ch != ' ' {
			return ""
		}
	}
	return utils.FormatPhone(t)
}
