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

var mpesaSvcGlobal *services.MpesaService
var smsSvcGlobal *services.SmsService
var mikrotikSvcGlobal *services.MikroTikService

// InitMpesaService injects M-Pesa, SMS, and MikroTik services.
func InitMpesaService(mpesa *services.MpesaService, sms *services.SmsService, mikrotik *services.MikroTikService) {
	mpesaSvcGlobal = mpesa
	smsSvcGlobal = sms
	mikrotikSvcGlobal = mikrotik
}

// MpesaStkPush initiates an M-Pesa STK Push payment.
func MpesaStkPush(c *fiber.Ctx) error {
	var body struct {
		Phone      string `json:"phone"`
		Name       string `json:"name"`
		PackageID  uint   `json:"package_id"`
		CustomerID *uint  `json:"customer_id"`
		VoucherID  *uint  `json:"voucher_id"`
		Mac        string `json:"mac"`
		IP         string `json:"ip"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.Phone == "" || body.PackageID == 0 {
		return utils.ErrorResponse(c, "phone and package_id are required.", "", fiber.StatusUnprocessableEntity)
	}

	var pkg models.Package
	if err := config.DB.First(&pkg, body.PackageID).Error; err != nil {
		return utils.ErrorResponse(c, "Package not found.", "", fiber.StatusNotFound)
	}

	// This endpoint is intentionally public — it also serves the anonymous
	// captive-portal purchase flow (no customer_id: a fresh voucher is
	// generated below) and the "existing subscriber renews without a JWT
	// session" flow (customer_id supplied, e.g. resolved client-side from a
	// previous guest/MAC lookup).
	//
	// The customer app now sends its session cookie on every request
	// (credentials: 'include'), and by the time a customer reaches package
	// purchase they've already gone through guest-login or OTP login, so a
	// valid customerAuth session is very likely present here. Prefer that
	// over any body-supplied customer_id — it's the actual authenticated
	// identity and needs no further trust decision.
	if cc := middleware.OptionalCustomerClaims(c); cc != nil && cc.CustomerID != 0 {
		cid := cc.CustomerID
		body.CustomerID = &cid
	} else if body.CustomerID != nil {
		var owner models.Customer
		if err := config.DB.First(&owner, *body.CustomerID).Error; err != nil {
			return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
		}
	} else if body.CustomerID == nil {
		var existingCustomer models.Customer
		if err := config.DB.Where("phone = ?", body.Phone).First(&existingCustomer).Error; err == nil {
			body.CustomerID = &existingCustomer.ID
		} else if body.Mac != "" {
			var dev models.CustomerDevice
			if err := config.DB.Where("mac_address = ?", body.Mac).First(&dev).Error; err == nil {
				body.CustomerID = &dev.CustomerID
			}
		}
	}

	if body.CustomerID != nil {
		var owner models.Customer
		if err := config.DB.First(&owner, *body.CustomerID).Error; err == nil {
			if body.Mac == "" && owner.MacAddress != nil {
				body.Mac = *owner.MacAddress
			}
			if body.Mac != "" && (owner.MacAddress == nil || *owner.MacAddress == "") {
				owner.MacAddress = &body.Mac
				config.DB.Model(&owner).Update("mac_address", body.Mac)
			}
		}
	}

	if body.CustomerID != nil && strings.TrimSpace(body.Name) != "" {
		trimmedName := strings.TrimSpace(body.Name)
		config.DB.Model(&models.Customer{}).Where("id = ?", *body.CustomerID).Update("name", trimmedName)
	}

	var voucherID *uint = body.VoucherID

	// Auto-generate voucher if neither customer nor voucher specified
	if body.CustomerID == nil && body.VoucherID == nil {
		voucher, err := voucherSvcGlobal.Generate(pkg.ZoneID, pkg.ID, "single_use", 1)
		if err != nil {
			return utils.ErrorResponse(c, err.Error(), "Failed to prepare voucher.", fiber.StatusBadRequest)
		}
		voucherID = &voucher.ID
	}

	// Create pending payment
	payment := models.Payment{
		CustomerID: body.CustomerID,
		VoucherID:  voucherID,
		ZoneID:     pkg.ZoneID,
		PackageID:  &pkg.ID,
		Phone:      body.Phone,
		Amount:     pkg.Price,
		Currency:   "KES",
		Method:     "mpesa",
		Status:     "pending",
		MacAddress: body.Mac,
		IpAddress:  body.IP,
	}
	if err := config.DB.Create(&payment).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create payment record.", fiber.StatusInternalServerError)
	}

	var ref string
	if body.CustomerID != nil {
		ref = fmt.Sprintf("Cust%d", *body.CustomerID)
	} else if voucherID != nil {
		ref = fmt.Sprintf("Vouch%d", *voucherID)
	}
	description := "WiFi"
	if pkg.Name != "" {
		description = pkg.Name
	}

	stkResp, err := mpesaSvcGlobal.InitiateSTKPush(pkg.ZoneID, body.Phone, pkg.Price, ref, description)
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
			mpesaSvcGlobal.SimulateCallback(stkResp.CheckoutRequestID, pkg.Price, body.Phone)
		}

		return utils.SuccessResponse(c, fiber.Map{
			"payment_id":     payment.ID,
			"transaction_id": stkResp.CheckoutRequestID,
			"message":        stkResp.ResponseDescription,
		}, "STK Push initiated successfully.")
	}

	reason := "Failed to initiate M-Pesa STK Push payment."
	config.DB.Model(&payment).Updates(map[string]interface{}{
		"status":        "failed",
		"status_reason": reason,
	})
	return utils.ErrorResponse(c, reason, "", fiber.StatusBadRequest)
}

// mpesaCallbackAuthorized guards the public Daraja callback/C2B endpoints.
//
// These routes must stay unauthenticated (Safaricom calls them directly,
// with no way for us to hand it a JWT), but that also means anyone who
// knows the URL can POST a forged "payment succeeded" notification. The
// CheckoutRequestID alone isn't a secret — MpesaStkPush returns it straight
// to the client that just initiated the push — so matching it to a pending
// Payment row (done in HandleCallback) blocks a *stranger* from crediting
// someone else's payment, but doesn't stop the very customer who requested
// that STK push from replaying/forging their own "it succeeded" callback
// before actually paying.
//
// The simplest robust fix that doesn't require IP allowlisting (Safaricom's
// published callback IP ranges change and aren't guaranteed stable, and
// Railway/most PaaS deployments sit behind a proxy that complicates trusting
// c.IP() anyway) is a shared secret embedded in the callback URL itself:
// operators append `?token=<MPESA_CALLBACK_SECRET>` to the CallBackURL /
// ValidationURL / ConfirmationURL they register with Daraja. That token is
// never returned to the payer in any API response, so it can't be derived
// from the STK-push flow the way CheckoutRequestID can.
func mpesaCallbackAuthorized(c *fiber.Ctx) bool {
	secret := config.Config.MpesaCallbackSecret
	if secret == "" {
		log.Printf("[M-Pesa] WARNING: MPESA_CALLBACK_SECRET is not set — callback/C2B endpoints accept requests from anyone. Set MPESA_CALLBACK_SECRET and append ?token=<secret> to your Daraja CallBackURL/ValidationURL/ConfirmationURL.")
		return true
	}
	return c.Query("token") == secret
}

// MpesaCallback handles the async Daraja payment notification (PUBLIC, but
// see mpesaCallbackAuthorized).
func MpesaCallback(c *fiber.Ctx) error {
	if !mpesaCallbackAuthorized(c) {
		log.Printf("[M-Pesa] Rejected callback with missing/invalid token from %s", c.IP())
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Unauthorized"})
	}

	var payload map[string]interface{}
	if err := c.BodyParser(&payload); err != nil {
		log.Printf("[M-Pesa] Failed to parse callback: %v", err)
		return c.JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Parse error"})
	}

	if err := mpesaSvcGlobal.HandleCallback(payload); err != nil {
		log.Printf("[M-Pesa] Callback processing error: %v", err)
		return c.JSON(fiber.Map{"ResultCode": 1, "ResultDesc": err.Error()})
	}

	return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Success"})
}

// MpesaC2BValidation handles M-Pesa C2B Paybill validation requests from
// Safaricom (PUBLIC, but see mpesaCallbackAuthorized).
func MpesaC2BValidation(c *fiber.Ctx) error {
	if !mpesaCallbackAuthorized(c) {
		log.Printf("[C2B Validation] Rejected request with missing/invalid token from %s", c.IP())
		return c.JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Rejected"})
	}

	var body struct {
		TransactionType   string  `json:"TransactionType"`
		TransID           string  `json:"TransID"`
		TransTime         string  `json:"TransTime"`
		TransAmount       float64 `json:"TransAmount,string"`
		BusinessShortCode string  `json:"BusinessShortCode"`
		BillRefNumber     string  `json:"BillRefNumber"`
		MSISDN            string  `json:"MSISDN"`
		FirstName         string  `json:"FirstName"`
	}
	if err := c.BodyParser(&body); err != nil {
		log.Printf("[C2B Validation] Failed to parse payload: %v", err)
	}
	log.Printf("[C2B Validation] Query for Account: %s, Amount: KES %.2f, TransID: %s", body.BillRefNumber, body.TransAmount, body.TransID)

	return c.JSON(fiber.Map{
		"ResultCode": 0,
		"ResultDesc": "Accepted",
	})
}

// MpesaC2BConfirmation handles M-Pesa C2B Paybill confirmation notifications
// from Safaricom (PUBLIC, but see mpesaCallbackAuthorized). This endpoint
// credits customer.CreditBalance directly off unauthenticated POST body
// fields, so the shared-secret check is the only thing standing between a
// forged request and free credit — there is no equivalent of the
// "matching pending Payment row" check MpesaCallback has, since a C2B
// payment isn't tied to a payment we initiated.
func MpesaC2BConfirmation(c *fiber.Ctx) error {
	if !mpesaCallbackAuthorized(c) {
		log.Printf("[C2B Confirmation] Rejected request with missing/invalid token from %s", c.IP())
		return c.JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Rejected"})
	}

	var body struct {
		TransactionType   string  `json:"TransactionType"`
		TransID           string  `json:"TransID"`
		TransTime         string  `json:"TransTime"`
		TransAmount       float64 `json:"TransAmount,string"`
		BusinessShortCode string  `json:"BusinessShortCode"`
		BillRefNumber     string  `json:"BillRefNumber"`
		MSISDN            string  `json:"MSISDN"`
		FirstName         string  `json:"FirstName"`
		MiddleName        string  `json:"MiddleName"`
		LastName          string  `json:"LastName"`
	}
	if err := c.BodyParser(&body); err != nil {
		log.Printf("[C2B Confirmation] Failed to parse payload: %v", err)
		return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Received"})
	}

	log.Printf("[C2B Confirmation] TransID: %s | Amount: KES %.2f | Account: %s | Phone: %s | Name: %s %s %s", 
		body.TransID, body.TransAmount, body.BillRefNumber, body.MSISDN, body.FirstName, body.MiddleName, body.LastName)

	cleanPhone := utils.FormatPhone(body.MSISDN)
	phoneSuffix := cleanPhone
	if len(phoneSuffix) >= 9 {
		phoneSuffix = phoneSuffix[len(phoneSuffix)-9:]
	}

	// Format Title Case Name from C2B payload
	nameParts := []string{}
	if body.FirstName != "" {
		nameParts = append(nameParts, strings.Title(strings.ToLower(strings.TrimSpace(body.FirstName))))
	}
	if body.MiddleName != "" {
		nameParts = append(nameParts, strings.Title(strings.ToLower(strings.TrimSpace(body.MiddleName))))
	}
	if body.LastName != "" {
		nameParts = append(nameParts, strings.Title(strings.ToLower(strings.TrimSpace(body.LastName))))
	}
	payerName := strings.Join(nameParts, " ")

	var customer models.Customer
	foundCustomer := false
	if body.BillRefNumber != "" {
		if err := config.DB.Where("account_number = ? OR pppoe_username = ? OR phone LIKE ?", body.BillRefNumber, body.BillRefNumber, "%"+body.BillRefNumber).First(&customer).Error; err == nil {
			foundCustomer = true
		}
	}
	if !foundCustomer && phoneSuffix != "" {
		if err := config.DB.Where("phone LIKE ?", "%"+phoneSuffix).First(&customer).Error; err == nil {
			foundCustomer = true
		}
	}

	// If customer still not found, auto-link to latest guest customer or create fresh customer
	if !foundCustomer && cleanPhone != "" {
		var guestCustomer models.Customer
		if err := config.DB.Where("phone LIKE 'GUEST%'").Order("id DESC").First(&guestCustomer).Error; err == nil {
			customer = guestCustomer
			customer.Phone = cleanPhone
			customer.Name = payerName
			customer.AccountNumber = "ZYR#" + cleanPhone
			config.DB.Model(&customer).Updates(map[string]interface{}{
				"phone":          cleanPhone,
				"name":           payerName,
				"account_number": "ZYR#" + cleanPhone,
			})
			foundCustomer = true
		} else {
			newCustomer := models.Customer{
				Name:          payerName,
				Phone:         cleanPhone,
				AccountNumber: "ZYR#" + cleanPhone,
				ZoneID:        1,
				Type:          "hotspot",
				Status:        "active",
				CreditBalance: 0,
			}
			if err := config.DB.Create(&newCustomer).Error; err == nil {
				customer = newCustomer
				foundCustomer = true
			}
		}
	}

	transIDStr := body.TransID

	if !foundCustomer {
		unmatched := models.UnmatchedC2BPayment{
			TransID:           transIDStr,
			Phone:             cleanPhone,
			Amount:            body.TransAmount,
			BillRefNumber:     body.BillRefNumber,
			BusinessShortCode: body.BusinessShortCode,
			FirstName:         body.FirstName,
			LastName:          strings.TrimSpace(body.MiddleName + " " + body.LastName),
			Status:            "pending",
		}
		if err := config.DB.Create(&unmatched).Error; err != nil {
			log.Printf("[C2B Confirmation] Failed to queue unmatched payment %s: %v", transIDStr, err)
		}
		return c.JSON(fiber.Map{
			"ResultCode": 0,
			"ResultDesc": "Success",
		})
	}

	customerID := &customer.ID
	zoneID := customer.ZoneID
	pkgID := customer.PackageID
	packageID := &pkgID

	// Always update customer name from official M-Pesa C2B legal names
	if payerName != "" {
		customer.Name = payerName
		config.DB.Model(&customer).Update("name", payerName)
	}

	{
		customer.CreditBalance += body.TransAmount
		config.DB.Model(&customer).Update("credit_balance", customer.CreditBalance)

		noteStr := fmt.Sprintf("M-Pesa C2B Paybill %s", body.TransID)
		config.DB.Create(&models.CreditLog{
			CustomerID: customer.ID,
			Amount:     body.TransAmount,
			Type:       "credit",
			Note:       &noteStr,
		})

		var pkg models.Package
		// 1. Try to find a package in this zone matching the exact transaction amount (e.g. KES 5)
		if err := config.DB.Where("zone_id = ? AND price = ? AND status = 'active'", customer.ZoneID, body.TransAmount).First(&pkg).Error; err != nil {
			// 2. Otherwise try matching against customer's assigned PackageID
			config.DB.First(&pkg, customer.PackageID)
		}

		if pkg.ID > 0 && customer.CreditBalance >= pkg.Price {
			customer.PackageID = pkg.ID
			customer.CreditBalance -= pkg.Price
			durationMinutes := 30 * 24 * 60
			if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
				durationMinutes = *pkg.TimeLimitMinutes
			} else if pkg.BillingCycle == "hourly" {
				durationMinutes = 60
			} else if pkg.BillingCycle == "daily" {
				durationMinutes = 24 * 60
			} else if pkg.BillingCycle == "weekly" {
				durationMinutes = 7 * 24 * 60
			}

			newExpiry := time.Now().Add(time.Duration(durationMinutes) * time.Minute)
			if customer.ExpiresAt != nil && customer.ExpiresAt.After(time.Now()) {
				newExpiry = customer.ExpiresAt.Add(time.Duration(durationMinutes) * time.Minute)
			}
			config.DB.Model(&customer).Updates(map[string]interface{}{
				"package_id":     pkg.ID,
				"credit_balance": customer.CreditBalance,
				"expires_at":     newExpiry,
				"status":         "active",
			})

			// Instant MikroTik Auto-Reconnect Push (unblocks PPPoE secret or Hotspot MAC)
			if mikrotikSvcGlobal != nil && customer.ZoneID > 0 {
				var zone models.Zone
				if err := config.DB.First(&zone, customer.ZoneID).Error; err == nil {
					customer.Package = &pkg
					go mikrotikSvcGlobal.ReactivateCustomer(&zone, &customer)
				}
			}
		}

		if smsSvcGlobal != nil && customer.Phone != "" {
			smsMsg := fmt.Sprintf("Payment Received: KES %.2f via M-Pesa (%s). Account balance: KES %.2f.", body.TransAmount, body.TransID, customer.CreditBalance)
			go smsSvcGlobal.SendForZone(customer.ZoneID, customer.Phone, smsMsg)
		}
	}

	payment := models.Payment{
		CustomerID:         customerID,
		ZoneID:             zoneID,
		PackageID:          packageID,
		Phone:              cleanPhone,
		Amount:             body.TransAmount,
		Currency:           "KES",
		Method:             "mpesa_c2b",
		Status:             "completed",
		MpesaReceiptNumber: &transIDStr,
		MpesaTransactionID: &transIDStr,
	}
	config.DB.Create(&payment)

	return c.JSON(fiber.Map{
		"ResultCode": 0,
		"ResultDesc": "Success",
	})
}
