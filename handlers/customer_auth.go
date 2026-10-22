package handlers

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
)

// otpStore is an in-memory OTP cache (phone → otp, expires).
var (
	otpStore   = sync.Map{}
	otpTimeout = 10 * time.Minute

	// otpRequestStore tracks the last time an OTP was requested for a given
	// phone (phone → time.Time), so we can enforce a per-phone cooldown.
	// The existing payLimiter on /customer/auth/otp is IP-keyed only, so a
	// distributed attacker (many source IPs) could still OTP-bomb a single
	// victim phone number by rotating IPs; this closes that gap regardless
	// of caller IP.
	otpRequestStore = sync.Map{}
	otpCooldown     = 60 * time.Second
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

	// Per-phone cooldown, independent of the caller's IP (see
	// otpRequestStore doc comment above).
	if v, ok := otpRequestStore.Load(phone); ok {
		if lastReq, ok := v.(time.Time); ok && time.Since(lastReq) < otpCooldown {
			wait := otpCooldown - time.Since(lastReq)
			return utils.ErrorResponse(c, fmt.Sprintf("Please wait %d seconds before requesting another code.", int(wait.Seconds())+1), "", fiber.StatusTooManyRequests)
		}
	}
	otpRequestStore.Store(phone, time.Now())

	var customer models.Customer
	cleanName := strings.TrimSpace(body.Name)
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

		customerName := cleanName
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
		// Set name only once: If customer has a placeholder or empty name, set their real name.
		// Once set to a real name, it cannot be edited or overwritten from the captive portal.
		if !isRealCustomerName(customer.Name) && isRealCustomerName(cleanName) {
			customer.Name = cleanName
			config.DB.Model(&customer).Update("name", cleanName)
		}
	}

	// Generate a cryptographically random 4-digit OTP
	otp := generateOtp()
	expiresAt := time.Now().Add(otpTimeout)
	otpStore.Store(phone, otpEntry{OTP: otp, ExpiresAt: expiresAt})

	// Persist OTP in database so it survives server restarts, deployments, and multi-process workers
	var otpRecord models.OtpCode
	if err := config.DB.Where("phone = ?", phone).First(&otpRecord).Error; err == nil {
		otpRecord.OTP = otp
		otpRecord.ExpiresAt = expiresAt
		otpRecord.Attempts = 0
		config.DB.Save(&otpRecord)
	} else {
		config.DB.Create(&models.OtpCode{
			Phone:     phone,
			OTP:       otp,
			ExpiresAt: expiresAt,
			Attempts:  0,
		})
	}

	template := GetSetting("sms_template_otp")
	msg := utils.RenderTemplate(template, map[string]string{
		"otp": otp,
	})
	if smsServiceGlobal != nil && GetSetting("sms_enable_otp") != "no" {
		go smsServiceGlobal.SendForZone(customer.ZoneID, phone, msg) //nolint:errcheck
	}

	return utils.SuccessResponse(c, fiber.Map{
		"phone":   phone,
		"message": "OTP sent successfully. Check your SMS.",
	}, "OTP sent.")
}

// VerifyOtp validates the OTP and returns a customer JWT.
func VerifyOtp(c *fiber.Ctx) error {
	var body struct {
		Phone       string `json:"phone"`
		OTP         string `json:"otp"`
		Mac         string `json:"mac"`
		IP          string `json:"ip"`
		ZoneID      string `json:"zone_id"`
		DeviceToken string `json:"device_token"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request.", "", fiber.StatusBadRequest)
	}

	phone := normalizePhone(body.Phone)
	otp := strings.TrimSpace(body.OTP)

	if phone == "" {
		return utils.ErrorResponse(c, "Phone number is required.", "", fiber.StatusBadRequest)
	}
	if otp == "" {
		return utils.ErrorResponse(c, "Verification code is required.", "", fiber.StatusBadRequest)
	}

	// Sandbox bypass — gated on an explicit opt-in (APP_ENV=local, or
	// ALLOW_SANDBOX_OTP=true for other non-production setups like CI/QA),
	// not merely `AppEnv != "production"`.
	sandboxOtpAllowed := config.Config.AppEnv == "local" || strings.EqualFold(os.Getenv("ALLOW_SANDBOX_OTP"), "true")
	sandboxPass := sandboxOtpAllowed && (otp == "1234" || otp == "123456")

	if !sandboxPass {
		var targetOtp string
		var expiresAt time.Time
		var attempts int
		found := false

		// 1. Check in-memory store
		if v, ok := otpStore.Load(phone); ok {
			entry := v.(otpEntry)
			targetOtp = entry.OTP
			expiresAt = entry.ExpiresAt
			found = true
		} else {
			// 2. Fallback to database store (survives deployments and server restarts)
			var dbRecord models.OtpCode
			if err := config.DB.Where("phone = ?", phone).First(&dbRecord).Error; err == nil {
				targetOtp = dbRecord.OTP
				expiresAt = dbRecord.ExpiresAt
				attempts = dbRecord.Attempts
				found = true
				otpStore.Store(phone, otpEntry{OTP: targetOtp, ExpiresAt: expiresAt})
			}
		}

		if !found {
			return utils.ErrorResponse(c, "No active verification code found for this number. Please request a new code.", "", fiber.StatusBadRequest)
		}

		if time.Now().After(expiresAt) {
			otpStore.Delete(phone)
			config.DB.Where("phone = ?", phone).Delete(&models.OtpCode{})
			return utils.ErrorResponse(c, "Verification code has expired. Please request a new code.", "", fiber.StatusBadRequest)
		}

		if attempts >= 5 {
			otpStore.Delete(phone)
			config.DB.Where("phone = ?", phone).Delete(&models.OtpCode{})
			return utils.ErrorResponse(c, "Too many incorrect attempts. Please request a new code.", "", fiber.StatusTooManyRequests)
		}

		if targetOtp != otp {
			config.DB.Model(&models.OtpCode{}).Where("phone = ?", phone).Update("attempts", attempts+1)
			return utils.ErrorResponse(c, "Incorrect verification code. Please check your SMS and try again.", "", fiber.StatusBadRequest)
		}

		// Success: clear used OTP
		otpStore.Delete(phone)
		config.DB.Where("phone = ?", phone).Delete(&models.OtpCode{})
	}

	var customer models.Customer
	if err := config.DB.Preload("Package").Preload("Zone").Where("phone = ?", phone).First(&customer).Error; err != nil {
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
			Name:          "Customer_" + phone[max(0, len(phone)-4):],
			Phone:         phone,
			ZoneID:        zone.ID,
			PackageID:     pkg.ID,
			Type:          "hotspot",
			Status:        "expired",
			PPPoEUsername: &pppoeUser,
		}
		if err := config.DB.Create(&customer).Error; err != nil {
			return utils.ErrorResponse(c, "Failed to register customer profile.", "", fiber.StatusInternalServerError)
		}
		// Preload relations
		config.DB.Preload("Package").Preload("Zone").First(&customer, customer.ID)
	}

	// Only mark active if customer has a valid unexpired subscription
	if customer.ExpiresAt != nil && customer.ExpiresAt.After(time.Now()) {
		customer.Status = "active"
	} else {
		customer.Status = "expired"
		customer.ExpiresAt = nil
	}

	// Moving a plan bought anonymously on this device (guest / voucher / free
	// trial account) onto the phone account needs proof the caller IS that
	// device — its device token. A MAC alone used to be enough, so anyone
	// could spoof a paying guest's MAC, verify their own phone and take the
	// plan.
	if (customer.Status != "active" || customer.ExpiresAt == nil) && body.Mac != "" {
		if tok := deviceTokenFrom(c, body.DeviceToken); tok != "" {
			if cred, err := services.CredentialByDeviceToken(tok); err == nil && cred.CustomerID != customer.ID {
				if m, ok := services.NormalizeMAC(body.Mac); ok && m == cred.MacAddress {
					migrateAnonymousPlan(&customer, cred.CustomerID)
				}
			}
		}
	}
	config.DB.Save(&customer)

	// Remember the device on the account.
	if body.Mac != "" {
		var dev models.CustomerDevice
		if err := config.DB.Where("customer_id = ? AND mac_address = ?", customer.ID, body.Mac).First(&dev).Error; err != nil {
			dev = models.CustomerDevice{
				CustomerID: customer.ID,
				MacAddress: body.Mac,
				LastSeenAt: time.Now(),
			}
			if body.IP != "" {
				dev.IPAddress = &body.IP
			}
			config.DB.Create(&dev)
		} else {
			dev.LastSeenAt = time.Now()
			if body.IP != "" {
				dev.IPAddress = &body.IP
			}
			config.DB.Save(&dev)
		}
		// OTP-proven: this is the customer's current device.
		config.DB.Model(&models.Customer{}).Where("id = ?", customer.ID).Update("mac_address", body.Mac)
		customer.MacAddress = &body.Mac
	}

	token, err := middleware.GenerateCustomerToken(customer.ID)
	if err != nil {
		return utils.ErrorResponse(c, "Token generation failed.", "", fiber.StatusInternalServerError)
	}

	middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)

	resp := fiber.Map{
		"token":    token,
		"customer": buildCustomerProfile(&customer),
	}
	// OTP proves the phone: a paid-up customer's plan moves onto this device
	// (e.g. the phone randomised its MAC address).
	if body.Mac != "" && customer.Status == "active" {
		if rl, _, err := grantRouterLogin(customer.ID, body.Mac, zoneByID(body.ZoneID), "", true); err == nil {
			resp["router_login"] = rl
		} else {
			log.Printf("[OTP Verify] no router login for customer %d on %s: %v", customer.ID, body.Mac, err)
		}
	}
	return utils.SuccessResponse(c, resp, "Verification successful.")
}

// isAnonymousAccount: guest / voucher / free-trial accounts with no real phone.
func isAnonymousAccount(c *models.Customer) bool {
	for _, p := range []string{"ZYR#GUEST#", "ZYR#VCHR#", "ZYR#FREE#"} {
		if strings.HasPrefix(c.AccountNumber, p) {
			return true
		}
	}
	for _, p := range []string{"GUEST", "VCHR", "FREE"} {
		if strings.HasPrefix(c.Phone, p) {
			return true
		}
	}
	return false
}

// migrateAnonymousPlan moves the active plan of the anonymous account fromID
// onto customer (same ISP only); the anonymous account's plan ends, so the
// time is moved, not copied.
func migrateAnonymousPlan(customer *models.Customer, fromID uint) {
	var prior models.Customer
	if err := config.DB.Preload("Package").Preload("Zone").First(&prior, fromID).Error; err != nil {
		return
	}
	now := time.Now()
	if !isAnonymousAccount(&prior) || prior.Status != "active" || prior.ExpiresAt == nil || !prior.ExpiresAt.After(now) {
		return
	}
	if !services.CustomerBelongsToZoneISP(customer.ID, prior.ZoneID) {
		return
	}
	res := config.DB.Model(&models.Customer{}).Where("id = ? AND status = ?", prior.ID, "active").
		Updates(map[string]interface{}{"status": "expired", "expires_at": now})
	if res.RowsAffected != 1 {
		return
	}
	customer.Status = "active"
	customer.ExpiresAt = prior.ExpiresAt
	customer.PackageID = prior.PackageID
	customer.Package = prior.Package
	if customer.ZoneID == 0 {
		customer.ZoneID = prior.ZoneID
		customer.Zone = prior.Zone
	}
	services.ReconcileCustomer(prior.ID)
	log.Printf("[OTP Verify] Moved active plan from anonymous account #%d to phone customer #%d (device token proof, expires %v)", prior.ID, customer.ID, prior.ExpiresAt)
}

// CustomerAuthByDevice recognises a returning device. Proof is the device
// token the portal stored when the device paid / proved itself; a MAC
// address alone is not enough any more (it is visible to everyone on the
// WiFi and trivial to spoof).
func CustomerAuthByDevice(c *fiber.Ctx) error {
	mac := c.Query("mac")
	deviceToken := strings.TrimSpace(c.Get("X-Device-Token"))
	zoneID := c.Query("zone_id", c.Query("zone"))
	if mac == "" || deviceToken == "" {
		var body struct {
			Mac         string `json:"mac"`
			IP          string `json:"ip"`
			DeviceToken string `json:"device_token"`
			ZoneID      string `json:"zone_id"`
		}
		if err := c.BodyParser(&body); err == nil {
			if mac == "" {
				mac = body.Mac
			}
			if deviceToken == "" {
				deviceToken = body.DeviceToken
			}
			if zoneID == "" {
				zoneID = body.ZoneID
			}
		}
	}
	mac = strings.TrimSpace(mac)
	if mac == "" {
		return utils.ErrorResponse(c, "MAC address is required.", "", fiber.StatusBadRequest)
	}
	cleanMac, ok := services.NormalizeMAC(mac)
	if !ok {
		return utils.ErrorResponse(c, "MAC address is required.", "", fiber.StatusBadRequest)
	}

	cred, err := services.CredentialByDeviceToken(deviceTokenFrom(c, deviceToken))
	if err != nil {
		return utils.SuccessResponse(c, fiber.Map{
			"found":          false,
			"authenticated":  false,
			"proof_required": true,
		}, "Device not recognized.")
	}
	zone := zoneByID(zoneID)
	if zone != nil && zone.OrganizationID != cred.OrganizationID {
		return utils.SuccessResponse(c, fiber.Map{"found": false, "authenticated": false, "proof_required": true}, "Device not recognized.")
	}

	var customer models.Customer
	if err := config.DB.Preload("Package").Preload("Zone").First(&customer, cred.CustomerID).Error; err != nil {
		return utils.SuccessResponse(c, fiber.Map{"found": false, "authenticated": false, "proof_required": true}, "Device not recognized.")
	}
	now := time.Now()
	isActive := customer.Status == "active" && customer.ExpiresAt != nil && customer.ExpiresAt.After(now)
	if !isActive {
		return utils.SuccessResponse(c, fiber.Map{
			"found":         true,
			"authenticated": false,
			"is_active":     false,
		}, "Device has no active subscription.")
	}

	// Recognised by its device token: a device session (it can't change
	// contact details or spend credit — see RequireStrongCustomerAuth).
	token, err := middleware.GenerateDeviceCustomerToken(customer.ID)
	if err != nil {
		return utils.ErrorResponse(c, "Token generation failed.", "", fiber.StatusInternalServerError)
	}
	middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)

	// Same phone with a new MAC: the token proves it, so the plan follows.
	var granted *models.HotspotCredential
	if cred.MacAddress != cleanMac {
		target := zone
		if target == nil {
			target = loginZone(nil, cred)
		}
		if target == nil {
			return utils.SuccessResponse(c, fiber.Map{"found": false, "authenticated": false, "proof_required": true}, "Device not recognized.")
		}
		granted, err = services.MoveCredential(customer.ID, target.ID, cleanMac, cred.ID)
	} else {
		granted, err = services.EnsureCredential(customer.ID, cleanMac, "")
	}
	if err != nil {
		return utils.SuccessResponse(c, fiber.Map{"found": true, "authenticated": false, "is_active": false}, "Device has no active subscription.")
	}
	rl, err := services.IssueRouterLogin(granted, granted.ID != cred.ID)
	if err != nil {
		return utils.ErrorResponse(c, "Could not prepare your login.", "", fiber.StatusInternalServerError)
	}
	pushLoginAsync(loginZone(zone, granted), granted)

	return utils.SuccessResponse(c, fiber.Map{
		"found":         true,
		"authenticated": true,
		"is_active":     true,
		"router_login":  rl,
		"token":         token,
		"customer":      buildCustomerProfile(&customer),
	}, "Device recognized.")
}

// CustomerAuthGuest creates a fresh guest account for browsing plans. An
// existing account is never handed out because a device's MAC matches it: a
// MAC is visible to everyone on the WiFi and trivial to spoof, so it gets no
// session for anyone else's (or its own earlier) account. A returning device
// proves itself with its device token (CustomerAuthByDevice), OTP, or its
// M-Pesa code + phone instead.
func CustomerAuthGuest(c *fiber.Ctx) error {
	var body struct {
		Mac string `json:"mac"`
		IP  string `json:"ip"`
	}
	c.BodyParser(&body) // mac/ip are optional; an empty body just skips device-binding

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
		Status:        "expired", // Unpaid guests start as expired until they purchase a plan or claim free trial
		AccountNumber: guestAcc,
		PPPoEUsername: &pppoeUser,
	}
	if body.Mac != "" {
		customer.MacAddress = &body.Mac
	}

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

// CustomerProfileUpdate allows a customer to update their display name.
func CustomerProfileUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return utils.ErrorResponse(c, "Unauthenticated.", "", fiber.StatusUnauthorized)
	}

	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request.", "", fiber.StatusBadRequest)
	}

	var customer models.Customer
	if err := config.DB.First(&customer, claims.CustomerID).Error; err != nil {
		return utils.ErrorResponse(c, "Customer profile not found.", "", fiber.StatusNotFound)
	}

	trimmedName := strings.TrimSpace(body.Name)
	if trimmedName == "" || len(trimmedName) < 2 {
		return utils.ErrorResponse(c, "Please provide your full name (minimum 2 characters).", "", fiber.StatusUnprocessableEntity)
	}
	if isRealCustomerName(customer.Name) {
		return utils.ErrorResponse(c, "Your name is permanently set and cannot be modified from the captive portal.", "", fiber.StatusForbidden)
	}
	customer.Name = trimmedName
	config.DB.Model(&customer).Update("name", trimmedName)

	return utils.SuccessResponse(c, buildCustomerProfile(&customer), "Name saved successfully.")
}

// CustomerReconnect gets a paid-up device back online. It needs proof: the
// device token, or a customer session proven by OTP/payment. A MAC address
// alone is not proof any more. If the customer's phone shows up with a new
// MAC (randomised), the plan moves to it.
func CustomerReconnect(c *fiber.Ctx) error {
	var body struct {
		Mac         string `json:"mac"`
		ZoneID      string `json:"zone_id"`
		DeviceToken string `json:"device_token"`
	}
	_ = c.BodyParser(&body)
	if body.Mac == "" {
		body.Mac = c.Query("mac")
	}
	if body.ZoneID == "" {
		body.ZoneID = c.Query("zone_id")
	}
	cleanMac, ok := services.NormalizeMAC(body.Mac)
	if !ok {
		return utils.ErrorResponse(c, "MAC address is required.", "", fiber.StatusBadRequest)
	}
	zone := zoneByID(body.ZoneID)
	now := time.Now()

	var customerID uint
	var fromCred uint
	if tok := deviceTokenFrom(c, body.DeviceToken); tok != "" {
		if cred, err := services.CredentialByDeviceToken(tok); err == nil {
			customerID = cred.CustomerID
			if cred.MacAddress != cleanMac {
				fromCred = cred.ID
			}
			if zone == nil {
				zone = loginZone(nil, cred)
			}
		}
	}
	if customerID == 0 {
		// Only a session proven by OTP / payment counts. A device (weak)
		// session can't mint router logins: it may have come from a MAC.
		if cl := strongCustomerClaims(c); cl != nil {
			customerID = cl.CustomerID
		}
	}
	if customerID == 0 {
		return routerLoginError(c, fiber.StatusUnauthorized, "proof_required", "Please sign back in with your phone number or your M-Pesa code.")
	}

	var customer models.Customer
	if err := config.DB.Preload("Package").Preload("Zone").First(&customer, customerID).Error; err != nil {
		return utils.ErrorResponse(c, "No active subscription found for this device. Please purchase a package to connect.", "No Active Subscription", fiber.StatusNotFound)
	}
	if customer.ExpiresAt == nil || !customer.ExpiresAt.After(now) || customer.Status != "active" {
		if customer.ExpiresAt != nil && customer.ExpiresAt.Before(now) {
			return routerLoginError(c, fiber.StatusPaymentRequired, "expired", fmt.Sprintf("Your package expired at %s. Please purchase a new package to continue.", utils.Kenya(*customer.ExpiresAt).Format("15:04")))
		}
		return routerLoginError(c, fiber.StatusNotFound, "no_plan", "No active subscription found for this device. Please purchase a package to connect.")
	}
	if zone == nil {
		zone = customer.Zone
	}
	if zone == nil {
		return utils.ErrorResponse(c, "Open this page from the WiFi login screen to reconnect.", "", fiber.StatusBadRequest)
	}

	cred, err := services.MoveCredential(customer.ID, zone.ID, cleanMac, fromCred)
	if err != nil {
		if errors.Is(err, services.ErrWrongISP) {
			return routerLoginError(c, fiber.StatusForbidden, "wrong_network", "Your plan is for a different WiFi network.")
		}
		return routerLoginError(c, fiber.StatusPaymentRequired, "expired", "Your plan has ended. Pick a plan to continue.")
	}
	rl, err := services.IssueRouterLogin(cred, true)
	if err != nil {
		return utils.ErrorResponse(c, "Could not prepare your login.", "", fiber.StatusInternalServerError)
	}
	pushLoginAsync(zone, cred)

	// Remember the device on the account.
	var dev models.CustomerDevice
	if err := config.DB.Where("customer_id = ? AND mac_address = ?", customer.ID, cleanMac).First(&dev).Error; err != nil {
		config.DB.Create(&models.CustomerDevice{CustomerID: customer.ID, MacAddress: cleanMac, LastSeenAt: now})
	} else {
		dev.LastSeenAt = now
		config.DB.Save(&dev)
	}

	speed := "5 Mbps"
	planName := "Internet Package"
	if customer.Package != nil {
		planName = customer.Package.Name
		speed = fmt.Sprintf("%.0f Mbps", float64(customer.Package.SpeedDownloadKbps)/1024)
	}

	return utils.SuccessResponse(c, fiber.Map{
		"success":      true,
		"router_login": rl,
		"plan_name":    planName,
		"speed":        speed,
		"expires_at":   customer.ExpiresAt,
		"customer":     buildCustomerProfile(&customer),
		"message":      "Device authorized successfully.",
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
	displayName := c.Name
	isPlaceholder := !isRealCustomerName(displayName)
	if isPlaceholder {
		if c.Phone != "" && !strings.HasPrefix(c.Phone, "GUEST") {
			formatted := utils.FormatPhone(c.Phone)
			if len(formatted) == 12 && strings.HasPrefix(formatted, "254") {
				displayName = "0" + formatted[3:]
			} else {
				displayName = c.Phone
			}
		} else {
			displayName = ""
		}
	}

	m := fiber.Map{
		"id":             c.ID,
		"name":           displayName,
		"name_required":  isPlaceholder,
		"phone":          c.Phone,
		"account_number": c.AccountNumber,
		"mac_address":    c.MacAddress,
		"type":           c.Type,
		"status":         c.Status,
		"credit_balance": c.CreditBalance,
		"expires_at":     c.ExpiresAt,
	}

	isActive := c.Status == "active"
	if c.ExpiresAt != nil && c.ExpiresAt.Before(time.Now()) {
		isActive = false
		if c.Status == "active" {
			c.Status = "expired"
			config.DB.Model(c).Update("status", "expired")
			m["status"] = "expired"
		}
	}

	if c.Package != nil {
		if isActive {
			speed := fmt.Sprintf("%.1fMbps / %.1fMbps",
				float64(c.Package.SpeedDownloadKbps)/1024,
				float64(c.Package.SpeedUploadKbps)/1024)
			m["active_subscription"] = fiber.Map{
				"package_name": c.Package.Name,
				"expires_at":   c.ExpiresAt,
				"speed":        speed,
				"status":       c.Status,
			}
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

func isRealCustomerName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(trimmed) < 2 {
		return false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "customer_") ||
		strings.HasPrefix(lower, "guest") ||
		strings.HasPrefix(lower, "user_") ||
		lower == "client" ||
		lower == "guest user" ||
		lower == "guest" {
		return false
	}
	// Check if name is purely digits/phone format
	matched, _ := regexp.MatchString(`^\+?[0-9\s-]+$`, trimmed)
	if matched && len(trimmed) >= 9 {
		return false
	}
	return true
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

	// Credit only buys this ISP's plans.
	if !services.CustomerBelongsToZoneISP(customer.ID, pkg.ZoneID) || pkg.Status != "active" {
		return utils.ErrorResponse(c, "Package not found.", "", fiber.StatusNotFound)
	}
	// Don't sell time on WiFi that can't connect anyone right now.
	if ok, msg := services.ZoneAcceptingPayments(pkg.ZoneID); !ok {
		return zoneOfflineResponse(c, msg)
	}

	// Verify sufficient credit balance
	if customer.CreditBalance < pkg.Price {
		return utils.ErrorResponse(c, "Insufficient credit balance. Please top up your account.", "Insufficient balance", fiber.StatusBadRequest)
	}

	// Deduct credit balance, log the deduction, record the payment, and
	// activate the subscription as a single DB transaction. Previously
	// these were separate writes with the payment-record error only
	// logged — if that Create failed, the customer was left charged
	// (balance deducted, package activated) with no payment record at all.
	// Wrapping them means any failure rolls back the whole operation
	// instead of leaving a paid-but-unrecorded state.
	newBalance := customer.CreditBalance - pkg.Price
	expiresAt := utils.CalculateExpiry(pkg.BillingCycle, customer.ExpiresAt, pkg.TimeLimitMinutes)
	var payment models.Payment
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&customer).Update("credit_balance", newBalance).Error; err != nil {
			return err
		}

		note := fmt.Sprintf("Purchased Package: %s", pkg.Name)
		if err := tx.Create(&models.CreditLog{
			CustomerID: customer.ID,
			Amount:     pkg.Price,
			Type:       "debit",
			Note:       &note,
		}).Error; err != nil {
			return err
		}

		payment = models.Payment{
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
		if err := tx.Create(&payment).Error; err != nil {
			return err
		}

		return tx.Model(&customer).Updates(map[string]interface{}{
			"status":     "active",
			"package_id": pkg.ID,
			"zone_id":    pkg.ZoneID,
			"expires_at": expiresAt,
		}).Error
	})
	if err != nil {
		log.Printf("[Credit Purchase] Transaction failed for Customer %d: %v", customer.ID, err)
		return utils.ErrorResponse(c, "Failed to complete purchase. Your credit balance was not charged.", "", fiber.StatusInternalServerError)
	}

	// This device gets its own router login for the new plan.
	var routerLogin *services.RouterLogin
	if body.Mac != "" {
		if rl, _, err := grantRouterLogin(customer.ID, body.Mac, zoneByID(fmt.Sprint(pkg.ZoneID)), "paid", true); err == nil {
			routerLogin = rl
		} else {
			log.Printf("[Credit Purchase] no router login for customer %d on %s: %v", customer.ID, body.Mac, err)
		}
	}

	// Send confirmation SMS
	templateActive := GetSetting("sms_template_active")
	msg := utils.RenderTemplate(templateActive, map[string]string{
		"name":    customer.Name,
		"package": pkg.Name,
		"expiry":  utils.Kenya(expiresAt).Format("2006-01-02 15:04"),
	})
	if GetSetting("sms_enable_active") != "no" {
		go smsSvcGlobal.SendForZone(pkg.ZoneID, customer.Phone, msg) //nolint:errcheck
	}

	return utils.SuccessResponse(c, fiber.Map{
		"credit_balance": newBalance,
		"expires_at":     expiresAt,
		"router_login":   routerLogin,
		"message":        "Package purchased successfully using credit balance.",
	}, "Purchase successful.")
}

// trialMu serialises free-trial grants so two quick taps can't both pass the
// one-trial-per-period check.
var trialMu sync.Mutex

// CustomerClaimFreeTier gives a device a free trial: its own short router
// login (never a shared `free` user). One trial per device MAC and per phone
// number per cooldown period, and never on top of / instead of a paid plan.
func CustomerClaimFreeTier(c *fiber.Ctx) error {
	var body struct {
		ZoneID    uint   `json:"zone_id"`
		PackageID uint   `json:"package_id"`
		Mac       string `json:"mac"`
		Phone     string `json:"phone"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	cleanMac, ok := services.NormalizeMAC(body.Mac)
	if !ok {
		return utils.ErrorResponse(c, "Open this page from the WiFi login screen to claim the free trial.", "", fiber.StatusBadRequest)
	}
	if body.ZoneID == 0 {
		return utils.ErrorResponse(c, "Open this page from the WiFi login screen to claim the free trial.", "", fiber.StatusBadRequest)
	}
	var zone models.Zone
	if err := config.DB.First(&zone, body.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "No free tier plan is currently available in this zone.", "", fiber.StatusNotFound)
	}

	// 1. The zone's free plan (an explicit package_id must still be this
	// zone's free plan — a paid package can't be claimed as a "trial").
	var pkg models.Package
	query := config.DB.Preload("Zone").Where("status = ? AND zone_id = ? AND (is_free_tier = ? OR price = 0)", "active", zone.ID, true)
	if body.PackageID > 0 {
		query = query.Where("id = ?", body.PackageID)
	}
	if err := query.First(&pkg).Error; err != nil {
		return utils.ErrorResponse(c, "No free tier plan is currently available in this zone.", "", fiber.StatusNotFound)
	}

	// Verify scheduled availability window (e.g. specific hours or days)
	if available, reason := IsFreeTierAvailableNow(&pkg); !available {
		return utils.ErrorResponse(c, reason, "Outside Scheduled Hours", fiber.StatusForbidden)
	}

	cooldownHours := pkg.FreeTierCooldownHours
	if cooldownHours <= 0 {
		cooldownHours = 24
	}
	now := time.Now()
	cooldownCutoff := now.Add(-time.Duration(cooldownHours) * time.Hour)
	phone := ""
	if strings.TrimSpace(body.Phone) != "" {
		phone = normalizePhone(body.Phone)
	}
	// Only for local development; no request parameter can skip the limit.
	bypassCooldown := config.Config.AppEnv == "local" && os.Getenv("FREE_TIER_COOLDOWN_DISABLED") == "true"

	trialMu.Lock()
	defer trialMu.Unlock()

	// 2. Never touch a paid plan: a device (or signed-in customer) that is
	// paid up just reconnects.
	for _, cred := range services.ActiveCredentialsForMAC(zone.OrganizationID, cleanMac) {
		if cred.Kind != "trial" {
			return routerLoginError(c, fiber.StatusConflict, "active_plan", "You already have an active plan on this device. Tap reconnect to get online.")
		}
	}
	var paid models.Customer
	if err := config.DB.Preload("Package").
		Where("(LOWER(mac_address) = ? OR REPLACE(LOWER(mac_address), '-', ':') = ?) AND status = ? AND expires_at > ? AND zone_id IN (SELECT id FROM zones WHERE organization_id = ?)",
			strings.ToLower(cleanMac), strings.ToLower(cleanMac), "active", now, zone.OrganizationID).
		First(&paid).Error; err == nil && paid.Package != nil && !paid.Package.IsFreeTier && paid.Package.Price > 0 {
		return routerLoginError(c, fiber.StatusConflict, "active_plan", "You already have an active plan on this device. Tap reconnect to get online.")
	}

	// 3. One trial per device and per phone per period (across the ISP).
	if !bypassCooldown {
		var claim models.FreeTrialClaim
		q := config.DB.Where("organization_id = ? AND claimed_at > ?", zone.OrganizationID, cooldownCutoff)
		if phone != "" {
			q = q.Where("mac_address = ? OR phone = ?", cleanMac, phone)
		} else {
			q = q.Where("mac_address = ?", cleanMac)
		}
		if err := q.Order("claimed_at DESC").First(&claim).Error; err == nil {
			// Still running? Hand the same trial back instead of a new one.
			var trialCust models.Customer
			if config.DB.First(&trialCust, claim.CustomerID).Error == nil && trialCust.ExpiresAt != nil &&
				trialCust.ExpiresAt.After(now) && trialCust.Status == "active" && claim.MacAddress == cleanMac {
				rl, _, err := grantRouterLogin(trialCust.ID, cleanMac, &zone, "trial", true)
				if err == nil {
					remaining := int(time.Until(*trialCust.ExpiresAt).Minutes())
					if remaining < 1 {
						remaining = 1
					}
					return utils.SuccessResponse(c, fiber.Map{
						"customer":      buildCustomerProfile(&trialCust),
						"expires_at":    *trialCust.ExpiresAt,
						"duration_mins": remaining,
						"router_login":  rl,
						"package_name":  pkg.Name,
						"message":       fmt.Sprintf("Your free trial is already active with %d minutes remaining.", remaining),
					}, "Free trial session resumed.")
				}
			}
			next := claim.ClaimedAt.Add(time.Duration(cooldownHours) * time.Hour)
			return utils.ErrorResponse(c, fmt.Sprintf("You have already used your free trial. You can claim again after %s or purchase a package.", utils.Kenya(next).Format("Jan 2 15:04")), "Cooldown Active", fiber.StatusTooManyRequests)
		}
	}

	// 4. Trial duration
	duration := 30 * time.Minute
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		duration = time.Duration(*pkg.TimeLimitMinutes) * time.Minute
	}
	expiresAt := now.Add(duration)

	// 5. A trial account of its own for this device (never a paying
	// customer's record, whose plan it would overwrite).
	var customer models.Customer
	err := config.DB.Where("account_number LIKE ? AND zone_id = ? AND (LOWER(mac_address) = ? OR mac_address = ?)", "ZYR#FREE#%", zone.ID, strings.ToLower(cleanMac), cleanMac).
		Order("id DESC").First(&customer).Error
	if err == nil {
		customer.PackageID = pkg.ID
		customer.Status = "active"
		customer.ExpiresAt = &expiresAt
		config.DB.Save(&customer)
	} else {
		var count int64
		config.DB.Unscoped().Model(&models.Customer{}).Where("account_number LIKE ?", "ZYR#FREE#%").Count(&count)
		macLower := strings.ToLower(cleanMac)
		customer = models.Customer{
			Name:          fmt.Sprintf("Free_User_%d", 10001+count),
			Phone:         fmt.Sprintf("FREE%d", 10001+count),
			ZoneID:        zone.ID,
			PackageID:     pkg.ID,
			Type:          "hotspot",
			Status:        "active",
			AccountNumber: fmt.Sprintf("ZYR#FREE#%d", 10001+count),
			ExpiresAt:     &expiresAt,
			MacAddress:    &macLower,
		}
		if err := config.DB.Create(&customer).Error; err != nil {
			return utils.ErrorResponse(c, "Failed to provision free tier account.", "", fiber.StatusInternalServerError)
		}
	}
	config.DB.Create(&models.FreeTrialClaim{
		OrganizationID: zone.OrganizationID, ZoneID: zone.ID, PackageID: pkg.ID, CustomerID: customer.ID,
		MacAddress: cleanMac, Phone: phone, ClaimedAt: now,
	})

	// 6. The device's own short router login.
	rl, _, err := grantRouterLogin(customer.ID, cleanMac, &zone, "trial", true)
	if err != nil {
		return utils.ErrorResponse(c, "Could not prepare your free trial. Please try again.", "", fiber.StatusInternalServerError)
	}

	// A device session: enough to see the trial, not to spend or change anything.
	token, _ := middleware.GenerateDeviceCustomerToken(customer.ID)
	middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)

	return utils.SuccessResponse(c, fiber.Map{
		"token":         token,
		"customer":      buildCustomerProfile(&customer),
		"expires_at":    expiresAt,
		"duration_mins": int(duration.Minutes()),
		"router_login":  rl,
		"package_name":  pkg.Name,
		"message":       fmt.Sprintf("Free trial of %d minutes activated! Enjoy your browsing.", int(duration.Minutes())),
	}, "Free trial activated successfully.")
}
