package handlers

import (
	"fmt"
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

	// Scope by organization for tenant admins
	if claims != nil && claims.Role != "super_admin" && claims.OrganizationID > 0 {
		query = query.Where("organization_id = ?", claims.OrganizationID)
	}
	if claims != nil && claims.ZoneID != nil {
		query = query.Where("zone_id = ? OR zone_id IS NULL", *claims.ZoneID)
	}

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

	if claims != nil && claims.Role != "super_admin" && claims.OrganizationID > 0 {
		query = query.Where("organization_id = ?", claims.OrganizationID)
	}

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

	if claims != nil && claims.Role != "super_admin" && claims.OrganizationID > 0 {
		query = query.Where("organization_id = ?", claims.OrganizationID)
	}

	if err := query.First(&ticket, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Ticket not found.", "", fiber.StatusNotFound)
	}

	var body map[string]interface{}
	c.BodyParser(&body)
	delete(body, "organization_id") // Immutable

	config.DB.Model(&ticket).Updates(body)
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
	if claims.Role != "super_admin" && claims.OrganizationID > 0 {
		query = query.Where("organization_id = ?", claims.OrganizationID)
	}

	if err := query.First(&ticket, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Ticket not found.", "", fiber.StatusNotFound)
	}

	if err := config.DB.Delete(&ticket).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Ticket deleted successfully.")
}
