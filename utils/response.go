package utils

import (
	"time"

	"github.com/gofiber/fiber/v2"
)

// SuccessResponse sends a JSON success response.
func SuccessResponse(c *fiber.Ctx, data interface{}, message string, statusCode ...int) error {
	code := fiber.StatusOK
	if len(statusCode) > 0 {
		code = statusCode[0]
	}
	return c.Status(code).JSON(fiber.Map{
		"success": true,
		"data":    data,
		"message": message,
	})
}

// ErrorResponse sends a JSON error response.
func ErrorResponse(c *fiber.Ctx, errMsg string, message string, statusCode ...int) error {
	code := fiber.StatusBadRequest
	if len(statusCode) > 0 {
		code = statusCode[0]
	}
	return c.Status(code).JSON(fiber.Map{
		"success": false,
		"error":   errMsg,
		"message": message,
	})
}

// PaginatedResponse wraps paginated data in the standard format.
func PaginatedResponse(c *fiber.Ctx, data interface{}, total int64, page, perPage int) error {
	return c.JSON(fiber.Map{
		"success": true,
		"data":    data,
		"meta": fiber.Map{
			"total":    total,
			"page":     page,
			"per_page": perPage,
		},
		"message": "",
	})
}

// ParsePage extracts pagination params from query string.
func ParsePage(c *fiber.Ctx) (page, perPage int) {
	page = c.QueryInt("page", 1)
	perPage = c.QueryInt("per_page", 15)
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 200 {
		perPage = 15
	}
	return
}

// Offset calculates the DB offset for pagination.
func Offset(page, perPage int) int {
	return (page - 1) * perPage
}

// FormatPhone normalises a Kenyan phone number to 2547XXXXXXXX format.
func FormatPhone(phone string) string {
	// Remove non-digit chars (preserve nothing special)
	digits := ""
	for _, ch := range phone {
		if ch >= '0' && ch <= '9' {
			digits += string(ch)
		}
	}

	// Handle double-254 prefix
	if len(digits) >= 6 && digits[:6] == "254254" {
		digits = digits[3:]
	}

	// Handle 2540 prefix (2540XXXXXXXX → 254XXXXXXXXX)
	if len(digits) >= 4 && digits[:4] == "2540" {
		digits = "254" + digits[4:]
	}

	// Handle leading 0
	if len(digits) > 0 && digits[0] == '0' {
		digits = "254" + digits[1:]
	}

	// Handle bare local number (9 digits starting with 7 or 1)
	if len(digits) == 9 && (digits[0] == '7' || digits[0] == '1') {
		digits = "254" + digits
	}

	return digits
}

// FormatPhoneE164 returns phone in +254... format for Africa's Talking.
func FormatPhoneE164(phone string) string {
	p := FormatPhone(phone)
	if len(p) > 0 && p[0] != '+' {
		return "+" + p
	}
	return p
}

// CalculateExpiry returns an expiry time based on billing_cycle.
func CalculateExpiry(billingCycle string, base *time.Time) time.Time {
	t := time.Now().UTC()
	if base != nil && base.After(t) {
		t = *base
	}
	switch billingCycle {
	case "hourly":
		return t.Add(time.Hour)
	case "daily":
		return t.Add(24 * time.Hour)
	case "weekly":
		return t.Add(7 * 24 * time.Hour)
	case "monthly":
		return t.AddDate(0, 1, 0)
	default:
		return t.AddDate(0, 1, 0)
	}
}
