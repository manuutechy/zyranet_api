package handlers

import (
	"crypto/subtle"
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
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

	// TODO(router-access branch): refuse checkout while the zone's router is
	// offline, once services.ZoneAcceptingPayments lands:
	//   if ok, why := services.ZoneAcceptingPayments(pkg.ZoneID); !ok { return utils.ErrorResponse(c, why, "", fiber.StatusServiceUnavailable) }

	// One prompt at a time per phone: a second tap within 90s (same phone, or
	// same device on the same zone) gets the prompt already on their phone
	// instead of a new one — two prompts risk paying twice.
	unlock := mpesaSvcGlobal.LockSTKPhone(body.Phone)
	defer unlock()
	if existing, ok := services.RecentPendingSTK(body.Phone, body.Mac, pkg.ZoneID, 90*time.Second); ok {
		return utils.SuccessResponse(c, fiber.Map{
			"payment_id":     existing.ID,
			"transaction_id": *existing.MpesaTransactionID,
			"message":        "An M-Pesa prompt was already sent to this phone. Enter your PIN on it to finish paying.",
			"reused":         true,
		}, "STK Push already in progress.")
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
		// A customer id from the request must belong to the ISP selling this package.
		if !services.CustomerBelongsToZoneISP(*body.CustomerID, pkg.ZoneID) {
			return utils.ErrorResponse(c, "Customer not found.", "", fiber.StatusNotFound)
		}
	} else if body.CustomerID == nil {
		if cust, ok := services.CustomerByPhoneForZone(pkg.ZoneID, body.Phone); ok {
			body.CustomerID = &cust.ID
		} else if cust, ok := services.CustomerByDeviceForZone(pkg.ZoneID, body.Mac); ok {
			body.CustomerID = &cust.ID
		}
	}
	// A signed-in customer buying on another ISP's WiFi is a new customer there.
	if body.CustomerID != nil && !services.CustomerBelongsToZoneISP(*body.CustomerID, pkg.ZoneID) {
		body.CustomerID = nil
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

	// No voucher is made here any more: an anonymous purchase gets its
	// voucher in ProcessPaymentSuccess, once the money has actually arrived,
	// so abandoned/failed pushes don't leave redeemable codes behind. A
	// client-supplied voucher is only accepted for this exact zone + package
	// (success re-enables it, so any other voucher would be a free reset).
	var voucherID *uint
	if body.VoucherID != nil {
		var v models.Voucher
		if err := config.DB.Where("zone_id = ? AND package_id = ?", pkg.ZoneID, pkg.ID).First(&v, *body.VoucherID).Error; err == nil {
			voucherID = &v.ID
		}
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

// mpesaCallbackAuthorized guards the public Daraja callback endpoints.
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
// The fix is a shared secret embedded in the callback URL itself:
// InitiateSTKPush appends `?token=<MPESA_CALLBACK_SECRET>` to every
// CallBackURL it hands Daraja, and B2B/transaction-status results carry it
// too. That token is never returned to the payer, so it can't be derived from
// the STK-push flow the way CheckoutRequestID can. With no secret configured
// everything is rejected (and the server refuses to start outside local/test —
// see config.CallbackSecretProblem).
//
// C2B paybill confirmations are the exception: the live paybill's URLs were
// registered with Safaricom without a token, so a confirmation is accepted
// either way and, without the token, verified with Safaricom before anything
// is credited (see MpesaC2BConfirmation).
func mpesaCallbackAuthorized(c *fiber.Ctx) bool {
	secret := config.Config.MpesaCallbackSecret
	if secret == "" {
		log.Printf("[M-Pesa] MPESA_CALLBACK_SECRET is not set — rejecting unauthenticated callback on %s", c.Path())
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Query("token")), []byte(secret)) == 1
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
// Safaricom (PUBLIC). It always accepts: validation moves no money (crediting
// happens only on confirmation, after verification), and rejecting a
// tokenless request here would make Safaricom decline real customers'
// payments on the live paybill, whose URLs carry no token.
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

// c2bConfirmation is the body Safaricom POSTs to the confirmation URL.
type c2bConfirmation struct {
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

// c2bVerifyAsync runs the Safaricom status check in the background so the
// confirmation is acknowledged quickly. Tests turn it off.
var c2bVerifyAsync = true

// MpesaC2BConfirmation handles M-Pesa C2B Paybill confirmation notifications
// from Safaricom (PUBLIC).
//
// Nothing here is credited on the request's word alone:
//
//   - Every TransID is first claimed in the c2b_transactions ledger (unique
//     key), so a re-delivered or replayed confirmation is a no-op — even two
//     racing deliveries can't both credit.
//   - With a valid ?token= (URLs registered with the secret) the request is
//     trusted like an STK callback and credited at once.
//   - Without it (the live paybill's URLs carry no token) Safaricom is asked
//     for its own record of the TransID (Transaction Status) and the money is
//     credited only when that answer matches — see MpesaC2BVerifyResult. If
//     the check can't be made, the payment is parked in unmatched_c2b_payments
//     for staff to confirm against the M-Pesa statement.
func MpesaC2BConfirmation(c *fiber.Ctx) error {
	ack := func() error { return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Success"}) }

	var body c2bConfirmation
	if err := c.BodyParser(&body); err != nil {
		log.Printf("[C2B Confirmation] Failed to parse payload: %v", err)
		return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Received"})
	}

	tokenOK := mpesaCallbackAuthorized(c)
	log.Printf("[C2B Confirmation] TransID: %s | Amount: KES %.2f | Account: %s | Phone: %s | Name: %s %s %s | token=%v",
		body.TransID, body.TransAmount, body.BillRefNumber, body.MSISDN, body.FirstName, body.MiddleName, body.LastName, tokenOK)

	transIDStr := strings.TrimSpace(body.TransID)
	if transIDStr == "" || body.TransAmount <= 0 {
		log.Printf("[C2B Confirmation] Ignoring confirmation without a TransID/amount")
		return ack()
	}
	// Transactions recorded before the ledger existed.
	if c2bAlreadyRecorded(transIDStr) {
		log.Printf("[C2B Confirmation] TransID %s already recorded — ignoring duplicate delivery", transIDStr)
		return ack()
	}

	ledger := models.C2BTransaction{
		TransID:           transIDStr,
		TransTime:         body.TransTime,
		Amount:            body.TransAmount,
		BusinessShortCode: strings.TrimSpace(body.BusinessShortCode),
		BillRefNumber:     body.BillRefNumber,
		MSISDN:            body.MSISDN,
		FirstName:         body.FirstName,
		MiddleName:        body.MiddleName,
		LastName:          body.LastName,
		Status:            "received",
	}
	res := config.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&ledger)
	if res.Error != nil {
		// Not acknowledging makes Safaricom retry later, rather than losing it.
		log.Printf("[C2B Confirmation] Could not record TransID %s: %v", transIDStr, res.Error)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Try again"})
	}
	if res.RowsAffected == 0 {
		log.Printf("[C2B Confirmation] TransID %s already received — ignoring duplicate delivery", transIDStr)
		return ack()
	}

	if tokenOK {
		ledger.Verification = "token"
		creditC2B(&ledger)
		return ack()
	}

	if c2bVerifyAsync {
		go startC2BVerification(ledger)
	} else {
		startC2BVerification(ledger)
	}
	return ack()
}

// startC2BVerification asks Safaricom to confirm a tokenless confirmation, or
// parks it for manual review when that can't be done.
func startC2BVerification(l models.C2BTransaction) {
	if mpesaSvcGlobal == nil {
		services.ParkC2B(&l, "Not verified: the M-Pesa service is not available")
		return
	}
	convID, err := mpesaSvcGlobal.RequestC2BVerification(l.TransID, l.BusinessShortCode)
	if err != nil {
		services.ParkC2B(&l, "Not verified with Safaricom ("+err.Error()+") — check the M-Pesa statement before crediting")
		return
	}
	config.DB.Model(&models.C2BTransaction{}).
		Where("id = ? AND status = ?", l.ID, "received").
		Updates(map[string]interface{}{"status": "verifying", "verify_conversation_id": convID})
	log.Printf("[C2B] %s sent to Safaricom for verification (conversation %s)", l.TransID, convID)
}

// MpesaC2BVerifyResult receives Safaricom's Transaction Status answer for a
// tokenless paybill confirmation (PUBLIC, token required — we built this URL
// ourselves, with the secret). A match credits exactly as a trusted
// confirmation would; anything else parks the payment for staff.
func MpesaC2BVerifyResult(c *fiber.Ctx) error {
	if !mpesaCallbackAuthorized(c) {
		log.Printf("[C2B Verify] Rejected result with missing/invalid token from %s", c.IP())
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Unauthorized"})
	}
	var payload map[string]interface{}
	if err := c.BodyParser(&payload); err != nil {
		return c.JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Parse error"})
	}
	result, err := services.ParseTransactionStatusResult(payload)
	if err != nil || result.OriginatorConversationID == "" {
		log.Printf("[C2B Verify] Unusable result: %v", err)
		return c.JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Invalid result"})
	}

	var ledger models.C2BTransaction
	if err := config.DB.Where("verify_conversation_id = ? AND status = ?", result.OriginatorConversationID, "verifying").
		First(&ledger).Error; err != nil {
		log.Printf("[C2B Verify] No confirmation awaiting conversation %s (duplicate or unknown)", result.OriginatorConversationID)
		return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Accepted"})
	}

	if ok, why := result.Matches(&ledger); !ok {
		services.ParkC2B(&ledger, "Safaricom's record did not match the confirmation: "+why)
		return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Accepted"})
	}
	ledger.Verification = "status_query"
	creditC2B(&ledger)
	return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Accepted"})
}

// MpesaC2BVerifyTimeout is Safaricom saying the status check timed out in its
// queue. The payment is parked for manual review.
func MpesaC2BVerifyTimeout(c *fiber.Ctx) error {
	if !mpesaCallbackAuthorized(c) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Unauthorized"})
	}
	var payload map[string]interface{}
	_ = c.BodyParser(&payload)
	result, _ := payload["Result"].(map[string]interface{})
	conv, _ := result["OriginatorConversationID"].(string)
	var ledger models.C2BTransaction
	if conv != "" && config.DB.Where("verify_conversation_id = ? AND status = ?", conv, "verifying").First(&ledger).Error == nil {
		services.ParkC2B(&ledger, "Safaricom's status check timed out — check the M-Pesa statement before crediting")
	}
	return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Accepted"})
}

// c2bPayerName builds "Jane Wanjiku Doe" from the confirmation's name parts.
func c2bPayerName(l *models.C2BTransaction) string {
	var parts []string
	for _, p := range []string{l.FirstName, l.MiddleName, l.LastName} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, strings.Title(strings.ToLower(p)))
		}
	}
	return strings.Join(parts, " ")
}

// creditC2B applies a paybill payment that is proven genuine (callback token
// or Safaricom's own status check). The ledger row is moved to "credited" in
// the same transaction as the balance change, so it happens at most once.
func creditC2B(l *models.C2BTransaction) {
	cleanPhone := utils.FormatPhone(l.MSISDN)
	payerName := c2bPayerName(l)

	// Which ISP does this payment belong to? A payment to an ISP's own
	// shortcode is that ISP's, so only their customers can match. On the
	// shared platform paybill any ISP's customer could, so a match there must
	// be unique — otherwise it's queued for manual reconciliation.
	orgID, scoped := services.OrgForShortcode(l.BusinessShortCode)

	var customer *models.Customer
	matched, ambiguous := matchC2BCustomer(orgID, scoped, l.BillRefNumber, l.MSISDN)
	if matched != nil {
		customer = matched
	} else if !ambiguous && scoped {
		if created, ok := autoCreateC2BCustomer(orgID, cleanPhone, payerName, l.Amount); ok {
			customer = created
		}
	}

	claim := func(tx *gorm.DB) (bool, error) {
		res := tx.Model(&models.C2BTransaction{}).
			Where("id = ? AND status IN ?", l.ID, []string{"received", "verifying"}).
			Updates(map[string]interface{}{"status": "credited", "verification": l.Verification})
		return res.RowsAffected == 1, res.Error
	}

	if customer == nil {
		// Genuine money we can't place: queue it (as verified) for staff.
		err := config.DB.Transaction(func(tx *gorm.DB) error {
			if ok, err := claim(tx); err != nil || !ok {
				return err
			}
			return tx.Create(&models.UnmatchedC2BPayment{
				TransID:           l.TransID,
				Phone:             cleanPhone,
				Amount:            l.Amount,
				BillRefNumber:     l.BillRefNumber,
				BusinessShortCode: l.BusinessShortCode,
				FirstName:         l.FirstName,
				LastName:          strings.TrimSpace(l.MiddleName + " " + l.LastName),
				Status:            "pending",
				Verification:      "verified",
				Note:              "Verified by " + l.Verification + "; no unique customer match",
			}).Error
		})
		if err != nil {
			log.Printf("[C2B Confirmation] Failed to queue unmatched payment %s: %v", l.TransID, err)
		}
		return
	}

	// Always update customer name from official M-Pesa C2B legal names
	if payerName != "" {
		customer.Name = payerName
		config.DB.Model(customer).Update("name", payerName)
	}

	var (
		pkg     models.Package
		renewed bool
		payment models.Payment
		done    bool
		via     = c2bCollectionChannel(scoped) // reads settings: keep it out of the transaction
	)
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		ok, err := claim(tx)
		if err != nil || !ok {
			return err
		}
		if _, err := services.AdjustCredit(tx, customer.ID, l.Amount); err != nil {
			return err
		}
		noteStr := fmt.Sprintf("M-Pesa C2B Paybill %s", l.TransID)
		if err := tx.Create(&models.CreditLog{CustomerID: customer.ID, Amount: l.Amount, Type: "credit", Note: &noteStr}).Error; err != nil {
			return err
		}

		// 1. A package in this zone priced exactly at the amount paid (e.g.
		// KES 5), 2. otherwise the customer's assigned package.
		if err := tx.Where("zone_id = ? AND price = ? AND status = 'active'", customer.ZoneID, l.Amount).First(&pkg).Error; err != nil {
			tx.First(&pkg, customer.PackageID)
		}
		if pkg.ID > 0 && pkg.Price > 0 {
			// Debit only if the balance (read by the database, not from a
			// stale copy) covers it.
			paid, err := services.AdjustCredit(tx, customer.ID, -pkg.Price)
			if err != nil {
				return err
			}
			if paid {
				var fresh models.Customer
				tx.Select("id", "expires_at").First(&fresh, customer.ID)
				newExpiry := c2bNewExpiry(&pkg, fresh.ExpiresAt)
				if err := tx.Model(&models.Customer{}).Where("id = ?", customer.ID).Updates(map[string]interface{}{
					"package_id": pkg.ID,
					"expires_at": newExpiry,
					"status":     "active",
				}).Error; err != nil {
					return err
				}
				renewed = true
			}
		}

		packageID := customer.PackageID
		if renewed {
			packageID = pkg.ID
		}
		transID := l.TransID
		payment = models.Payment{
			CustomerID:         &customer.ID,
			ZoneID:             customer.ZoneID,
			PackageID:          &packageID,
			Phone:              cleanPhone,
			Amount:             l.Amount,
			Currency:           "KES",
			Method:             "mpesa_c2b",
			Status:             "completed",
			MpesaReceiptNumber: &transID,
			MpesaTransactionID: &transID,
			CollectedVia:       via,
		}
		if err := tx.Create(&payment).Error; err != nil {
			return err
		}
		done = true
		return tx.Model(&models.C2BTransaction{}).Where("id = ?", l.ID).Update("payment_id", payment.ID).Error
	})
	if err != nil {
		log.Printf("[C2B Confirmation] NOT CREDITED %s (KES %.2f) for customer %d: %v", l.TransID, l.Amount, customer.ID, err)
		return
	}
	if !done {
		return // another delivery got there first
	}

	if renewed && mpesaSvcGlobal != nil && customer.ZoneID > 0 {
		// Unblock the PPPoE secret / hotspot MAC — persisted and retried.
		cid := customer.ID
		mpesaSvcGlobal.EnqueueRouterAction(models.PendingRouterAction{
			ZoneID: customer.ZoneID, PaymentID: &payment.ID, Action: services.RouterActionReactivate, CustomerID: &cid,
		})
	}
	if smsSvcGlobal != nil && customer.Phone != "" {
		smsMsg := fmt.Sprintf("Payment Received: KES %.2f via M-Pesa (%s). Account balance: KES %.2f.", l.Amount, l.TransID, services.CreditBalanceOf(config.DB, customer.ID))
		go smsSvcGlobal.SendForZone(customer.ZoneID, customer.Phone, smsMsg)
	}
	if radiusSvcGlobal != nil {
		radiusSvcGlobal.SyncCustomerAsync(customer.ID)
	}
}

// c2bNewExpiry extends an unexpired subscription, or starts a new one now.
func c2bNewExpiry(pkg *models.Package, current *time.Time) time.Time {
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
	d := time.Duration(durationMinutes) * time.Minute
	if current != nil && current.After(time.Now()) {
		return current.Add(d)
	}
	return time.Now().Add(d)
}
