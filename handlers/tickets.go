package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// TicketIndex lists all support tickets (admin).
func TicketIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var tickets []models.Ticket
	var total int64

	query := config.DB.Model(&models.Ticket{}).Preload("Customer").Preload("AssignedUser")
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
		query = query.Where("name LIKE ? OR phone LIKE ? OR subject LIKE ?",
			"%"+search+"%", "%"+search+"%", "%"+search+"%")
	}

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&tickets)
	return utils.PaginatedResponse(c, tickets, total, page, perPage)
}

// TicketStorePublic allows a guest or portal user to submit a ticket.
func TicketStorePublic(c *fiber.Ctx) error {
	var body struct {
		CustomerID *uint  `json:"customer_id"`
		Name       string `json:"name"`
		Phone      string `json:"phone"`
		Subject    string `json:"subject"`
		Message    string `json:"message"`
		Priority   string `json:"priority"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.Priority == "" {
		body.Priority = "medium"
	}
	ticket := models.Ticket{
		CustomerID: body.CustomerID,
		Name:       body.Name,
		Phone:      body.Phone,
		Subject:    body.Subject,
		Message:    body.Message,
		Status:     "pending",
		Priority:   body.Priority,
	}
	config.DB.Create(&ticket)
	return utils.SuccessResponse(c, ticket, "Ticket submitted successfully. Support team notified.", fiber.StatusCreated)
}

// TicketStoreCustomer allows an authenticated customer to submit a ticket.
func TicketStoreCustomer(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Type != "customer" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusUnauthorized)
	}

	var customer models.Customer
	if err := config.DB.First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
	}

	var body struct {
		Subject  string `json:"subject"`
		Message  string `json:"message"`
		Priority string `json:"priority"`
	}
	c.BodyParser(&body)
	if body.Priority == "" {
		body.Priority = "medium"
	}

	ticket := models.Ticket{
		CustomerID: &customer.ID,
		Name:       customer.Name,
		Phone:      customer.Phone,
		Subject:    body.Subject,
		Message:    body.Message,
		Status:     "open",
		Priority:   body.Priority,
	}
	config.DB.Create(&ticket)
	return utils.SuccessResponse(c, ticket, "Support ticket created successfully.", fiber.StatusCreated)
}

// TicketShow returns a single ticket.
func TicketShow(c *fiber.Ctx) error {
	var ticket models.Ticket
	if err := config.DB.Preload("Customer").Preload("AssignedUser").First(&ticket, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Ticket not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, ticket, "")
}

// TicketUpdate updates a ticket's status/priority/assignee (admin).
func TicketUpdate(c *fiber.Ctx) error {
	var ticket models.Ticket
	if err := config.DB.First(&ticket, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Ticket not found.", "", fiber.StatusNotFound)
	}
	var body map[string]interface{}
	c.BodyParser(&body)
	config.DB.Model(&ticket).Updates(body)
	config.DB.Preload("Customer").Preload("AssignedUser").First(&ticket, ticket.ID)
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

// TicketDestroy soft-deletes a ticket (super_admin only).
func TicketDestroy(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to delete tickets.", "", fiber.StatusForbidden)
	}
	if err := config.DB.Delete(&models.Ticket{}, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Ticket deleted successfully.")
}
