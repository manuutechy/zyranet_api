package handlers

import (
	"fmt"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// TicketIndex lists all support tickets (admin).
func TicketIndex(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	page, perPage := utils.ParsePage(c)
	var tickets []models.Ticket
	var total int64

	query := config.DB.Model(&models.Ticket{}).Preload("Customer").Preload("AssignedUser").Preload("Zone")

	// Every ISP staff role — super_admin included (it is the ISP owner, not
	// platform staff) — sees only its own organization's tickets.
	query = query.Where("organization_id = ?", ticketOrgID(claims))
	query = ticketZoneScope(c, claims, query)

	if s := c.Query("status"); s != "" {
		query = query.Where("status = ?", s)
	}
	if p := c.Query("priority"); p != "" {
		query = query.Where("priority = ?", p)
	}
	if a := c.Query("assigned_to"); a != "" {
		query = query.Where("assigned_to = ?", a)
	}
	if cid := c.Query("customer_id"); cid != "" {
		query = query.Where("customer_id = ?", cid)
	}
	if search := c.Query("search"); search != "" {
		query = query.Where("name LIKE ? OR phone LIKE ? OR subject LIKE ? OR message LIKE ?",
			"%"+search+"%", "%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&tickets)
	return utils.PaginatedResponse(c, tickets, total, page, perPage)
}

// TicketStorePublic allows a captive portal visitor or guest to submit a ticket.
func TicketStorePublic(c *fiber.Ctx) error {
	var body struct {
		ZoneID        *uint  `json:"zone_id"`
		ZoneIDStr     string `json:"zone"`
		CustomerID    *uint  `json:"customer_id"`
		Name          string `json:"name"`
		Phone         string `json:"phone"`
		Subject       string `json:"subject"`
		Message       string `json:"message"`
		Priority      string `json:"priority"`
		AttachmentURL string `json:"attachment_url"`
		Source        string `json:"source"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	body.Name = strings.TrimSpace(body.Name)
	body.Phone = strings.TrimSpace(body.Phone)
	body.Subject = strings.TrimSpace(body.Subject)
	body.Message = strings.TrimSpace(body.Message)

	if body.Name == "" || body.Phone == "" || body.Subject == "" || body.Message == "" {
		return utils.ErrorResponse(c, "Name, phone, subject, and message are required.", "", fiber.StatusBadRequest)
	}

	if body.Priority == "" {
		body.Priority = "medium"
	}
	if body.Source == "" {
		body.Source = "captive_portal"
	}

	formattedPhone := utils.FormatPhone(body.Phone)

	// Resolve zone and organization
	var targetZoneID *uint
	var organizationID uint = 1 // Default fallback

	var targetZoneLookup string
	if body.ZoneID != nil {
		targetZoneLookup = fmt.Sprintf("%d", *body.ZoneID)
	} else if body.ZoneIDStr != "" {
		targetZoneLookup = body.ZoneIDStr
	}

	if zone, err := resolveHotspotZone(c, targetZoneLookup); err == nil && zone != nil {
		targetZoneID = &zone.ID
		if zone.OrganizationID > 0 {
			organizationID = zone.OrganizationID
		}
	}

	// Link customer if existing customer found with this phone
	customerID := body.CustomerID
	if customerID == nil && formattedPhone != "" {
		var cust models.Customer
		if err := config.DB.Where("phone = ?", formattedPhone).First(&cust).Error; err == nil {
			customerID = &cust.ID
		}
	}

	ticket := models.Ticket{
		OrganizationID: organizationID,
		ZoneID:         targetZoneID,
		CustomerID:     customerID,
		Name:           body.Name,
		Phone:          formattedPhone,
		Subject:        body.Subject,
		Message:        body.Message,
		AttachmentURL:  body.AttachmentURL,
		Source:         body.Source,
		Status:         "pending",
		Priority:       body.Priority,
	}

	if err := config.DB.Create(&ticket).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to submit ticket.", err.Error(), fiber.StatusInternalServerError)
	}

	return utils.SuccessResponse(c, ticket, "Ticket submitted successfully. Support team notified.", fiber.StatusCreated)
}

// TicketUploadPublic allows captive portal customers to upload a screenshot or document for their ticket.
func TicketUploadPublic(c *fiber.Ctx) error {
	file, err := c.FormFile("attachment")
	if err != nil {
		file, err = c.FormFile("image")
	}
	if err != nil {
		return utils.ErrorResponse(c, "No file found in request.", "", fiber.StatusBadRequest)
	}

	// 5MB limit
	if file.Size > 5*1024*1024 {
		return utils.ErrorResponse(c, "File size exceeds 5MB limit.", "", fiber.StatusBadRequest)
	}

	ext := strings.ToLower(filepath.Ext(file.Filename))
	allowed := map[string]bool{
		".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true, ".pdf": true,
	}
	if !allowed[ext] {
		return utils.ErrorResponse(c, "Invalid file format. Allowed: JPG, PNG, WEBP, GIF, PDF.", "", fiber.StatusBadRequest)
	}

	uploadDir := "public/uploads/tickets"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		return utils.ErrorResponse(c, "Failed to create upload directory.", "", fiber.StatusInternalServerError)
	}

	filename := fmt.Sprintf("ticket_%d_%s%s", time.Now().Unix(), randomHex(4), ext)
	dst := filepath.Join(uploadDir, filename)

	if err := c.SaveFile(file, dst); err != nil {
		return utils.ErrorResponse(c, "Failed to save file.", "", fiber.StatusInternalServerError)
	}

	path := "uploads/tickets/" + filename
	fileURL := "/" + path
	if c.BaseURL() != "" {
		fileURL = c.BaseURL() + "/" + path
	}

	return utils.SuccessResponse(c, fiber.Map{
		"path": path,
		"url":  fileURL,
		"name": file.Filename,
	}, "Attachment uploaded successfully.")
}

// TicketStatusPublic allows captive portal users to check tickets they submitted.
func TicketStatusPublic(c *fiber.Ctx) error {
	phone := c.Query("phone")
	ticketID := c.Query("ticket_id")

	if phone == "" && ticketID == "" {
		return utils.ErrorResponse(c, "Phone number or ticket ID is required.", "", fiber.StatusBadRequest)
	}

	var tickets []models.Ticket
	query := config.DB.Model(&models.Ticket{})

	if ticketID != "" {
		query = query.Where("id = ?", ticketID)
	}
	if phone != "" {
		formatted := utils.FormatPhone(phone)
		query = query.Where("phone = ? OR phone = ?", formatted, phone)
	}

	query.Order("created_at DESC").Limit(5).Find(&tickets)
	return utils.SuccessResponse(c, tickets, "")
}

// TicketStoreCustomer allows an authenticated customer to submit a ticket.
func TicketStoreCustomer(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Type != "customer" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusUnauthorized)
	}

	var customer models.Customer
	if err := config.DB.Preload("Zone").First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
	}

	var body struct {
		Subject       string `json:"subject"`
		Message       string `json:"message"`
		Priority      string `json:"priority"`
		AttachmentURL string `json:"attachment_url"`
	}
	c.BodyParser(&body)
	if body.Priority == "" {
		body.Priority = "medium"
	}

	var orgID uint = 1
	var zoneID *uint
	if customer.ZoneID > 0 {
		zoneID = &customer.ZoneID
		if customer.Zone != nil && customer.Zone.OrganizationID > 0 {
			orgID = customer.Zone.OrganizationID
		}
	}

	ticket := models.Ticket{
		OrganizationID: orgID,
		ZoneID:         zoneID,
		CustomerID:     &customer.ID,
		Name:           customer.Name,
		Phone:          customer.Phone,
		Subject:        body.Subject,
		Message:        body.Message,
		AttachmentURL:  body.AttachmentURL,
		Source:         "customer_dashboard",
		Status:         "open",
		Priority:       body.Priority,
	}
	config.DB.Create(&ticket)
	return utils.SuccessResponse(c, ticket, "Support ticket created successfully.", fiber.StatusCreated)
}

// TicketShow returns a single ticket.
func TicketShow(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var ticket models.Ticket
	query := config.DB.Preload("Customer").Preload("AssignedUser").Preload("Zone").Preload("Organization")

	query = query.Where("organization_id = ?", ticketOrgID(claims))
	query = ticketZoneScope(c, claims, query)

	if err := query.First(&ticket, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Ticket not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, ticket, "")
}

// TicketUpdate updates a ticket's status/priority/assignee/notes (admin).
func TicketUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var ticket models.Ticket
	query := config.DB.Model(&models.Ticket{})

	query = query.Where("organization_id = ?", ticketOrgID(claims))
	query = ticketZoneScope(c, claims, query)

	if err := query.First(&ticket, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Ticket not found.", "", fiber.StatusNotFound)
	}

	var raw map[string]interface{}
	if err := c.BodyParser(&raw); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	// Staff may only work the ticket: status, priority, assignee, notes.
	body := map[string]interface{}{}
	for _, k := range []string{"status", "priority", "assigned_to", "internal_notes"} {
		if v, ok := raw[k]; ok {
			body[k] = v
		}
	}
	if a, ok := body["assigned_to"].(float64); ok {
		var n int64
		config.DB.Model(&models.User{}).Where("id = ? AND organization_id = ?", uint(a), ticketOrgID(claims)).Count(&n)
		if n == 0 {
			return utils.ErrorResponse(c, "Assignee is not one of your staff.", "", fiber.StatusUnprocessableEntity)
		}
	}
	if len(body) > 0 {
		if err := config.DB.Model(&ticket).Updates(body).Error; err != nil {
			return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
		}
	}
	config.DB.Preload("Customer").Preload("AssignedUser").Preload("Zone").Preload("Organization").First(&ticket, ticket.ID)
	return utils.SuccessResponse(c, ticket, "Ticket updated successfully.")
}

// TicketCustomerList returns tickets for the authenticated customer.
func TicketCustomerList(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Type != "customer" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusUnauthorized)
	}
	var tickets []models.Ticket
	config.DB.Where("customer_id = ?", claims.CustomerID).Order("created_at DESC").Find(&tickets)
	return utils.SuccessResponse(c, tickets, "")
}

// TicketDestroy soft-deletes a ticket (super_admin or tenant admin).
func TicketDestroy(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" && claims.Role != "admin" {
		return utils.ErrorResponse(c, "Unauthorized to delete tickets.", "", fiber.StatusForbidden)
	}

	var ticket models.Ticket
	query := config.DB.Model(&models.Ticket{})
	query = query.Where("organization_id = ?", ticketOrgID(claims))

	if err := query.First(&ticket, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Ticket not found.", "", fiber.StatusNotFound)
	}

	if err := config.DB.Delete(&ticket).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Ticket deleted successfully.")
}

// ticketOrgID is the organization whose tickets the caller may see (0 = none).
func ticketOrgID(claims *middleware.Claims) uint {
	if claims == nil {
		return 0
	}
	return claims.OrganizationID
}

// ticketZoneScope limits zone-bound staff to their zone's tickets (and
// tickets with no zone) and area managers to their areas' zones — the same
// rule for the list and for opening or changing one ticket.
func ticketZoneScope(c *fiber.Ctx, claims *middleware.Claims, q *gorm.DB) *gorm.DB {
	if claims != nil && claims.ZoneID != nil {
		return q.Where("(zone_id = ? OR zone_id IS NULL)", *claims.ZoneID)
	}
	if claims != nil && claims.AreaManager {
		ids := append([]uint{0}, middleware.AreaZoneIDs(c)...)
		return q.Where("(zone_id IN ? OR zone_id IS NULL)", ids)
	}
	return q
}
