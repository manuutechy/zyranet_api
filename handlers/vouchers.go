package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

var voucherSvcGlobal *services.VoucherService

// InitVoucherService injects the voucher service.
func InitVoucherService(svc *services.VoucherService) {
	voucherSvcGlobal = svc
}

// VoucherIndex lists vouchers with filters.
func VoucherIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var vouchers []models.Voucher
	var total int64

	query := config.DB.Model(&models.Voucher{}).Preload("Zone").Preload("Package")
	if z := c.Query("zone_id"); z != "" {
		query = query.Where("zone_id = ?", z)
	}
	if p := c.Query("package_id"); p != "" {
		query = query.Where("package_id = ?", p)
	}
	if s := c.Query("status"); s != "" {
		query = query.Where("status = ?", s)
	}
	if search := c.Query("search"); search != "" {
		query = query.Where("code LIKE ?", "%"+search+"%")
	}

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&vouchers)
	return utils.PaginatedResponse(c, vouchers, total, page, perPage)
}

// VoucherGenerate generates a single new voucher.
func VoucherGenerate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role == "finance" {
		return utils.ErrorResponse(c, "Unauthorized to generate vouchers.", "", fiber.StatusForbidden)
	}

	var body struct {
		ZoneID     uint   `json:"zone_id"`
		PackageID  uint   `json:"package_id"`
		Type       string `json:"type"`
		UsageLimit int    `json:"usage_limit"`
		Quantity   int    `json:"quantity"`
	}
	if err := c.BodyParser(&body); err != nil || body.ZoneID == 0 || body.PackageID == 0 {
		return utils.ErrorResponse(c, "zone_id and package_id are required.", "", fiber.StatusUnprocessableEntity)
	}
	if body.Type == "" {
		body.Type = "single_use"
	}
	if body.UsageLimit == 0 {
		body.UsageLimit = 1
	}
	if body.Quantity == 0 {
		body.Quantity = 1
	}

	// Zone manager can only generate for their zone
	if claims.Role == "zone_manager" && claims.ZoneID != nil && body.ZoneID != *claims.ZoneID {
		return utils.ErrorResponse(c, "Unauthorized to generate vouchers for this zone.", "", fiber.StatusForbidden)
	}

	if body.Quantity == 1 {
		voucher, err := voucherSvcGlobal.Generate(body.ZoneID, body.PackageID, body.Type, body.UsageLimit)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Voucher generation failed.", fiber.StatusInternalServerError)
		}
		return utils.SuccessResponse(c, voucher, "Voucher generated successfully.", fiber.StatusCreated)
	}

	// Batch generation
	var vouchers []*models.Voucher
	for i := 0; i < body.Quantity; i++ {
		v, err := voucherSvcGlobal.Generate(body.ZoneID, body.PackageID, body.Type, body.UsageLimit)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Batch generation failed.", fiber.StatusInternalServerError)
		}
		vouchers = append(vouchers, v)
	}
	return utils.SuccessResponse(c, vouchers, "Vouchers generated successfully.", fiber.StatusCreated)
}

// VoucherShow returns a single voucher.
func VoucherShow(c *fiber.Ctx) error {
	var voucher models.Voucher
	if err := config.DB.Preload("Zone").Preload("Package").First(&voucher, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Voucher not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, voucher, "")
}

// VoucherDestroy deletes a voucher.
func VoucherDestroy(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" && claims.Role != "zone_manager" {
		return utils.ErrorResponse(c, "Unauthorized to delete vouchers.", "", fiber.StatusForbidden)
	}
	if err := config.DB.Delete(&models.Voucher{}, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Voucher deleted successfully.")
}

// VoucherRedeem redeems a voucher code (public endpoint).
func VoucherRedeem(c *fiber.Ctx) error {
	var body struct {
		Code  string `json:"code"`
		Phone string `json:"phone"`
		Name  string `json:"name"`
	}
	if err := c.BodyParser(&body); err != nil || body.Code == "" {
		return utils.ErrorResponse(c, "Voucher code is required.", "", fiber.StatusUnprocessableEntity)
	}
	if body.Phone == "" {
		body.Phone = utils.FormatPhone("0113297270") // fallback
	}

	result, err := voucherSvcGlobal.Redeem(body.Code, body.Phone)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Redemption failed.", fiber.StatusBadRequest)
	}

	// Update customer name if provided
	if body.Name != "" {
		if c, ok := result["customer"].(*models.Customer); ok {
			config.DB.Model(c).Update("name", body.Name)
		}
	}

	return utils.SuccessResponse(c, result, "Voucher redeemed successfully. Internet activated.")
}

// VoucherRedeemAuthenticated redeems a voucher for an authenticated customer.
func VoucherRedeemAuthenticated(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Type != "customer" {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusUnauthorized)
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := c.BodyParser(&body); err != nil || body.Code == "" {
		return utils.ErrorResponse(c, "Voucher code is required.", "", fiber.StatusUnprocessableEntity)
	}

	var customer models.Customer
	if err := config.DB.First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
	}

	result, err := voucherSvcGlobal.Redeem(body.Code, customer.Phone)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Redemption failed.", fiber.StatusBadRequest)
	}
	return utils.SuccessResponse(c, result, "Voucher redeemed successfully. Internet activated.")
}
