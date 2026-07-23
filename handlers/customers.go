package handlers

import (
	crand "crypto/rand"
	"encoding/hex"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// CustomerIndex lists customers with filters.
func CustomerIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var customers []models.Customer
	var total int64

	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}

	query := config.DB.Model(&models.Customer{}).Preload("Zone").Preload("Package").Where("zone_id IN (?)", orgZoneIDs)
	claims := middleware.GetClaims(c)
	if claims.Role == "zone_manager" && claims.ZoneID != nil {
		query = query.Where("zone_id = ?", *claims.ZoneID)
	} else if z := c.Query("zone_id"); z != "" {
		query = query.Where("zone_id = ?", z)
	}
	if t := c.Query("type"); t != "" {
		query = query.Where("type = ?", t)
	}
	if s := c.Query("status"); s != "" {
		query = query.Where("status = ?", s)
	}
	if search := c.Query("search"); search != "" {
		query = query.Where("name LIKE ? OR phone LIKE ? OR pppoe_username LIKE ?",
			"%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&customers)
	return utils.PaginatedResponse(c, customers, total, page, perPage)
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
	var body map[string]interface{}
	c.BodyParser(&body)
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
	config.DB.Model(customer).Update("status", "suspended")
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
	config.DB.Model(customer).Update("status", "active")
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

// CustomerAddCredit adjusts a customer's credit balance.
func CustomerAddCredit(c *fiber.Ctx) error {
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
	if body.Type == "debit" && customer.CreditBalance < body.Amount {
		return utils.ErrorResponse(c, "Insufficient credit balance for debit.", "", fiber.StatusUnprocessableEntity)
	}

	if body.Type == "credit" {
		customer.CreditBalance += body.Amount
	} else {
		customer.CreditBalance -= body.Amount
	}
	config.DB.Save(customer)

	claims := middleware.GetClaims(c)
	note := body.Note
	config.DB.Create(&models.CreditLog{
		CustomerID: customer.ID,
		Amount:     body.Amount,
		Type:       body.Type,
		Note:       &note,
		AddedBy:    &claims.UserID,
	})

	return utils.SuccessResponse(c, fiber.Map{
		"credit_balance": customer.CreditBalance,
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
