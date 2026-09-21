package handlers

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
)

var payoutSvcGlobal *services.PayoutService

// InitPayoutService injects the payout service.
func InitPayoutService(p *services.PayoutService) { payoutSvcGlobal = p }

func platformFloat(key string) float64 {
	f, _ := strconv.ParseFloat(GetPlatformSetting(key), 64)
	return f
}

// payoutError maps engine errors to HTTP responses. The current payout (if
// any) is returned so the UI can show the state it was left in.
func payoutError(c *fiber.Ctx, err error, payout *models.Payout) error {
	status := fiber.StatusBadGateway
	switch {
	case errors.Is(err, services.ErrPayoutNotInState), errors.Is(err, services.ErrPayoutOpen):
		status = fiber.StatusConflict
	case errors.Is(err, services.ErrPayoutNoDestination), errors.Is(err, services.ErrPayoutNothingOwed),
		errors.Is(err, services.ErrPayoutBelowMinimum), errors.Is(err, services.ErrPayoutB2BNotConfigured),
		errors.Is(err, services.ErrPayoutCallbackSecretMissing):
		status = fiber.StatusUnprocessableEntity
	}
	body := fiber.Map{"success": false, "error": err.Error(), "message": err.Error()}
	if payout != nil {
		body["payout"] = payout
	}
	return c.Status(status).JSON(body)
}

// requireSecondApprover enforces the optional two-person rule: whoever
// created a payout may not be the one who sends it or marks it paid.
func requireSecondApprover(staffID uint, p *models.Payout) bool {
	return GetPlatformSetting("payouts_require_second_approver") != "yes" || p.CreatedByPlatformUserID != staffID
}

func loadPayout(c *fiber.Ctx) (*models.Payout, error) {
	var p models.Payout
	if err := config.DB.Preload("Organization").First(&p, c.Params("id")).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// PlatformPayoutBalances lists, per ISP paid via the shared shortcode, what is
// owed (payments collected and not yet claimed by a payout), priced with the
// commission, and where it would be sent.
func PlatformPayoutBalances(c *fiber.Ctx) error {
	type agg struct {
		OrgID uint
		Cnt   int64
		Gross float64
	}
	var aggs []agg
	config.DB.Table("payments p").
		Select("z.organization_id AS org_id, COUNT(p.id) AS cnt, COALESCE(SUM(p.amount), 0) AS gross").
		Joins("JOIN zones z ON z.id = p.zone_id").
		Where("p.status = ? AND p.collected_via = ? AND p.payout_id IS NULL", "completed", "platform").
		Group("z.organization_id").Scan(&aggs)
	owed := map[uint]agg{}
	for _, a := range aggs {
		owed[a.OrgID] = a
	}

	var ownOrgs []uint
	config.DB.Model(&models.OrganizationMpesaConfig{}).Where("mode = ?", "own").Pluck("organization_id", &ownOrgs)
	isOwn := map[uint]bool{}
	for _, id := range ownOrgs {
		isOwn[id] = true
	}

	var open []models.Payout
	config.DB.Where("status IN ?", []string{"pending", "processing"}).Find(&open)
	openBy := map[uint]models.Payout{}
	for _, p := range open {
		openBy[p.OrganizationID] = p
	}

	var orgs []models.Organization
	config.DB.Order("name ASC").Find(&orgs)
	defaultPct := platformFloat("default_commission_percent")

	rows := make([]fiber.Map, 0, len(orgs))
	for i := range orgs {
		org := &orgs[i]
		a := owed[org.ID]
		if isOwn[org.ID] && a.Cnt == 0 {
			continue // its customers pay the ISP directly; nothing to settle
		}
		pct := services.EffectiveCommission(org, defaultPct)
		commission, net, pay := services.Price(a.Gross, pct)
		_, till, paybill, account, derr := services.Destination(org)
		dest := ""
		switch {
		case till != "":
			dest = "Till " + till
		case paybill != "":
			dest = "Paybill " + paybill + " / " + account
		}
		row := fiber.Map{
			"organization_id": org.ID, "organization": org.Name, "slug": org.Slug,
			"payment_count": a.Cnt, "gross": a.Gross, "commission_percent": pct,
			"commission": commission, "net": net, "payout_amount": pay,
			"destination": dest, "destination_ready": derr == nil,
		}
		if p, ok := openBy[org.ID]; ok {
			row["open_payout_id"], row["open_payout_status"] = p.ID, p.Status
		}
		rows = append(rows, row)
	}
	return utils.SuccessResponse(c, fiber.Map{
		"balances":        rows,
		"payouts_enabled": GetPlatformSetting("payouts_enabled") == "yes",
	}, "")
}

// PlatformPayoutIndex lists payouts, newest first (?organization_id, ?status).
func PlatformPayoutIndex(c *fiber.Ctx) error {
	q := config.DB.Preload("Organization").Order("id DESC").Limit(200)
	if id := c.QueryInt("organization_id", 0); id > 0 {
		q = q.Where("organization_id = ?", id)
	}
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	var payouts []models.Payout
	q.Find(&payouts)
	return utils.SuccessResponse(c, payouts, "")
}

// PlatformPayoutShow returns one payout with the payments it covers.
func PlatformPayoutShow(c *fiber.Ctx) error {
	p, err := loadPayout(c)
	if err != nil {
		return utils.ErrorResponse(c, "Payout not found.", "", fiber.StatusNotFound)
	}
	var payments []models.Payment
	config.DB.Select("id", "zone_id", "phone", "amount", "mpesa_receipt_number", "created_at").
		Where("payout_id = ?", p.ID).Order("id ASC").Limit(1000).Find(&payments)
	return utils.SuccessResponse(c, fiber.Map{"payout": p, "payments": payments}, "")
}

// PlatformPayoutStore claims everything owed to an ISP into a new pending payout.
func PlatformPayoutStore(c *fiber.Ctx) error {
	var body struct {
		OrganizationID uint `json:"organization_id"`
	}
	if err := c.BodyParser(&body); err != nil || body.OrganizationID == 0 {
		return utils.ErrorResponse(c, "organization_id is required.", "", fiber.StatusUnprocessableEntity)
	}
	staff := middleware.GetClaims(c).PlatformUserID
	min := int64(platformFloat("payout_min_amount"))
	p, err := payoutSvcGlobal.CreatePayout(body.OrganizationID, staff, platformFloat("default_commission_percent"), min)
	if err != nil {
		return payoutError(c, err, nil)
	}
	log.Printf("[payout %d] created for org %d by platform user %d: KES %d (%d payments)", p.ID, p.OrganizationID, staff, p.PayoutAmount, p.PaymentCount)
	return utils.SuccessResponse(c, p, "Payout created.", fiber.StatusCreated)
}

// PlatformPayoutSend sends a pending payout via M-Pesa B2B.
func PlatformPayoutSend(c *fiber.Ctx) error {
	p, err := loadPayout(c)
	if err != nil {
		return utils.ErrorResponse(c, "Payout not found.", "", fiber.StatusNotFound)
	}
	if GetPlatformSetting("payouts_enabled") != "yes" {
		return utils.ErrorResponse(c, "Sending payouts through M-Pesa is switched off. Enable it in platform settings first.", "", fiber.StatusForbidden)
	}
	staff := middleware.GetClaims(c).PlatformUserID
	if !requireSecondApprover(staff, p) {
		return utils.ErrorResponse(c, "A different staff member must send this payout.", "", fiber.StatusForbidden)
	}
	log.Printf("[payout %d] send requested by platform user %d", p.ID, staff)
	sent, err := payoutSvcGlobal.SendB2B(p.ID, staff)
	if err != nil {
		return payoutError(c, err, sent)
	}
	return utils.SuccessResponse(c, sent, "Payout sent to Safaricom. It completes when Safaricom confirms.")
}

// PlatformPayoutMarkPaid completes a payout that was paid outside the system,
// or confirms a stuck one against the M-Pesa statement.
func PlatformPayoutMarkPaid(c *fiber.Ctx) error {
	p, err := loadPayout(c)
	if err != nil {
		return utils.ErrorResponse(c, "Payout not found.", "", fiber.StatusNotFound)
	}
	var body struct {
		Reference string `json:"reference"`
	}
	_ = c.BodyParser(&body)
	staff := middleware.GetClaims(c).PlatformUserID
	if !requireSecondApprover(staff, p) {
		return utils.ErrorResponse(c, "A different staff member must confirm this payout.", "", fiber.StatusForbidden)
	}
	if err := payoutSvcGlobal.MarkPaid(p.ID, staff, body.Reference); err != nil {
		if errors.Is(err, services.ErrPayoutNotInState) {
			return payoutError(c, err, nil)
		}
		return utils.ErrorResponse(c, err.Error(), "", fiber.StatusUnprocessableEntity)
	}
	log.Printf("[payout %d] marked paid by platform user %d (ref %q)", p.ID, staff, body.Reference)
	config.DB.Preload("Organization").First(p, p.ID)
	return utils.SuccessResponse(c, p, "Payout marked as paid.")
}

// PlatformPayoutCancel abandons a pending payout, releasing its payments.
func PlatformPayoutCancel(c *fiber.Ctx) error {
	p, err := loadPayout(c)
	if err != nil {
		return utils.ErrorResponse(c, "Payout not found.", "", fiber.StatusNotFound)
	}
	if err := payoutSvcGlobal.Cancel(p.ID); err != nil {
		return payoutError(c, err, nil)
	}
	log.Printf("[payout %d] cancelled by platform user %d", p.ID, middleware.GetClaims(c).PlatformUserID)
	return utils.SuccessResponse(c, nil, "Payout cancelled. Its payments are back in the ISP's balance.")
}

// PlatformPayoutFail declares a stuck "processing" payout as not paid.
func PlatformPayoutFail(c *fiber.Ctx) error {
	p, err := loadPayout(c)
	if err != nil {
		return utils.ErrorResponse(c, "Payout not found.", "", fiber.StatusNotFound)
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.BodyParser(&body)
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "Marked as not paid by platform staff."
	}
	if err := payoutSvcGlobal.Fail(p.ID, reason); err != nil {
		return payoutError(c, err, nil)
	}
	log.Printf("[payout %d] marked failed by platform user %d: %s", p.ID, middleware.GetClaims(c).PlatformUserID, reason)
	return utils.SuccessResponse(c, nil, "Payout marked as failed. Its payments are back in the ISP's balance.")
}

// PlatformPayoutBackfill brings an ISP's *historical* shared-shortcode
// payments into the ledger. Payments completed before payouts existed have no
// collected_via, and are deliberately left out (they may already have been
// paid manually); this lets staff opt an ISP in from an explicit date.
func PlatformPayoutBackfill(c *fiber.Ctx) error {
	var body struct {
		OrganizationID uint   `json:"organization_id"`
		From           string `json:"from"` // YYYY-MM-DD
	}
	if err := c.BodyParser(&body); err != nil || body.OrganizationID == 0 {
		return utils.ErrorResponse(c, "organization_id and from (YYYY-MM-DD) are required.", "", fiber.StatusUnprocessableEntity)
	}
	from, err := time.Parse("2006-01-02", body.From)
	if err != nil {
		return utils.ErrorResponse(c, "from must be a date like 2026-09-01.", "", fiber.StatusUnprocessableEntity)
	}
	var cfg models.OrganizationMpesaConfig
	if config.DB.Where("organization_id = ? AND mode = ?", body.OrganizationID, "own").First(&cfg).Error == nil {
		return utils.ErrorResponse(c, "This ISP collects on its own Daraja app; there is nothing to settle.", "", fiber.StatusUnprocessableEntity)
	}

	zoneIDs := config.DB.Unscoped().Model(&models.Zone{}).Select("id").Where("organization_id = ?", body.OrganizationID)
	scope := func() *gorm.DB {
		return config.DB.Model(&models.Payment{}).Where(
			"status = ? AND method IN ? AND (collected_via = '' OR collected_via IS NULL) AND payout_id IS NULL AND created_at >= ? AND zone_id IN (?) AND (mpesa_receipt_number IS NULL OR mpesa_receipt_number NOT LIKE ?)",
			"completed", []string{"mpesa", "mpesa_c2b"}, from, zoneIDs, "MOCK%")
	}
	var count int64
	var gross float64
	scope().Count(&count)
	scope().Select("COALESCE(SUM(amount), 0)").Scan(&gross)
	if err := scope().Update("collected_via", "platform").Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Backfill failed.", fiber.StatusInternalServerError)
	}
	log.Printf("[payout] backfill by platform user %d: org %d from %s — %d payments, KES %.2f", middleware.GetClaims(c).PlatformUserID, body.OrganizationID, body.From, count, gross)
	return utils.SuccessResponse(c, fiber.Map{"payments": count, "gross": gross}, "Payments added to the ISP's balance.")
}

// PayoutB2BResult / PayoutB2BTimeout receive Safaricom's asynchronous B2B
// callbacks. They're public but guarded by the shared callback secret.
func PayoutB2BResult(c *fiber.Ctx) error {
	// Unlike the STK callback, this endpoint decides whether real money is
	// recorded as paid, so with no secret configured it refuses everything
	// instead of accepting anyone.
	if config.Config.MpesaCallbackSecret == "" || !mpesaCallbackAuthorized(c) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Unauthorized"})
	}
	var payload map[string]interface{}
	if err := c.BodyParser(&payload); err != nil {
		return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Accepted"})
	}
	if err := payoutSvcGlobal.HandleB2BResult(payload); err != nil {
		log.Printf("[payout] B2B result not applied: %v", err)
	}
	return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Accepted"})
}

func PayoutB2BTimeout(c *fiber.Ctx) error {
	if config.Config.MpesaCallbackSecret == "" || !mpesaCallbackAuthorized(c) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"ResultCode": 1, "ResultDesc": "Unauthorized"})
	}
	var payload map[string]interface{}
	if err := c.BodyParser(&payload); err == nil {
		payoutSvcGlobal.HandleB2BTimeout(payload)
	}
	return c.JSON(fiber.Map{"ResultCode": 0, "ResultDesc": "Accepted"})
}

// OrganizationPayoutsIndex is the ISP's own read-only view: what it is owed,
// where it will be sent, and its payout history. Only its own data, and none
// of the platform's internal identifiers.
func OrganizationPayoutsIndex(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var org models.Organization
	if err := config.DB.First(&org, claims.OrganizationID).Error; err != nil {
		return utils.ErrorResponse(c, "Organization not found.", "", fiber.StatusNotFound)
	}
	var cfg models.OrganizationMpesaConfig
	own := config.DB.Where("organization_id = ? AND mode = ?", org.ID, "own").First(&cfg).Error == nil

	count, gross := services.Unsettled(org.ID)
	pct := services.EffectiveCommission(&org, platformFloat("default_commission_percent"))
	commission, net, pay := services.Price(gross, pct)
	_, till, paybill, account, derr := services.Destination(&org)

	var payouts []models.Payout
	config.DB.Where("organization_id = ?", org.ID).Order("id DESC").Limit(100).Find(&payouts)
	history := make([]fiber.Map, 0, len(payouts))
	for _, p := range payouts {
		history = append(history, fiber.Map{
			"id": p.ID, "status": p.Status, "gross_amount": p.GrossAmount,
			"commission_amount": p.CommissionAmount, "payout_amount": p.PayoutAmount,
			"payment_count": p.PaymentCount, "reference": p.Reference,
			"created_at": p.CreatedAt, "completed_at": p.CompletedAt,
		})
	}
	return utils.SuccessResponse(c, fiber.Map{
		"own_daraja": own,
		"balance": fiber.Map{
			"payment_count": count, "gross": gross, "commission_percent": pct,
			"commission": commission, "net": net, "payout_amount": pay,
		},
		"destination": fiber.Map{"type": org.SettlementType, "till": till, "paybill": paybill, "account": account, "ready": derr == nil},
		"payouts":     history,
	}, "")
}
