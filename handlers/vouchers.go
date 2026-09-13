package handlers

import (
	"fmt"
	"log"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

var voucherSvcGlobal *services.VoucherService

// InitVoucherService injects the voucher service and router service.
func InitVoucherService(svc *services.VoucherService, mikrotik *services.MikroTikService) {
	voucherSvcGlobal = svc
	if mikrotik != nil {
		mikrotikSvcGlobal = mikrotik
	}
}

// VoucherIndex lists vouchers with filters.
func VoucherIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var vouchers []models.Voucher
	var total int64

	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}

	query := config.DB.Model(&models.Voucher{}).Preload("Zone").Preload("Package").Where("zone_id IN (?)", orgZoneIDs)
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

	var targetZone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&targetZone, body.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid zone for this organization.", "", fiber.StatusUnprocessableEntity)
	}

	var createdVouchers []*models.Voucher
	if body.Quantity == 1 {
		voucher, err := voucherSvcGlobal.Generate(body.ZoneID, body.PackageID, body.Type, body.UsageLimit)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Voucher generation failed.", fiber.StatusInternalServerError)
		}
		createdVouchers = append(createdVouchers, voucher)
	} else {
		// Batch generation
		for i := 0; i < body.Quantity; i++ {
			v, err := voucherSvcGlobal.Generate(body.ZoneID, body.PackageID, body.Type, body.UsageLimit)
			if err != nil {
				return utils.ErrorResponse(c, err.Error(), "Batch generation failed.", fiber.StatusInternalServerError)
			}
			createdVouchers = append(createdVouchers, v)
		}
	}

	// Push newly generated vouchers to router hotspot
	if mikrotikSvcGlobal == nil {
		log.Printf("[VoucherGenerate] WARNING: mikrotikSvcGlobal is nil")
	} else if len(createdVouchers) > 0 {
		var voucherModels []models.Voucher
		for _, v := range createdVouchers {
			var vm models.Voucher
			if err := config.DB.Preload("Package").First(&vm, v.ID).Error; err != nil {
				log.Printf("[VoucherGenerate] Preload failed for voucher %d: %v", v.ID, err)
			} else if vm.Package == nil {
				log.Printf("[VoucherGenerate] Voucher %d has nil package", v.ID)
			} else {
				voucherModels = append(voucherModels, vm)
			}
		}
		if len(voucherModels) > 0 {
			go func(z models.Zone, vm []models.Voucher) {
				count, err := mikrotikSvcGlobal.PushHotspotUsers(&z, vm)
				if err != nil {
					log.Printf("[VoucherGenerate] PushHotspotUsers to router failed: %v", err)
				} else {
					log.Printf("[VoucherGenerate] Successfully pushed %d vouchers to router at %s", count, z.RouterIP)
				}
			}(targetZone, voucherModels)
		}
	}

	if body.Quantity == 1 {
		return utils.SuccessResponse(c, createdVouchers[0], "Voucher generated successfully.", fiber.StatusCreated)
	}
	return utils.SuccessResponse(c, createdVouchers, "Vouchers generated successfully.", fiber.StatusCreated)
}

// VoucherShow returns a single voucher.
func VoucherShow(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var voucher models.Voucher
	if err := config.DB.Preload("Zone").Preload("Package").Where("zone_id IN (?)", orgZoneIDs).First(&voucher, c.Params("id")).Error; err != nil {
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
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var voucher models.Voucher
	if err := config.DB.Where("zone_id IN (?)", orgZoneIDs).First(&voucher, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Voucher not found.", "", fiber.StatusNotFound)
	}
	if err := config.DB.Delete(&models.Voucher{}, voucher.ID).Error; err != nil {
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
		Mac   string `json:"mac"`
	}
	if err := c.BodyParser(&body); err != nil || body.Code == "" {
		return utils.ErrorResponse(c, "Voucher code is required.", "", fiber.StatusUnprocessableEntity)
	}
	if body.Phone == "" {
		return utils.ErrorResponse(c, "Phone number is required.", "", fiber.StatusUnprocessableEntity)
	}
	body.Phone = utils.FormatPhone(body.Phone)

	result, err := voucherSvcGlobal.Redeem(body.Code, body.Phone)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Redemption failed.", fiber.StatusBadRequest)
	}

	cleanMac := strings.ToLower(strings.TrimSpace(body.Mac))
	cleanMac = strings.ReplaceAll(cleanMac, "-", ":")

	// Update customer name and MAC if provided
	if cust, ok := result["customer"].(models.Customer); ok {
		updates := map[string]interface{}{}
		if body.Name != "" {
			updates["name"] = body.Name
			cust.Name = body.Name
		}
		if cleanMac != "" {
			updates["mac_address"] = cleanMac
			cust.MacAddress = &cleanMac
		}
		if len(updates) > 0 {
			config.DB.Model(&models.Customer{}).Where("id = ?", cust.ID).Updates(updates)
		}
		result["customer"] = cust
	}

	redeemedPkg := result["package"].(models.Package)
	redeemedCustomer := result["customer"].(models.Customer)

	// Whitelist MAC on zone router if available
	if cleanMac != "" && mikrotikSvcGlobal != nil {
		var zone models.Zone
		if err := config.DB.First(&zone, redeemedCustomer.ZoneID).Error; err == nil {
			go func(z models.Zone, m string, p models.Package) {
				if err := mikrotikSvcGlobal.WhitelistMAC(&z, m, &p); err != nil {
					log.Printf("[VoucherRedeem] WhitelistMAC for %s failed: %v", m, err)
				} else {
					log.Printf("[VoucherRedeem] Successfully whitelisted MAC %s on router at %s", m, z.RouterIP)
				}
			}(zone, cleanMac, redeemedPkg)
		}
	}

	pkgTag := fmt.Sprintf("pkg-%d", redeemedPkg.ID)
	speed := fmt.Sprintf("%.0f Mbps", float64(redeemedPkg.SpeedDownloadKbps)/1024)

	result["username"] = pkgTag
	result["password"] = pkgTag
	result["plan_name"] = redeemedPkg.Name
	result["speed"] = speed
	result["expires_at"] = redeemedCustomer.ExpiresAt

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
		Mac  string `json:"mac"`
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

	cleanMac := strings.ToLower(strings.TrimSpace(body.Mac))
	cleanMac = strings.ReplaceAll(cleanMac, "-", ":")
	if cleanMac != "" {
		customer.MacAddress = &cleanMac
		config.DB.Model(&customer).Update("mac_address", cleanMac)
	} else if customer.MacAddress != nil {
		cleanMac = *customer.MacAddress
	}

	redeemedPkg := result["package"].(models.Package)
	redeemedCustomer := result["customer"].(models.Customer)

	// Whitelist MAC on zone router if available
	if cleanMac != "" && mikrotikSvcGlobal != nil {
		var zone models.Zone
		if err := config.DB.First(&zone, redeemedCustomer.ZoneID).Error; err == nil {
			go func(z models.Zone, m string, p models.Package) {
				if err := mikrotikSvcGlobal.WhitelistMAC(&z, m, &p); err != nil {
					log.Printf("[VoucherRedeemAuthenticated] WhitelistMAC for %s failed: %v", m, err)
				} else {
					log.Printf("[VoucherRedeemAuthenticated] Successfully whitelisted MAC %s on router at %s", m, z.RouterIP)
				}
			}(zone, cleanMac, redeemedPkg)
		}
	}

	pkgTag := fmt.Sprintf("pkg-%d", redeemedPkg.ID)
	speed := fmt.Sprintf("%.0f Mbps", float64(redeemedPkg.SpeedDownloadKbps)/1024)

	result["username"] = pkgTag
	result["password"] = pkgTag
	result["plan_name"] = redeemedPkg.Name
	result["speed"] = speed
	result["expires_at"] = redeemedCustomer.ExpiresAt

	return utils.SuccessResponse(c, result, "Voucher redeemed successfully. Internet activated.")
}
