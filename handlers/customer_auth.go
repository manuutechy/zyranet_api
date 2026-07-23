package handlers

import (
	crand "crypto/rand"
	"fmt"
	"log"
	"math/big"
	"regexp"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// otpStore is an in-memory OTP cache (phone → otp, expires).
var (
	otpStore   = sync.Map{}
	otpTimeout = 10 * time.Minute
)

type otpEntry struct {
	OTP       string
	ExpiresAt time.Time
}

var smsServiceGlobal *services.SmsService

// InitCustomerAuthSMS injects the SMS service.
func InitCustomerAuthSMS(sms *services.SmsService) {
	smsServiceGlobal = sms
}

// RequestOtp sends a 4-digit OTP to the customer's phone.
func RequestOtp(c *fiber.Ctx) error {
	var body struct {
		Phone string `json:"phone"`
		Name  string `json:"name"`
	}
	if err := c.BodyParser(&body); err != nil || body.Phone == "" {
		return utils.ErrorResponse(c, "Phone number required.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}

	phone := normalizePhone(body.Phone)

	var customer models.Customer
	if err := config.DB.Where("phone = ?", phone).First(&customer).Error; err != nil {
		// Auto-register
		var zone models.Zone
		var pkg models.Package
		config.DB.First(&zone)
		config.DB.Where("type = ?", "hotspot").First(&pkg)
		if pkg.ID == 0 {
			config.DB.First(&pkg)
		}
		if zone.ID == 0 || pkg.ID == 0 {
			return utils.ErrorResponse(c, "System not configured. Please create a Zone and Package first.", "Setup required.", fiber.StatusBadRequest)
		}
		pppoeUser := "user_" + phone[max(0, len(phone)-6):]
		
		customerName := body.Name
		if customerName == "" {
			customerName = "Customer_" + phone[max(0, len(phone)-4):]
		}
		
		customer = models.Customer{
			Name:          customerName,
			Phone:         phone,
			ZoneID:        zone.ID,
			PackageID:     pkg.ID,
			Type:          "hotspot",
			Status:        "expired",
			PPPoEUsername: &pppoeUser,
		}
		config.DB.Create(&customer)
	} else {
		// Update customer name if a new one is provided during OTP request
		if body.Name != "" && customer.Name != body.Name {
			customer.Name = body.Name
			config.DB.Save(&customer)
		}
	}

	// Generate a cryptographically random 4-digit OTP
	otp := generateOtp()
	otpStore.Store(phone, otpEntry{OTP: otp, ExpiresAt: time.Now().Add(otpTimeout)})

	log.Printf("[OTP] Phone %s OTP: %s", phone, otp)

	template := GetSetting("sms_template_otp")
	msg := utils.RenderTemplate(template, map[string]string{
		"otp": otp,
	})
	if smsServiceGlobal != nil && GetSetting("sms_enable_otp") != "no" {
		go smsServiceGlobal.Send(phone, msg) //nolint:errcheck
	}

	return utils.SuccessResponse(c, fiber.Map{
		"phone":   phone,
		"message": "OTP sent successfully. Check your SMS.",
	}, "OTP sent.")
}

// VerifyOtp validates the OTP and returns a customer JWT.
func VerifyOtp(c *fiber.Ctx) error {
	var body struct {
		Phone string `json:"phone"`
		OTP   string `json:"otp"`
		Mac   string `json:"mac"`
		IP    string `json:"ip"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request.", "", fiber.StatusBadRequest)
	}

	phone := normalizePhone(body.Phone)
	otp := body.OTP

	// Sandbox bypass — only ever active outside production, so this can
	// never be used as an account-takeover backdoor on the live system.
	sandboxPass := config.Config.AppEnv != "production" && (otp == "1234" || otp == "123456")

	if !sandboxPass {
		v, ok := otpStore.Load(phone)
		if !ok {
			return utils.ErrorResponse(c, "Invalid or expired verification code.", "", fiber.StatusBadRequest)
		}
		entry := v.(otpEntry)
		if time.Now().After(entry.ExpiresAt) || entry.OTP != otp {
			otpStore.Delete(phone)
			return utils.ErrorResponse(c, "Invalid or expired verification code.", "", fiber.StatusBadRequest)
		}
		otpStore.Delete(phone)
	}

	var customer models.Customer
	if err := config.DB.Preload("Package").Preload("Zone").Where("phone = ?", phone).First(&customer).Error; err != nil {
		// If sandbox/demo mode, auto-register them here as well so any number works with 1234/123456!
		if sandboxPass {
			var zone models.Zone
			var pkg models.Package
			config.DB.First(&zone)
			config.DB.Where("type = ?", "hotspot").First(&pkg)
			if pkg.ID == 0 {
				config.DB.First(&pkg)
			}
			if zone.ID == 0 || pkg.ID == 0 {
				return utils.ErrorResponse(c, "System not configured. Please create a Zone and Package first.", "Setup required.", fiber.StatusBadRequest)
			}
			pppoeUser := "user_" + phone[max(0, len(phone)-6):]
			customer = models.Customer{
				Name:          "DemoCustomer_" + phone[max(0, len(phone)-4):],
				Phone:         phone,
				ZoneID:        zone.ID,
				PackageID:     pkg.ID,
				Type:          "hotspot",
				Status:        "expired",
				PPPoEUsername: &pppoeUser,
			}
			if err := config.DB.Create(&customer).Error; err != nil {
				return utils.ErrorResponse(c, "Failed to auto-register demo customer.", "", fiber.StatusInternalServerError)
			}
			// Preload relations
			config.DB.Preload("Package").Preload("Zone").First(&customer, customer.ID)
		} else {
			return utils.ErrorResponse(c, "Customer profile not found.", "", fiber.StatusNotFound)
		}
	}

	// Force activation on successful OTP verification so they can access the internet immediately
	// Give them 24 hours of demo/trial access
	expiry := time.Now().Add(24 * time.Hour)
	customer.Status = "active"
	customer.ExpiresAt = &expiry
	config.DB.Save(&customer)

	// Whitelist the MAC address on the MikroTik router if MAC is provided
	if body.Mac != "" && mikrotikSvc != nil {
		log.Printf("[OTP Verify] Whitelisting MAC %s for customer %s (%s)", body.Mac, customer.Name, customer.Phone)
		err := mikrotikSvc.WhitelistMAC(customer.Zone, body.Mac, customer.Package)
		if err != nil {
			log.Printf("[OTP Verify] WhitelistMAC failed for %s: %v", body.Mac, err)
		}
	}

	token, err := middleware.GenerateCustomerToken(customer.ID)
	if err != nil {
		return utils.ErrorResponse(c, "Token generation failed.", "", fiber.StatusInternalServerError)
	}

	middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)

	return utils.SuccessResponse(c, fiber.Map{
		"token":    token,
		"customer": buildCustomerProfile(&customer),
	}, "Verification successful.")
}

// CustomerAuthGuest authenticates the customer as a guest. If this device's
// MAC address already has a guest account from a previous visit, that
// account is reused (so a returning guest keeps their credit balance /
// active subscription) instead of minting a new throwaway record every
// time. The lookup is deliberately scoped to guest accounts only
// (account_number LIKE 'ZYR#GUEST#%') — a MAC address is easy to spoof, so
// it must never be sufficient on its own to log in as a real subscriber.
func CustomerAuthGuest(c *fiber.Ctx) error {
	var body struct {
		Mac string `json:"mac"`
		IP  string `json:"ip"`
	}
	c.BodyParser(&body) // mac/ip are optional; an empty body just skips device-binding

	if body.Mac != "" {
		var existing models.Customer
		err := config.DB.
			Where("mac_address = ? AND account_number LIKE ?", body.Mac, "ZYR#GUEST#%").
			Order("created_at DESC").
			First(&existing).Error
		if err == nil {
			token, err := middleware.GenerateCustomerToken(existing.ID)
			if err != nil {
				return utils.ErrorResponse(c, "Token generation failed.", "", fiber.StatusInternalServerError)
			}
			middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)
			return utils.SuccessResponse(c, fiber.Map{
				"token":    token,
				"customer": buildCustomerProfile(&existing),
			}, "Welcome back.")
		}
	}

	// Find guest package and zone
	var zone models.Zone
	var pkg models.Package
	config.DB.First(&zone)
	config.DB.Where("type = ?", "hotspot").First(&pkg)
	if pkg.ID == 0 {
		config.DB.First(&pkg)
	}
	if zone.ID == 0 || pkg.ID == 0 {
		return utils.ErrorResponse(c, "System not configured. Please create a Zone and Package first.", "Setup required.", fiber.StatusBadRequest)
	}

	// Create a unique guest account number
	var count int64
	config.DB.Unscoped().Model(&models.Customer{}).Where("account_number LIKE ?", "ZYR#GUEST#%").Count(&count)
	guestAcc := fmt.Sprintf("ZYR#GUEST#%d", 10001+count)

	guestPhone := fmt.Sprintf("GUEST%d", 10001+count)

	guestName := fmt.Sprintf("Guest_%d", 10001+count)
	pppoeUser := "guest_" + guestPhone

	customer := models.Customer{
		Name:          guestName,
		Phone:         guestPhone,
		ZoneID:        zone.ID,
		PackageID:     pkg.ID,
		Type:          "hotspot",
		Status:        "active", // Guest gets direct active access or active status
		AccountNumber: guestAcc,
		PPPoEUsername: &pppoeUser,
	}
	if body.Mac != "" {
		customer.MacAddress = &body.Mac
	}

	// Give a 1 hour duration or let it use the package billing cycle
	expiry := time.Now().Add(time.Hour)
	customer.ExpiresAt = &expiry

	if err := config.DB.Create(&customer).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to create guest user", err.Error(), fiber.StatusInternalServerError)
	}

	token, err := middleware.GenerateCustomerToken(customer.ID)
	if err != nil {
		return utils.ErrorResponse(c, "Token generation failed.", "", fiber.StatusInternalServerError)
	}

	middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)

	return utils.SuccessResponse(c, fiber.Map{
		"token":    token,
		"customer": buildCustomerProfile(&customer),
	}, "Guest login successful.")
}

// CustomerLogout clears the customer session cookie. Public: clearing a
// cookie that may already be missing/expired is always safe.
func CustomerLogout(c *fiber.Ctx) error {
	middleware.ClearAuthCookie(c, middleware.CustomerCookieName)
	return utils.SuccessResponse(c, nil, "Logged out successfully.")
}

// CustomerProfile returns the authenticated customer's profile.
func CustomerProfile(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}

	var customer models.Customer
	if err := config.DB.Preload("Package").Preload("Zone").First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
	}

	var voucherCount int64
	config.DB.Model(&models.Voucher{}).Where("used_by = ?", customer.ID).Count(&voucherCount)

	profile := buildCustomerProfile(&customer)
	profile["vouchers_redeemed"] = voucherCount

	return utils.SuccessResponse(c, profile, "")
}

// CustomerReconnect whitelists the customer's MAC address on the zone's router.
func CustomerReconnect(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}

	var body struct {
		Mac string `json:"mac"`
	}
	if err := c.BodyParser(&body); err != nil || body.Mac == "" {
		return utils.ErrorResponse(c, "MAC address is required.", "", fiber.StatusUnprocessableEntity)
	}

	var customer models.Customer
	if err := config.DB.Preload("Package").Preload("Zone").First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
	}

	if customer.Status != "active" || customer.Package == nil || customer.Zone == nil {
		return utils.ErrorResponse(c, "No active subscription found or Zone not configured.", "", fiber.StatusBadRequest)
	}

	// Whitelist MAC address on the zone's router
	if mikrotikSvc != nil {
		err := mikrotikSvc.WhitelistMAC(customer.Zone, body.Mac, customer.Package)
		if err != nil {
			log.Printf("[Reconnect] Failed to whitelist MAC %s: %v", body.Mac, err)
			if config.Config.AppEnv != "local" {
				return utils.ErrorResponse(c, err.Error(), "Failed to authorize device on router.", fiber.StatusInternalServerError)
			}
		}
	} else {
		log.Printf("[Reconnect] Warning: mikrotikSvc is nil, skipping router whitelist in local/test environment.")
	}

	return utils.SuccessResponse(c, fiber.Map{
		"success":  true,
		"username": body.Mac,
		"password": body.Mac,
		"message":  "Device authorized successfully.",
	}, "Reconnected.")
}


// CustomerAuthPayments returns the authenticated customer's payment history.
func CustomerAuthPayments(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}

	page, perPage := utils.ParsePage(c)
	var payments []models.Payment
	var total int64

	query := config.DB.Model(&models.Payment{}).
		Preload("Package").Preload("Zone").
		Where("customer_id = ?", claims.CustomerID)

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&payments)

	return utils.PaginatedResponse(c, payments, total, page, perPage)
}

func buildCustomerProfile(c *models.Customer) fiber.Map {
	m := fiber.Map{
		"id":             c.ID,
		"name":           c.Name,
		"phone":          c.Phone,
		"type":           c.Type,
		"status":         c.Status,
		"credit_balance": c.CreditBalance,
		"expires_at":     c.ExpiresAt,
	}
	if c.Package != nil {
		speed := fmt.Sprintf("%.1fMbps / %.1fMbps",
			float64(c.Package.SpeedDownloadKbps)/1024,
			float64(c.Package.SpeedUploadKbps)/1024)
		m["active_subscription"] = fiber.Map{
			"package_name": c.Package.Name,
			"expires_at":   c.ExpiresAt,
			"speed":        speed,
			"status":       c.Status,
		}
		m["package"] = fiber.Map{
			"id":                  c.Package.ID,
			"name":                c.Package.Name,
			"price":               c.Package.Price,
			"speed_upload_kbps":   c.Package.SpeedUploadKbps,
			"speed_download_kbps": c.Package.SpeedDownloadKbps,
		}
	}
	if c.Zone != nil {
		m["zone"] = fiber.Map{"id": c.Zone.ID, "name": c.Zone.Name}
	}
	return m
}

func normalizePhone(phone string) string {
	re := regexp.MustCompile(`\D`)
	digits := re.ReplaceAllString(phone, "")
	if len(digits) > 0 && digits[0] == '0' {
		digits = "254" + digits[1:]
	}
	if len(digits) == 9 && (digits[0] == '7' || digits[0] == '1') {
		digits = "254" + digits
	}
	return digits
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func generateOtp() string {
	n, err := crand.Int(crand.Reader, big.NewInt(10000))
	if err != nil {
		// crypto/rand failure is effectively unrecoverable; fall back to a
		// timestamp-derived value rather than panicking on an OTP request.
		return fmt.Sprintf("%04d", time.Now().UnixNano()%10000)
	}
	return fmt.Sprintf("%04d", n.Int64())
}

// CustomerTopUp initiates an M-Pesa STK Push to top up credit balance.
func CustomerTopUp(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.CustomerID == 0 {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusUnauthorized)
	}

	var body struct {
		Phone  string  `json:"phone"`
		Amount float64 `json:"amount"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.Phone == "" || body.Amount <= 0 {
		return utils.ErrorResponse(c, "Phone and amount are required.", "", fiber.StatusUnprocessableEntity)
	}

	// Fetch customer
	var customer models.Customer
	if err := config.DB.First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
	}

	// Create pending payment record (PackageID is nil because it's a top-up)
	payment := models.Payment{
		CustomerID: &customer.ID,
		VoucherID:  nil,
		ZoneID:     customer.ZoneID,
		PackageID:  nil,
		Phone:      normalizePhone(body.Phone),
		Amount:     body.Amount,
		Currency:   "KES",
		Method:     "mpesa",
		Status:     "pending",
	}
	if err := config.DB.Create(&payment).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create payment record.", fiber.StatusInternalServerError)
	}

	ref := fmt.Sprintf("Cust-%d", customer.ID)
	description := fmt.Sprintf("Credit Top Up for %s", customer.Name)

	stkResp, err := mpesaSvcGlobal.InitiateSTKPush(customer.ZoneID, body.Phone, body.Amount, ref, description)
	if err != nil {
		reason := err.Error()
		config.DB.Model(&payment).Updates(map[string]interface{}{
			"status":        "failed",
			"status_reason": reason,
		})
		return utils.ErrorResponse(c, err.Error(), "M-Pesa API error.", fiber.StatusInternalServerError)
	}

	if stkResp.Status == "success" {
		config.DB.Model(&payment).Update("mpesa_transaction_id", stkResp.CheckoutRequestID)

		// Simulate callback in mock/sandbox mode
		if stkResp.IsMock {
			mpesaSvcGlobal.SimulateCallback(stkResp.CheckoutRequestID, body.Amount, body.Phone)
		}

		return utils.SuccessResponse(c, fiber.Map{
			"payment_id":     payment.ID,
			"transaction_id": stkResp.CheckoutRequestID,
			"message":        stkResp.ResponseDescription,
		}, "Top up STK Push initiated successfully.")
	}

	reason := "Failed to initiate top-up M-Pesa STK Push payment."
	config.DB.Model(&payment).Updates(map[string]interface{}{
		"status":        "failed",
		"status_reason": reason,
	})
	return utils.ErrorResponse(c, reason, "", fiber.StatusBadRequest)
}

// CustomerPurchaseWithCredit purchases a package using credit balance.
func CustomerPurchaseWithCredit(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.CustomerID == 0 {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusUnauthorized)
	}

	var body struct {
		PackageID uint   `json:"package_id"`
		Mac       string `json:"mac"`
		IP        string `json:"ip"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.PackageID == 0 {
		return utils.ErrorResponse(c, "package_id is required.", "", fiber.StatusUnprocessableEntity)
	}

	// Fetch customer
	var customer models.Customer
	if err := config.DB.First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
	}

	// Fetch package
	var pkg models.Package
	if err := config.DB.First(&pkg, body.PackageID).Error; err != nil {
		return utils.ErrorResponse(c, "Package not found.", "", fiber.StatusNotFound)
	}

	// Verify sufficient credit balance
	if customer.CreditBalance < pkg.Price {
		return utils.ErrorResponse(c, "Insufficient credit balance. Please top up your account.", "Insufficient balance", fiber.StatusBadRequest)
	}

	// Deduct credit balance
	newBalance := customer.CreditBalance - pkg.Price
	config.DB.Model(&customer).Update("credit_balance", newBalance)

	// Log credit deduction
	note := fmt.Sprintf("Purchased Package: %s", pkg.Name)
	config.DB.Create(&models.CreditLog{
		CustomerID: customer.ID,
		Amount:     pkg.Price,
		Type:       "debit",
		Note:       &note,
	})

	// Create completed payment record
	payment := models.Payment{
		CustomerID: &customer.ID,
		VoucherID:  nil,
		ZoneID:     pkg.ZoneID,
		PackageID:  &pkg.ID,
		Phone:      customer.Phone,
		Amount:     pkg.Price,
		Currency:   "KES",
		Method:     "credit", // paid via credit balance
		Status:     "completed",
		MacAddress: body.Mac,
		IpAddress:  body.IP,
	}
	if err := config.DB.Create(&payment).Error; err != nil {
		log.Printf("[Credit Purchase] Failed to record payment for Customer %d: %v", customer.ID, err)
	}

	// Activate subscription
	expiresAt := utils.CalculateExpiry(pkg.BillingCycle, customer.ExpiresAt)
	config.DB.Model(&customer).Updates(map[string]interface{}{
		"status":     "active",
		"package_id": pkg.ID,
		"zone_id":    pkg.ZoneID,
		"expires_at": expiresAt,
	})

	// Load Zone to run MikroTik commands
	var zone models.Zone
	if err := config.DB.First(&zone, pkg.ZoneID).Error; err == nil {
		if body.Mac != "" {
			go func() {
				// Whitelist on router
				if err := mpesaSvcGlobal.MikroTik.WhitelistMAC(&zone, body.Mac, &pkg); err != nil {
					log.Printf("[Credit Purchase] WhitelistMAC failed for %s: %v", body.Mac, err)
				} else {
					log.Printf("[Credit Purchase] Successfully whitelisted MAC %s on router", body.Mac)
				}
			}()
		}
	}

	// Send confirmation SMS
	templateActive := GetSetting("sms_template_active")
	msg := utils.RenderTemplate(templateActive, map[string]string{
		"name":    customer.Name,
		"package": pkg.Name,
		"expiry":  expiresAt.Format("2006-01-02 15:04"),
	})
	if GetSetting("sms_enable_active") != "no" {
		go smsSvcGlobal.Send(customer.Phone, msg) //nolint:errcheck
	}

	return utils.SuccessResponse(c, fiber.Map{
		"credit_balance": newBalance,
		"expires_at":     expiresAt,
		"message":        "Package purchased successfully using credit balance.",
	}, "Purchase successful.")
}
