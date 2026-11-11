package handlers

import (
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
)

// Home accounts (houses on fibre): admin "Add a home", switch off / on, the
// payment card, and the public renew page's API. Money rules live in
// services/home.go.

// homeMaxAmount keeps one renew-page prompt within what M-Pesa accepts.
const homeMaxAmount = 150000

var kePhoneRe = regexp.MustCompile(`^254(7|1)\d{8}$`)

// normalizeKEPhone turns 07…, 7…, +254…, 254… into 2547…/2541…; "" if invalid.
func normalizeKEPhone(raw string) string {
	p := utils.FormatPhone(strings.TrimSpace(raw))
	if !kePhoneRe.MatchString(p) {
		return ""
	}
	return p
}

// maskName keeps the first two letters of the first name and the first
// letter of the others: "Jane Otieno" → "Ja*** O***".
func maskName(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "Customer"
	}
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		keep := 1
		if i == 0 {
			keep = 2
		}
		if utf8.RuneCountInString(p) <= keep {
			keep = 1
		}
		r := []rune(p)
		out = append(out, string(r[:keep])+"***")
	}
	return strings.Join(out, " ")
}

// homeState is what the owner and the customer see: on, off (unpaid) or
// suspended (switched off by staff).
func homeState(c *models.Customer, now time.Time) string {
	switch {
	case c.Status == "suspended":
		return "suspended"
	case c.Status == "active" && c.ExpiresAt != nil && c.ExpiresAt.After(now):
		return "on"
	default:
		return "off"
	}
}

// homePayInfo is how a home pays by M-Pesa without a prompt: Paybill +
// account (their customer number), or a till (no account possible — the
// payer's phone number is matched instead).
func homePayInfo(zoneID uint, customerNo *uint) fiber.Map {
	account := ""
	if customerNo != nil {
		account = strconv.FormatUint(uint64(*customerNo), 10)
	}
	if mpesaSvcGlobal == nil {
		return fiber.Map{"type": "", "number": "", "account": account}
	}
	billingType, paybill, till := mpesaSvcGlobal.GetPaymentInfo(zoneID)
	if billingType == "till" && till != "" {
		return fiber.Map{"type": "till", "number": till, "account": ""}
	}
	return fiber.Map{"type": "paybill", "number": paybill, "account": account}
}

// homePlan describes a plan for pricing a renewal.
func homePlan(pkg *models.Package) fiber.Map {
	if pkg == nil {
		return nil
	}
	unit, count := services.HomePeriod(pkg)
	return fiber.Map{
		"id": pkg.ID, "name": pkg.Name, "price": pkg.Price,
		"period_unit": unit, "period_count": count,
		"speed_down_kbps": pkg.SpeedDownloadKbps, "speed_up_kbps": pkg.SpeedUploadKbps,
	}
}

// homeRenewOptions are the quick choices on the renew page, as multiples of
// the plan's own period (a daily plan: 1, 7, 30 periods = 1, 7, 30 days).
func homeRenewOptions(pkg *models.Package) []fiber.Map {
	if pkg == nil || pkg.Price <= 0 {
		return []fiber.Map{}
	}
	unit, _ := services.HomePeriod(pkg)
	choices := map[string][]int{
		"minute": {1, 2, 4}, "hour": {1, 6, 24}, "day": {1, 7, 30}, "week": {1, 2, 4}, "month": {1, 3, 6},
	}[unit]
	out := []fiber.Map{}
	for _, n := range choices {
		amount := float64(int64(n)*int64(pkg.Price*100+0.5)) / 100
		if amount > homeMaxAmount {
			continue
		}
		out = append(out, fiber.Map{"periods": n, "amount": amount})
	}
	return out
}

func eatStamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return utils.Kenya(*t).Format("Mon 2 Jan 2006, 15:04")
}

// ---------------- public renew page ----------------

// homeByNo finds a home account by the customer number typed on the renew page.
func homeByNo(raw string) (*models.Customer, bool) {
	if u, err := url.PathUnescape(raw); err == nil {
		raw = u // "zyr%20%2310017" → "zyr #10017"
	}
	no, ok := models.ParseCustomerNo(raw)
	if !ok {
		return nil, false
	}
	var c models.Customer
	if err := config.DB.Preload("Package", func(db *gorm.DB) *gorm.DB { return db.Unscoped() }).
		Where("customer_no = ? AND type = ?", no, "pppoe").First(&c).Error; err != nil {
		return nil, false
	}
	return &c, true
}

func homeNotFound(c *fiber.Ctx) error {
	return utils.ErrorResponse(c, "We couldn't find a home account with that number. Check the number on your payment card.", "Not found.", fiber.StatusNotFound)
}

// HomePublicLookup — GET /public/home/:no. Only what a payer needs: a masked
// name (so they know it's the right house), the plan, paid-until and how to
// pay. No phone, no full name, no balance.
func HomePublicLookup(c *fiber.Ctx) error {
	cust, ok := homeByNo(c.Params("no"))
	if !ok {
		return homeNotFound(c)
	}
	now := time.Now()
	return utils.SuccessResponse(c, fiber.Map{
		"customer_no":    cust.CustomerNo,
		"name":           maskName(cust.Name),
		"plan":           homePlan(cust.Package),
		"options":        homeRenewOptions(cust.Package),
		"max_periods":    services.HomeMaxPeriods,
		"status":         homeState(cust, now),
		"paid_until":     cust.ExpiresAt,
		"paid_until_eat": eatStamp(cust.ExpiresAt),
		"pay":            homePayInfo(cust.ZoneID, cust.CustomerNo),
	}, "")
}

// HomePublicPay — POST /public/home/:no/pay {phone, periods}. Sends an M-Pesa
// prompt for price × periods through the zone's payment provider; when it is
// paid the success path buys the periods (services.processHomeRenewal).
func HomePublicPay(c *fiber.Ctx) error {
	cust, ok := homeByNo(c.Params("no"))
	if !ok {
		return homeNotFound(c)
	}
	var body struct {
		Phone   string `json:"phone"`
		Periods int    `json:"periods"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	phone := normalizeKEPhone(body.Phone)
	if phone == "" {
		return utils.ErrorResponse(c, "Enter a Safaricom number like 0712 345 678.", "Invalid phone.", fiber.StatusUnprocessableEntity)
	}
	if body.Periods < 1 || body.Periods > services.HomeMaxPeriods {
		return utils.ErrorResponse(c, fmt.Sprintf("Choose between 1 and %d.", services.HomeMaxPeriods), "Invalid periods.", fiber.StatusUnprocessableEntity)
	}
	if cust.Status == "suspended" {
		return utils.ErrorResponse(c, "This connection was switched off by your internet provider. Please call them before paying.", "Suspended.", fiber.StatusConflict)
	}
	pkg := cust.Package
	if pkg == nil || pkg.Price <= 0 {
		return utils.ErrorResponse(c, "This home has no plan yet. Please call your internet provider.", "No plan.", fiber.StatusConflict)
	}
	amount := float64(int64(body.Periods)*int64(pkg.Price*100+0.5)) / 100
	if amount > homeMaxAmount {
		return utils.ErrorResponse(c, "That is more than one M-Pesa payment allows. Choose fewer days.", "Too much.", fiber.StatusUnprocessableEntity)
	}
	if mpesaSvcGlobal == nil {
		return utils.ErrorResponse(c, "Payments are not available right now.", "", fiber.StatusServiceUnavailable)
	}
	// Same rule as every other STK checkout: no prompt while the zone's router is silent.
	if ok, why := services.ZoneAcceptingPayments(cust.ZoneID); !ok {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"success": false, "code": "zone_offline", "error": why})
	}

	unlock := mpesaSvcGlobal.LockSTKPhone(phone)
	defer unlock()
	if existing, ok := services.RecentPendingSTK(phone, "", cust.ZoneID, 90*time.Second); ok {
		var hr models.HomeRenewal
		if config.DB.Where("payment_id = ? AND customer_id = ?", existing.ID, cust.ID).First(&hr).Error == nil {
			return utils.SuccessResponse(c, fiber.Map{"payment_id": existing.ID, "reused": true,
				"message": "An M-Pesa prompt was already sent to this phone. Enter your PIN on it to finish paying."}, "")
		}
		return utils.ErrorResponse(c, "Another M-Pesa prompt was just sent to this phone. Finish or cancel it, then try again in a minute.", "Busy.", fiber.StatusConflict)
	}

	pkgID := pkg.ID
	payment := models.Payment{
		CustomerID: &cust.ID, ZoneID: cust.ZoneID, PackageID: &pkgID, Phone: phone,
		Amount: amount, Currency: "KES", Method: "mpesa", Status: "pending",
		IpAddress: c.IP(),
	}
	if err := config.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&payment).Error; err != nil {
			return err
		}
		return tx.Create(&models.HomeRenewal{PaymentID: payment.ID, CustomerID: cust.ID, Periods: body.Periods}).Error
	}); err != nil {
		return utils.ErrorResponse(c, "Could not start the payment. Please try again.", "", fiber.StatusInternalServerError)
	}

	start, err := mpesaSvcGlobal.StartSTK(&payment, fmt.Sprintf("Cust%d", cust.ID), "Home internet")
	if err != nil {
		failPendingPayment(payment.ID, err.Error())
		log.Printf("[homes] renew prompt for customer %d failed: %v", cust.ID, err)
		return utils.ErrorResponse(c, "M-Pesa didn't accept the request. Please try again, or pay by Paybill.", "M-Pesa error.", fiber.StatusBadGateway)
	}
	if start.IsMock {
		mpesaSvcGlobal.SimulateCallback(start.Reference, amount, phone)
	}
	return utils.SuccessResponse(c, fiber.Map{
		"payment_id": payment.ID,
		"amount":     amount,
		"provider":   start.Provider,
		"message":    "Check your phone and enter your M-Pesa PIN.",
	}, "")
}

// HomePublicPayStatus — GET /public/home/:no/pay/:id. Only renewals of this
// home can be read here.
func HomePublicPayStatus(c *fiber.Ctx) error {
	cust, ok := homeByNo(c.Params("no"))
	if !ok {
		return homeNotFound(c)
	}
	id, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil || id == 0 {
		return utils.ErrorResponse(c, "Payment not found.", "", fiber.StatusNotFound)
	}
	var hr models.HomeRenewal
	if err := config.DB.Where("payment_id = ? AND customer_id = ?", id, cust.ID).First(&hr).Error; err != nil {
		return utils.ErrorResponse(c, "Payment not found.", "", fiber.StatusNotFound)
	}
	var payment models.Payment
	if err := config.DB.First(&payment, id).Error; err != nil {
		return utils.ErrorResponse(c, "Payment not found.", "", fiber.StatusNotFound)
	}
	if payment.Status == "pending" && mpesaSvcGlobal != nil && time.Since(payment.CreatedAt) > 20*time.Second {
		// Callback late? Ask M-Pesa (throttled inside).
		if _, err := mpesaSvcGlobal.QueryAndUpdateSTKStatus(&payment); err == nil {
			config.DB.First(&payment, id)
		}
	}
	out := fiber.Map{"status": payment.Status, "amount": payment.Amount, "periods": hr.Periods}
	switch payment.Status {
	case "completed":
		var fresh models.Customer
		config.DB.Select("id", "status", "expires_at").First(&fresh, cust.ID)
		out["paid_until"] = fresh.ExpiresAt
		out["paid_until_eat"] = eatStamp(fresh.ExpiresAt)
		out["home_status"] = homeState(&fresh, time.Now())
		out["message"] = "Paid. Your internet is on."
	case "failed":
		msg := "Payment failed. Please try again."
		if payment.StatusReason != nil && *payment.StatusReason != "" {
			msg = *payment.StatusReason
		}
		out["message"] = msg
	default:
		out["message"] = "Waiting for you to enter your M-Pesa PIN."
	}
	return utils.SuccessResponse(c, out, "")
}

// ---------------- admin ----------------

func homeStaffCanManage(c *fiber.Ctx) bool {
	claims := middleware.GetClaims(c)
	return claims != nil && (claims.Role == "super_admin" || claims.Role == "zone_manager")
}

// homeDetail is the admin view of one home.
func homeDetail(cust *models.Customer) fiber.Map {
	now := time.Now()
	return fiber.Map{
		"customer":       cust,
		"customer_no":    cust.CustomerNo,
		"state":          homeState(cust, now),
		"paid_until":     cust.ExpiresAt,
		"paid_until_eat": eatStamp(cust.ExpiresAt),
		"credit":         cust.CreditBalance,
		"plan":           homePlan(cust.Package),
		"pay":            homePayInfo(cust.ZoneID, cust.CustomerNo),
		"username":       services.HomeUsername(cust),
	}
}

// HomeStore — POST /homes: add a home (PPPoE) account. The PPPoE username is
// the customer number and the password is generated. A new home starts Off:
// it goes on with its first payment (Paybill, renew page, or credit + Switch on).
func HomeStore(c *fiber.Ctx) error {
	if !homeStaffCanManage(c) {
		return utils.ErrorResponse(c, "Only the owner or a zone manager can add homes.", "Forbidden.", fiber.StatusForbidden)
	}
	claims := middleware.GetClaims(c)
	var body struct {
		Name      string `json:"name"`
		Phone     string `json:"phone"`
		ZoneID    uint   `json:"zone_id"`
		PackageID uint   `json:"package_id"`
		Location  string `json:"location"`
		OntSerial string `json:"ont_serial"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 255 {
		return utils.ErrorResponse(c, "Enter the customer's name.", "", fiber.StatusUnprocessableEntity)
	}
	phone := normalizeKEPhone(body.Phone)
	if phone == "" {
		return utils.ErrorResponse(c, "Enter a valid Kenyan phone number (07… or 01…).", "", fiber.StatusUnprocessableEntity)
	}
	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, body.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "Choose one of your zones.", "", fiber.StatusUnprocessableEntity)
	}
	if claims.Role == "zone_manager" && claims.ZoneID != nil && zone.ID != *claims.ZoneID {
		return utils.ErrorResponse(c, "You can only add homes in your own zone.", "", fiber.StatusForbidden)
	}
	var pkg models.Package
	if err := config.DB.Where("zone_id = ? AND type = ? AND status = ?", zone.ID, "pppoe", "active").First(&pkg, body.PackageID).Error; err != nil {
		return utils.ErrorResponse(c, "Choose a home plan of this zone.", "", fiber.StatusUnprocessableEntity)
	}
	trimPtr := func(s string, max int) *string {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if len(s) > max {
			s = s[:max]
		}
		return &s
	}
	pw := generatePPPoEPassword()
	cust := models.Customer{
		Name: body.Name, Phone: phone, ZoneID: zone.ID, PackageID: pkg.ID, Type: "pppoe",
		Status: "expired", PPPoEPassword: &pw,
		Location: trimPtr(body.Location, 255), OntSerial: trimPtr(body.OntSerial, 64),
	}
	if err := config.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&cust).Error; err != nil {
			return err
		}
		if cust.CustomerNo == nil {
			return fmt.Errorf("no customer number assigned")
		}
		user := strconv.FormatUint(uint64(*cust.CustomerNo), 10)
		cust.PPPoEUsername = &user
		return tx.Model(&models.Customer{}).Where("id = ?", cust.ID).Update("pppoe_username", user).Error
	}); err != nil {
		return utils.ErrorResponse(c, "Could not add the home: "+err.Error(), "", fiber.StatusInternalServerError)
	}
	// RADIUS zones: rows follow the customer (none until paid). API zones:
	// the PPP secret is created on the router with the first payment
	// (ReactivateCustomer creates it when missing).
	if radiusSvcGlobal != nil {
		radiusSvcGlobal.SyncCustomerAsync(cust.ID)
	}
	config.DB.Preload("Zone").Preload("Package").First(&cust, cust.ID)
	return utils.SuccessResponse(c, homeDetail(&cust), "Home added.", fiber.StatusCreated)
}

// HomeShow — GET /homes/:id.
func HomeShow(c *fiber.Ctx) error {
	cust, err := findCustomerOrFail(c, "Zone", "Package")
	if err != nil {
		return err
	}
	if !customerInScope(c, cust) {
		return utils.ErrorResponse(c, "Unauthorized to view this customer.", "", fiber.StatusForbidden)
	}
	if !services.IsHome(cust) {
		return utils.ErrorResponse(c, "This customer is not a home account.", "", fiber.StatusUnprocessableEntity)
	}
	return utils.SuccessResponse(c, homeDetail(cust), "")
}

// homeSwitchRouter is what switch-off talks to (nil in tests without a router).
func homeSwitchRouter() services.HomeRouter {
	if mikrotikSvcGlobal == nil {
		return nil
	}
	return mikrotikSvcGlobal
}

// HomeSwitchOff — POST /homes/:id/switch-off: staff cut a home off now
// (status suspended). Payments then only add credit until it is switched on.
func HomeSwitchOff(c *fiber.Ctx) error {
	if !homeStaffCanManage(c) {
		return utils.ErrorResponse(c, "Only the owner or a zone manager can switch homes off.", "Forbidden.", fiber.StatusForbidden)
	}
	cust, err := findCustomerOrFail(c, "Zone", "Package")
	if err != nil {
		return err
	}
	if !customerInScope(c, cust) || !services.IsHome(cust) {
		return utils.ErrorResponse(c, "Home not found.", "", fiber.StatusNotFound)
	}
	if err := config.DB.Model(&models.Customer{}).Where("id = ?", cust.ID).Update("status", "suspended").Error; err != nil {
		return utils.ErrorResponse(c, "Could not switch off.", "", fiber.StatusInternalServerError)
	}
	now := time.Now()
	config.DB.Model(&models.Session{}).Where("customer_id = ? AND ended_at IS NULL", cust.ID).Update("ended_at", &now)
	cust.Status = "suspended"
	if cust.Zone != nil {
		z, cc := *cust.Zone, *cust
		go func() {
			if err := services.HomeSwitchOff(homeSwitchRouter(), &cc, &z); err != nil {
				log.Printf("[homes] switch off %d: %v", cc.ID, err)
			}
		}()
	}
	return utils.SuccessResponse(c, homeDetail(cust), "Switched off.")
}

// HomeSwitchOn — POST /homes/:id/switch-on: lift a staff switch-off. With paid
// time left the home goes straight back on; with none, the next period is
// bought from credit; with neither it stays off (no free internet by accident).
func HomeSwitchOn(c *fiber.Ctx) error {
	if !homeStaffCanManage(c) {
		return utils.ErrorResponse(c, "Only the owner or a zone manager can switch homes on.", "Forbidden.", fiber.StatusForbidden)
	}
	cust, err := findCustomerOrFail(c, "Zone", "Package")
	if err != nil {
		return err
	}
	if !customerInScope(c, cust) || !services.IsHome(cust) {
		return utils.ErrorResponse(c, "Home not found.", "", fiber.StatusNotFound)
	}
	now := time.Now()
	paidLeft := cust.ExpiresAt != nil && cust.ExpiresAt.After(now)
	var res services.HomeRenewResult
	if err := config.DB.Transaction(func(tx *gorm.DB) error {
		if paidLeft {
			return tx.Model(&models.Customer{}).Where("id = ?", cust.ID).Update("status", "active").Error
		}
		var err error
		res, err = services.RenewHomeFromCredit(tx, cust.ID, 1, "Switched on by staff, paid from credit")
		return err
	}); err != nil {
		return utils.ErrorResponse(c, "Could not switch on.", "", fiber.StatusInternalServerError)
	}
	if !paidLeft && res.Periods == 0 {
		price := 0.0
		if cust.Package != nil {
			price = cust.Package.Price
		}
		return utils.ErrorResponse(c, fmt.Sprintf("No paid time and not enough credit (KES %.0f, plan costs KES %.0f). Ask them to pay, or add credit first.", cust.CreditBalance, price), "Not paid.", fiber.StatusUnprocessableEntity)
	}
	if mpesaSvcGlobal != nil {
		mpesaSvcGlobal.HomeSwitchOn(cust.ID, cust.ZoneID, nil)
	}
	config.DB.Preload("Zone").Preload("Package").First(cust, cust.ID)
	return utils.SuccessResponse(c, homeDetail(cust), "Switched on.")
}
