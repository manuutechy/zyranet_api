package handlers

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

var mpesaSvcGlobal *services.MpesaService
var smsSvcGlobal *services.SmsService

// InitMpesaService injects M-Pesa and SMS services.
func InitMpesaService(mpesa *services.MpesaService, sms *services.SmsService) {
	mpesaSvcGlobal = mpesa
	smsSvcGlobal = sms
}

// MpesaStkPush initiates an M-Pesa STK Push payment.
func MpesaStkPush(c *fiber.Ctx) error {
	var body struct {
		Phone      string `json:"phone"`
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
		ref = fmt.Sprintf("Cust-%d", *body.CustomerID)
	} else if voucherID != nil {
		ref = fmt.Sprintf("Vouch-%d", *voucherID)
	}
	description := "Internet Payment for " + pkg.Name

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

// MpesaCallback handles the async Daraja payment notification (PUBLIC).
func MpesaCallback(c *fiber.Ctx) error {
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

// MpesaC2BValidation handles M-Pesa C2B Paybill validation requests from Safaricom.
func MpesaC2BValidation(c *fiber.Ctx) error {
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

// MpesaC2BConfirmation handles M-Pesa C2B Paybill confirmation notifications from Safaricom.
func MpesaC2BConfirmation(c *fiber.Ctx) error {
	var body struct {
		TransactionType   string  `json:"TransactionType"`
		TransID           string  `json:"TransID"`
		TransTime         string  `json:"TransTime"`
		TransAmount       float64 `json:"TransAmount,string"`
		BusinessShortCode string  `json:"BusinessShortCode"`
		BillRefNumber     string  `json:"BillRefNumber"`
		MSISDN            string  `json:"MSISDN"`
		FirstName         string  `json:"FirstName"`
		LastName          string  `json:"LastName"`
	}
	if err := c.BodyParser(&body); err != nil {
		log.Printf("[C2B Confirmation] Failed to parse payload: %v", err)
		return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Received"})
	}

	log.Printf("[C2B Confirmation] TransID: %s | Amount: KES %.2f | Account: %s | Phone: %s", body.TransID, body.TransAmount, body.BillRefNumber, body.MSISDN)

	phone := body.MSISDN
	if len(phone) > 9 && !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}

	var customer models.Customer
	foundCustomer := false
	if body.BillRefNumber != "" {
		if err := config.DB.Where("account_number = ? OR pppoe_username = ? OR phone = ?", body.BillRefNumber, body.BillRefNumber, body.BillRefNumber).First(&customer).Error; err == nil {
			foundCustomer = true
		}
	}
	if !foundCustomer && phone != "" {
		if err := config.DB.Where("phone = ?", phone).First(&customer).Error; err == nil {
			foundCustomer = true
		}
	}

	var customerID *uint
	var zoneID uint = 1
	var packageID *uint
	transIDStr := body.TransID

	if foundCustomer {
		customerID = &customer.ID
		zoneID = customer.ZoneID
		pkgID := customer.PackageID
		packageID = &pkgID

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
		if err := config.DB.First(&pkg, customer.PackageID).Error; err == nil && customer.CreditBalance >= pkg.Price {
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
				"credit_balance": customer.CreditBalance,
				"expires_at":     newExpiry,
				"status":         "active",
			})
		}

		if smsSvcGlobal != nil && customer.Phone != "" {
			smsMsg := fmt.Sprintf("Payment Received: KES %.2f via M-Pesa (%s). Account balance: KES %.2f.", body.TransAmount, body.TransID, customer.CreditBalance)
			go smsSvcGlobal.Send(customer.Phone, smsMsg)
		}
	}

	payment := models.Payment{
		CustomerID:         customerID,
		ZoneID:             zoneID,
		PackageID:          packageID,
		Phone:              phone,
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
