package handlers

import (
	"fmt"
	"log"

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

	stkResp, err := mpesaSvcGlobal.InitiateSTKPush(body.Phone, pkg.Price, ref, description)
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
