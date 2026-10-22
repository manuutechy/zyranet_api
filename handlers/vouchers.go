package handlers

import (
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

	// Vouchers are not pushed to the router as users any more: a voucher is
	// redeemed on the portal, which gives that device its own router login.

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
	// An agent owes for vouchers they hold: take it back as a return (which
	// reverses the ledger), never delete it from under the ledger.
	var held int64
	config.DB.Model(&models.AgentVoucher{}).Where("voucher_id = ?", voucher.ID).Count(&held)
	if held > 0 {
		return utils.ErrorResponse(c, "This voucher was issued to an agent. Return it from the agent's page instead.", "", fiber.StatusConflict)
	}
	if err := config.DB.Delete(&models.Voucher{}, voucher.ID).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Voucher deleted successfully.")
}

// resolveCustomerForVoucher picks whose account a voucher's time goes on.
// Only a proven identity counts: a customer session from OTP/payment, or the
// device token of an anonymous voucher/guest account on this device. A MAC
// or a typed phone number is not proof (both were used to land on — and get
// a full session for — someone else's account). Anyone else gets a fresh
// anonymous voucher account.
func resolveCustomerForVoucher(c *fiber.Ctx, voucher *models.Voucher, cleanMac, phone, name, deviceToken string) (*models.Customer, error) {
	var customer models.Customer

	// 1. A session proven by OTP / payment, of this voucher's ISP.
	if claims := strongCustomerClaims(c); claims != nil {
		if err := config.DB.First(&customer, claims.CustomerID).Error; err == nil && customer.ID != 0 &&
			services.CustomerBelongsToZoneISP(customer.ID, voucher.ZoneID) {
			return &customer, nil
		}
	}

	// 2. This device's anonymous account (time stacks on it), proven by its token.
	if tok := deviceTokenFrom(c, deviceToken); tok != "" {
		if cred, err := services.CredentialByDeviceToken(tok); err == nil {
			if m, ok := services.NormalizeMAC(cleanMac); ok && m == cred.MacAddress {
				if err := config.DB.First(&customer, cred.CustomerID).Error; err == nil && isAnonymousAccount(&customer) &&
					services.CustomerBelongsToZoneISP(customer.ID, voucher.ZoneID) {
					return &customer, nil
				}
			}
		}
	}

	// 3. A new anonymous voucher account.
	var count int64
	config.DB.Unscoped().Model(&models.Customer{}).Where("account_number LIKE 'ZYR#VCHR#%' OR account_number LIKE 'ZYR#GUEST#%'").Count(&count)

	guestPhone := fmt.Sprintf("VCHR%d", 10001+count)
	if p := strings.TrimSpace(phone); p != "" {
		// Keep the number for receipts only if nobody at this ISP has it yet
		// (a typed number must not attach this account to someone else's).
		formatted := utils.FormatPhone(p)
		if _, taken := services.CustomerByPhoneForZone(voucher.ZoneID, formatted); !taken {
			guestPhone = formatted
		}
	}

	accountNum := fmt.Sprintf("ZYR#VCHR#%d", 10001+count)
	custName := strings.TrimSpace(name)
	if custName == "" {
		if len(cleanMac) >= 5 {
			custName = fmt.Sprintf("Guest_%s", strings.ReplaceAll(cleanMac[len(cleanMac)-5:], ":", ""))
		} else {
			custName = fmt.Sprintf("Guest_%d", 10001+count)
		}
	}
	pppoeUser := "guest_" + guestPhone

	customer = models.Customer{
		Name:          custName,
		Phone:         guestPhone,
		ZoneID:        voucher.ZoneID,
		PackageID:     voucher.PackageID,
		Type:          "hotspot",
		Status:        "active",
		AccountNumber: accountNum,
		PPPoEUsername: &pppoeUser,
	}
	if cleanMac != "" {
		customer.MacAddress = &cleanMac
	}

	if err := config.DB.Create(&customer).Error; err != nil {
		log.Printf("[Voucher] Failed to auto-create customer for voucher %s: %v", voucher.Code, err)
		return nil, err
	}

	return &customer, nil
}

// VoucherRedeem redeems a voucher code (public endpoint).
func VoucherRedeem(c *fiber.Ctx) error {
	var body struct {
		Code        string `json:"code"`
		Phone       string `json:"phone"`
		Name        string `json:"name"`
		Mac         string `json:"mac"`
		ZoneID      string `json:"zone_id"`
		DeviceToken string `json:"device_token"`
	}
	if err := c.BodyParser(&body); err != nil || body.Code == "" {
		return utils.ErrorResponse(c, "Voucher code is required.", "", fiber.StatusUnprocessableEntity)
	}

	return processVoucherRedemption(c, body.Code, body.Mac, body.Phone, body.Name, body.ZoneID, body.DeviceToken)
}

// VoucherRedeemAuthenticated redeems a voucher (authenticated or unauthenticated guest).
func VoucherRedeemAuthenticated(c *fiber.Ctx) error {
	var body struct {
		Code        string `json:"code"`
		Mac         string `json:"mac"`
		Phone       string `json:"phone"`
		Name        string `json:"name"`
		ZoneID      string `json:"zone_id"`
		DeviceToken string `json:"device_token"`
	}
	if err := c.BodyParser(&body); err != nil || body.Code == "" {
		return utils.ErrorResponse(c, "Voucher code is required.", "", fiber.StatusUnprocessableEntity)
	}

	return processVoucherRedemption(c, body.Code, body.Mac, body.Phone, body.Name, body.ZoneID, body.DeviceToken)
}

func processVoucherRedemption(c *fiber.Ctx, codeInput, macInput, phoneInput, nameInput, zoneInput, deviceToken string) error {
	cleanCode := strings.ToUpper(strings.TrimSpace(codeInput))
	cleanMac := strings.ToLower(strings.TrimSpace(macInput))
	cleanMac = strings.ReplaceAll(cleanMac, "-", ":")

	// 1. Verify voucher existence
	var voucher models.Voucher
	if err := config.DB.Preload("Package").Where("code = ?", cleanCode).First(&voucher).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid voucher ticket code.", "Voucher not found.", fiber.StatusBadRequest)
	}
	if voucher.Status == "expired" || voucher.Status == "depleted" || voucher.Status == services.VoucherStatusVoid {
		return utils.ErrorResponse(c, "This voucher ticket has already been used or expired.", "", fiber.StatusBadRequest)
	}
	if voucher.ExpiresAt != nil && time.Now().UTC().After(*voucher.ExpiresAt) {
		return utils.ErrorResponse(c, "This voucher ticket has expired.", "", fiber.StatusBadRequest)
	}
	// A voucher only works on its own ISP's WiFi.
	zone := zoneByID(zoneInput)
	if zone != nil {
		if org, ok := services.ZoneOrganization(voucher.ZoneID); !ok || org != zone.OrganizationID {
			return utils.ErrorResponse(c, "Invalid voucher ticket code.", "Voucher not found.", fiber.StatusBadRequest)
		}
	}

	// 2. Resolve or auto-create customer
	customer, err := resolveCustomerForVoucher(c, &voucher, cleanMac, phoneInput, nameInput, deviceToken)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to initialize customer session.", "", fiber.StatusInternalServerError)
	}

	// 3. Redeem voucher via VoucherService
	result, err := voucherSvcGlobal.Redeem(cleanCode, customer.Phone)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Redemption failed.", fiber.StatusBadRequest)
	}

	// Update customer name and MAC if provided
	updates := map[string]interface{}{}
	if nameInput != "" && customer.Name != nameInput && !isRealCustomerName(customer.Name) {
		updates["name"] = nameInput
		customer.Name = nameInput
	}
	if cleanMac != "" && (customer.MacAddress == nil || *customer.MacAddress != cleanMac) {
		updates["mac_address"] = cleanMac
		customer.MacAddress = &cleanMac
	}
	if len(updates) > 0 {
		config.DB.Model(&models.Customer{}).Where("id = ?", customer.ID).Updates(updates)
	}

	// 4. Issue a valid session token and cookie so client is automatically
	// authenticated (holding the voucher code is the proof).
	token, _ := middleware.GenerateCustomerToken(customer.ID)
	if token != "" {
		middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)
		result["token"] = token
	}

	redeemedPkg := result["package"].(models.Package)
	redeemedCustomer := result["customer"].(models.Customer)

	// 5. This device's own router login (never a shared pkg-<id> user, never
	// the voucher code itself as a router user).
	if cleanMac != "" {
		if zone == nil {
			zone = zoneByID(fmt.Sprint(redeemedCustomer.ZoneID))
		}
		if rl, _, err := grantRouterLogin(redeemedCustomer.ID, cleanMac, zone, "voucher", true); err == nil {
			result["router_login"] = rl
		} else {
			log.Printf("[VoucherRedeem] no router login for customer %d on %s: %v", redeemedCustomer.ID, cleanMac, err)
		}
	}

	speed := fmt.Sprintf("%.0f Mbps", float64(redeemedPkg.SpeedDownloadKbps)/1024)
	result["plan_name"] = redeemedPkg.Name
	result["speed"] = speed
	result["expires_at"] = redeemedCustomer.ExpiresAt
	result["customer"] = buildCustomerProfile(customer)

	return utils.SuccessResponse(c, result, "Voucher redeemed successfully. Internet activated.")
}
