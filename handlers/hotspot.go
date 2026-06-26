package handlers

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// HotspotPay handles payment initiation from the captive portal.
func HotspotPay(c *fiber.Ctx) error {
	var body struct {
		Phone  string `json:"phone"`
		PlanID string `json:"plan_id"`
		Mac    string `json:"mac"`
		IP     string `json:"ip"`
	}

	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	if body.Phone == "" || body.PlanID == "" {
		return utils.ErrorResponse(c, "phone and plan_id are required.", "", fiber.StatusUnprocessableEntity)
	}

	phone := normalizePhone(body.Phone)

	// Get first active zone
	var zone models.Zone
	if err := config.DB.First(&zone).Error; err != nil {
		return utils.ErrorResponse(c, "No zone configured on the server.", "", fiber.StatusInternalServerError)
	}

	// Map plan_id to package details
	var price float64
	var speedDown, speedUp int
	var name string

	switch strings.ToLower(body.PlanID) {
	case "basic":
		price = 20
		speedDown = 2048
		speedUp = 1024
		name = "Basic"
	case "premium":
		price = 50
		speedDown = 10240
		speedUp = 5120
		name = "Premium"
	default: // standard
		price = 30
		speedDown = 5120
		speedUp = 2048
		name = "Standard"
	}

	// Find or create package in DB
	var pkg models.Package
	if err := config.DB.Where("price = ? AND type = ? AND zone_id = ?", price, "hotspot", zone.ID).First(&pkg).Error; err != nil {
		pkg = models.Package{
			ZoneID:            zone.ID,
			Name:              name,
			Type:              "hotspot",
			Price:             price,
			BillingCycle:      "daily",
			SpeedDownloadKbps: speedDown,
			SpeedUploadKbps:   speedUp,
			Status:            "active",
		}
		if err := config.DB.Create(&pkg).Error; err != nil {
			return utils.ErrorResponse(c, err.Error(), "Failed to create package.", fiber.StatusInternalServerError)
		}
	}

	// Auto-generate voucher
	voucher, err := voucherSvcGlobal.Generate(zone.ID, pkg.ID, "single_use", 1)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to prepare voucher.", fiber.StatusInternalServerError)
	}

	// Create pending payment record
	payment := models.Payment{
		CustomerID: nil,
		VoucherID:  &voucher.ID,
		ZoneID:     zone.ID,
		PackageID:  &pkg.ID,
		Phone:      phone,
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

	ref := fmt.Sprintf("Vouch-%d", voucher.ID)
	description := "Internet Payment for " + pkg.Name

	stkResp, err := mpesaSvcGlobal.InitiateSTKPush(phone, pkg.Price, ref, description)
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
			mpesaSvcGlobal.SimulateCallback(stkResp.CheckoutRequestID, pkg.Price, phone)
		}

		return c.JSON(fiber.Map{
			"reference": stkResp.CheckoutRequestID,
			"message":   "STK Push initiated successfully.",
		})
	}

	reason := "Failed to initiate M-Pesa STK Push payment."
	config.DB.Model(&payment).Updates(map[string]interface{}{
		"status":        "failed",
		"status_reason": reason,
	})
	return utils.ErrorResponse(c, reason, "", fiber.StatusBadRequest)
}

// HotspotStatus checks payment status for polling.
func HotspotStatus(c *fiber.Ctx) error {
	ref := c.Params("reference")
	if ref == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"status": "failed", "message": "Reference is required"})
	}

	var payment models.Payment
	if err := config.DB.Where("mpesa_transaction_id = ?", ref).First(&payment).Error; err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"status": "failed", "message": "Payment record not found"})
	}

	if payment.Status == "completed" {
		return c.JSON(fiber.Map{"status": "paid"})
	} else if payment.Status == "failed" {
		msg := "Payment failed"
		if payment.StatusReason != nil {
			msg = *payment.StatusReason
		}
		return c.JSON(fiber.Map{"status": "failed", "message": msg})
	}

	// Reconcile status with M-Pesa query API as a fallback if the transaction is pending
	if payment.Status == "pending" && mpesaSvcGlobal != nil {
		newStatus, err := mpesaSvcGlobal.QueryAndUpdateSTKStatus(&payment)
		if err == nil {
			if newStatus == "completed" {
				return c.JSON(fiber.Map{"status": "paid"})
			} else if newStatus == "failed" {
				msg := "Payment failed"
				if payment.StatusReason != nil {
					msg = *payment.StatusReason
				}
				return c.JSON(fiber.Map{"status": "failed", "message": msg})
			}
		} else {
			log.Printf("[M-Pesa] Error querying STK status: %v", err)
		}
	}

	return c.JSON(fiber.Map{"status": "pending"})
}

// HotspotSession returns the active session stats for a MAC address.
func HotspotSession(c *fiber.Ctx) error {
	mac := c.Query("mac")
	if mac == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"active": false, "error": "mac is required"})
	}

	var zone models.Zone
	if err := config.DB.First(&zone).Error; err != nil {
		return c.JSON(fiber.Map{"active": false})
	}

	sessions, err := mikrotikSvc.GetActiveSessions(&zone)
	if err != nil {
		// Local development bypass
		if config.Config.AppEnv == "local" {
			log.Printf("[Hotspot] Router connection failed: %v. Returning mock session for local testing.", err)
			var payment models.Payment
			if err := config.DB.Preload("Package").Where("mac_address = ? AND status = ?", mac, "completed").Order("created_at DESC").First(&payment).Error; err == nil && payment.Package != nil {
				return c.JSON(fiber.Map{
					"active":    true,
					"plan_name": payment.Package.Name,
					"speed":     fmt.Sprintf("%.0f Mbps", float64(payment.Package.SpeedDownloadKbps)/1024),
					"time_left": 86300, // 24 hours
					"bytes_in":  104857600, // 100 MB
					"bytes_out": 20971520,  // 20 MB
				})
			}
		}
		return c.JSON(fiber.Map{"active": false})
	}

	// Find the session matching the MAC
	for _, s := range sessions {
		if strings.EqualFold(s.MAC, mac) {
			planName := "Standard"
			speed := "5 Mbps"
			var timeLeft int64 = 86400

			// Enrich from database
			var voucher models.Voucher
			if err := config.DB.Preload("Package").Where("code = ?", s.Username).First(&voucher).Error; err == nil && voucher.Package != nil {
				planName = voucher.Package.Name
				speed = fmt.Sprintf("%.0f Mbps", float64(voucher.Package.SpeedDownloadKbps)/1024)
				if voucher.ExpiresAt != nil {
					timeLeft = int64(time.Until(*voucher.ExpiresAt).Seconds())
				}
			} else {
				var payment models.Payment
				if err := config.DB.Preload("Package").Where("mac_address = ? AND status = ?", mac, "completed").Order("created_at DESC").First(&payment).Error; err == nil && payment.Package != nil {
					planName = payment.Package.Name
					speed = fmt.Sprintf("%.0f Mbps", float64(payment.Package.SpeedDownloadKbps)/1024)
					// Estimate time left (Daily plan is 24h)
					expiry := payment.CreatedAt.Add(24 * time.Hour)
					timeLeft = int64(time.Until(expiry).Seconds())
				}
			}

			if timeLeft < 0 {
				timeLeft = 0
			}

			return c.JSON(fiber.Map{
				"active":    true,
				"plan_name": planName,
				"speed":     speed,
				"time_left": timeLeft,
				"bytes_in":  s.BytesIn,
				"bytes_out": s.BytesOut,
			})
		}
	}

	return c.JSON(fiber.Map{"active": false})
}

// HotspotLogout disconnects a client by MAC.
func HotspotLogout(c *fiber.Ctx) error {
	var body struct {
		Mac string `json:"mac"`
	}
	if err := c.BodyParser(&body); err != nil || body.Mac == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"success": false, "error": "mac is required"})
	}

	var zone models.Zone
	if err := config.DB.First(&zone).Error; err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": "No zones configured"})
	}

	if err := mikrotikSvc.DisconnectClient(&zone, body.Mac); err != nil {
		log.Printf("[Hotspot] Failed to disconnect client %s: %v", body.Mac, err)
		if config.Config.AppEnv == "local" {
			log.Printf("[Hotspot] Dev mode: bypassing disconnect failure.")
			return c.JSON(fiber.Map{"success": true})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"success": false, "error": err.Error()})
	}

	return c.JSON(fiber.Map{"success": true})
}
