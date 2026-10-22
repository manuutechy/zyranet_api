package handlers

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
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

// Agents (resellers) and their cash ledger. The money rules live in
// services/agent_ledger.go; these handlers do access control and shapes.
//
// Who can do what:
//   - super_admin (the ISP owner): everything — create agents, set
//     commission and credit limits, record handovers, adjust and reverse.
//   - zone_manager: view agents selling in their zone, issue vouchers to and
//     take returns from them (for their zone only).
//   - finance: view all agents and statements.
//   - field_agent: only their own agent profile and statement.

// isAgentOwner is the ISP role that controls agent money.
func isAgentOwner(role string) bool { return role == "super_admin" }

// agentActor is the acting user, from the session only.
func agentActor(c *fiber.Ctx) services.AgentActor {
	claims := middleware.GetClaims(c)
	return services.AgentActor{OrgID: claims.OrganizationID, UserID: claims.UserID}
}

// findAgentForCaller loads an agent of the caller's ISP the caller may see.
// Anything else is "not found", so IDs of other ISPs' agents reveal nothing.
func findAgentForCaller(c *fiber.Ctx) (*models.Agent, error) {
	claims := middleware.GetClaims(c)
	var agent models.Agent
	q := config.DB.Preload("Zones").Where("organization_id = ?", claims.OrganizationID)
	if err := q.First(&agent, c.Params("id")).Error; err != nil {
		return nil, utils.ErrorResponse(c, "Agent not found.", "", fiber.StatusNotFound)
	}
	if !agentVisible(claims, &agent) {
		return nil, utils.ErrorResponse(c, "Agent not found.", "", fiber.StatusNotFound)
	}
	return &agent, nil
}

func agentVisible(claims *middleware.Claims, agent *models.Agent) bool {
	switch claims.Role {
	case "super_admin", "finance":
		return true
	case "zone_manager":
		return claims.ZoneID != nil && agentInZone(agent, *claims.ZoneID)
	case "field_agent":
		return agent.UserID != nil && *agent.UserID == claims.UserID
	}
	return false
}

func agentInZone(agent *models.Agent, zoneID uint) bool {
	for _, z := range agent.Zones {
		if z.ID == zoneID {
			return true
		}
	}
	return false
}

// canIssueToAgent: the owner, or a zone manager for their own zone.
func canIssueToAgent(claims *middleware.Claims, agent *models.Agent, zoneID uint) bool {
	if isAgentOwner(claims.Role) {
		return true
	}
	return claims.Role == "zone_manager" && claims.ZoneID != nil && *claims.ZoneID == zoneID && agentInZone(agent, zoneID)
}

// ledgerFail turns a service error into a response.
func ledgerFail(c *fiber.Ctx, err error) error {
	var le *services.LedgerError
	if errors.As(err, &le) {
		return utils.ErrorResponse(c, le.Msg, "", le.Status)
	}
	return utils.ErrorResponse(c, "Something went wrong recording this. Nothing was saved.", err.Error(), fiber.StatusInternalServerError)
}

// idempotencyKey is the client's request key: the Idempotency-Key header, or
// idempotency_key in the body.
func idempotencyKey(c *fiber.Ctx, bodyKey string) string {
	if k := strings.TrimSpace(c.Get("Idempotency-Key")); k != "" {
		return k
	}
	return strings.TrimSpace(bodyKey)
}

func auditAgent(c *fiber.Ctx, action string, modelName string, id uint, values interface{}) {
	claims := middleware.GetClaims(c)
	b, _ := json.Marshal(values)
	s := string(b)
	config.DB.Create(&models.AuditLog{UserID: &claims.UserID, Action: action, Model: modelName, ModelID: id, NewValues: &s})
}

// weekStartEAT is Monday 00:00 East Africa Time of the current week.
func weekStartEAT(now time.Time) time.Time {
	k := utils.Kenya(now)
	offset := (int(k.Weekday()) + 6) % 7 // Monday = 0
	d := time.Date(k.Year(), k.Month(), k.Day(), 0, 0, 0, 0, utils.EAT)
	return d.AddDate(0, 0, -offset)
}

type agentWithSummary struct {
	models.Agent
	Summary *services.AgentSummary `json:"summary"`
}

func withSummaries(orgID uint, agents []models.Agent) ([]agentWithSummary, error) {
	ids := make([]uint, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	sums, err := services.AgentSummaries(config.DB, orgID, ids, weekStartEAT(time.Now()))
	if err != nil {
		return nil, err
	}
	out := make([]agentWithSummary, len(agents))
	for i, a := range agents {
		out[i] = agentWithSummary{Agent: a, Summary: sums[a.ID]}
	}
	return out, nil
}

// AgentIndex lists the caller's agents with what each owes, sold this week
// and their last handover.
func AgentIndex(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var agents []models.Agent
	q := config.DB.Preload("Zones").Where("organization_id = ?", claims.OrganizationID)
	switch claims.Role {
	case "super_admin", "finance":
	case "zone_manager":
		if claims.ZoneID == nil {
			return utils.SuccessResponse(c, []agentWithSummary{}, "")
		}
		q = q.Where("id IN (?)", config.DB.Table("agent_zones").Select("agent_id").Where("zone_id = ?", *claims.ZoneID))
	case "field_agent":
		q = q.Where("user_id = ?", claims.UserID)
	default:
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}
	if s := strings.TrimSpace(c.Query("search")); s != "" {
		q = q.Where("name LIKE ? OR phone LIKE ?", "%"+s+"%", "%"+s+"%")
	}
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	if err := q.Order("name").Find(&agents).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to load agents.", fiber.StatusInternalServerError)
	}
	out, err := withSummaries(claims.OrganizationID, agents)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to load agents.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, out, "")
}

// AgentMe returns the signed-in field agent's own agent profile.
func AgentMe(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var agent models.Agent
	if err := config.DB.Preload("Zones").Where("organization_id = ? AND user_id = ?", claims.OrganizationID, claims.UserID).
		First(&agent).Error; err != nil {
		return utils.ErrorResponse(c, "Your login isn't linked to an agent account.", "", fiber.StatusNotFound)
	}
	out, err := withSummaries(claims.OrganizationID, []models.Agent{agent})
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, out[0], "")
}

// AgentShow returns one agent with their summary.
func AgentShow(c *fiber.Ctx) error {
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	out, err := withSummaries(agent.OrganizationID, []models.Agent{*agent})
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, out[0], "")
}

type agentBody struct {
	Name             *string `json:"name"`
	Phone            *string `json:"phone"`
	ZoneIDs          *[]uint `json:"zone_ids"`
	CommissionType   *string `json:"commission_type"`
	CommissionValue  *int64  `json:"commission_value"`
	CreditLimitCents *int64  `json:"credit_limit_cents"`
	Status           *string `json:"status"`
	// UserID links a field_agent login; 0 unlinks.
	UserID *uint `json:"user_id"`
}

// applyAgentBody validates body onto agent and resolves its zones.
func applyAgentBody(orgID uint, agent *models.Agent, body agentBody) ([]models.Zone, string) {
	if body.Name != nil {
		agent.Name = strings.TrimSpace(*body.Name)
	}
	if body.Phone != nil {
		agent.Phone = utils.FormatPhone(strings.TrimSpace(*body.Phone))
	}
	if body.CommissionType != nil {
		agent.CommissionType = *body.CommissionType
	}
	if body.CommissionValue != nil {
		agent.CommissionValue = *body.CommissionValue
	}
	if body.CreditLimitCents != nil {
		agent.CreditLimitCents = *body.CreditLimitCents
	}
	if body.Status != nil {
		agent.Status = *body.Status
	}
	if agent.Name == "" || agent.Phone == "" {
		return nil, "Name and phone are required."
	}
	switch agent.CommissionType {
	case "percent":
		if agent.CommissionValue < 0 || agent.CommissionValue > 10000 {
			return nil, "Commission percent must be between 0 and 100."
		}
	case "fixed":
		if agent.CommissionValue < 0 {
			return nil, "Commission per voucher can't be negative."
		}
	default:
		return nil, "Commission type must be percent or fixed."
	}
	if agent.CreditLimitCents < 0 {
		return nil, "Credit limit can't be negative."
	}
	if agent.Status != "active" && agent.Status != "inactive" {
		return nil, "Status must be active or inactive."
	}
	if body.UserID != nil {
		if *body.UserID == 0 {
			agent.UserID = nil
		} else {
			var u models.User
			if err := config.DB.Where("id = ? AND organization_id = ? AND role = ?", *body.UserID, orgID, "field_agent").First(&u).Error; err != nil {
				return nil, "The login must be a field agent of your organization."
			}
			var n int64
			config.DB.Model(&models.Agent{}).Where("user_id = ? AND id <> ?", u.ID, agent.ID).Count(&n)
			if n > 0 {
				return nil, "That login is already linked to another agent."
			}
			agent.UserID = &u.ID
		}
	}
	if body.ZoneIDs == nil {
		return nil, ""
	}
	if len(*body.ZoneIDs) == 0 {
		return nil, "Pick at least one zone."
	}
	var zones []models.Zone
	config.DB.Where("id IN ? AND organization_id = ?", *body.ZoneIDs, orgID).Find(&zones)
	if len(zones) != len(dedupeUint(*body.ZoneIDs)) {
		return nil, "That zone does not belong to your organization."
	}
	return zones, ""
}

func dedupeUint(ids []uint) []uint {
	seen := map[uint]bool{}
	var out []uint
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// AgentStore creates an agent (owner only).
func AgentStore(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if !isAgentOwner(claims.Role) {
		return utils.ErrorResponse(c, "Only the account owner can add agents.", "Forbidden.", fiber.StatusForbidden)
	}
	var body agentBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if body.ZoneIDs == nil {
		return utils.ErrorResponse(c, "Pick at least one zone.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	agent := models.Agent{OrganizationID: claims.OrganizationID, CommissionType: "percent", Status: "active", CreatedBy: &claims.UserID}
	zones, msg := applyAgentBody(claims.OrganizationID, &agent, body)
	if msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	agent.Zones = zones
	if err := config.DB.Create(&agent).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create agent.", fiber.StatusInternalServerError)
	}
	auditAgent(c, "agent_created", "Agent", agent.ID, agent)
	return utils.SuccessResponse(c, agent, "Agent added.", fiber.StatusCreated)
}

// AgentUpdate changes an agent's profile, commission, credit limit, status
// or zones (owner only). New commission rates apply to vouchers issued from
// now on; vouchers already issued keep the rate they were issued at.
func AgentUpdate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if !isAgentOwner(claims.Role) {
		return utils.ErrorResponse(c, "Only the account owner can change agents.", "Forbidden.", fiber.StatusForbidden)
	}
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	before := *agent
	var body agentBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	zones, msg := applyAgentBody(claims.OrganizationID, agent, body)
	if msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	err = config.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.Agent{}).Where("id = ? AND organization_id = ?", agent.ID, claims.OrganizationID).
			Updates(map[string]interface{}{
				"name": agent.Name, "phone": agent.Phone, "commission_type": agent.CommissionType,
				"commission_value": agent.CommissionValue, "credit_limit_cents": agent.CreditLimitCents,
				"status": agent.Status, "user_id": agent.UserID,
			}).Error; err != nil {
			return err
		}
		if zones != nil {
			return tx.Model(&models.Agent{ID: agent.ID}).Association("Zones").Replace(zones)
		}
		return nil
	})
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zones").First(agent, agent.ID)
	auditAgent(c, "agent_updated", "Agent", agent.ID, fiber.Map{"before": before, "after": agent})
	return utils.SuccessResponse(c, agent, "Agent updated.")
}

// AgentVouchers lists the vouchers an agent holds or held.
func AgentVouchers(c *fiber.Ctx) error {
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	page, perPage := utils.ParsePage(c)
	q := config.DB.Model(&models.AgentVoucher{}).Preload("Voucher").Preload("Voucher.Package").
		Where("agent_id = ? AND organization_id = ?", agent.ID, agent.OrganizationID)
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	if b := c.QueryInt("batch_id"); b > 0 {
		q = q.Where("batch_id = ?", b)
	}
	var total int64
	q.Count(&total)
	var rows []models.AgentVoucher
	q.Order("id DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&rows)
	return utils.PaginatedResponse(c, rows, total, page, perPage)
}

// AgentBatches lists an agent's issued batches with how many are still unsold.
func AgentBatches(c *fiber.Ctx) error {
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	var batches []models.AgentVoucherBatch
	config.DB.Preload("Package").Preload("Zone").Where("agent_id = ? AND organization_id = ?", agent.ID, agent.OrganizationID).
		Order("id DESC").Limit(100).Find(&batches)
	type counts struct {
		BatchID uint
		Status  string
		N       int64
	}
	var cs []counts
	config.DB.Model(&models.AgentVoucher{}).Select("batch_id, status, COUNT(*) AS n").
		Where("agent_id = ?", agent.ID).Group("batch_id, status").Scan(&cs)
	type batchOut struct {
		models.AgentVoucherBatch
		Unsold   int64 `json:"unsold"`
		Sold     int64 `json:"sold"`
		Returned int64 `json:"returned"`
	}
	byID := map[uint]*batchOut{}
	out := make([]batchOut, len(batches))
	for i, b := range batches {
		out[i] = batchOut{AgentVoucherBatch: b}
		byID[b.ID] = &out[i]
	}
	for _, r := range cs {
		if b := byID[r.BatchID]; b != nil {
			switch r.Status {
			case "issued":
				b.Unsold = r.N
			case "sold":
				b.Sold = r.N
			case "returned":
				b.Returned = r.N
			}
		}
	}
	return utils.SuccessResponse(c, out, "")
}

// AgentIssue issues a batch of vouchers to an agent (owner, or the zone's
// manager). Needs an idempotency key: a repeat returns the first batch.
func AgentIssue(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	var body struct {
		ZoneID         uint   `json:"zone_id"`
		PackageID      uint   `json:"package_id"`
		Quantity       int    `json:"quantity"`
		Funding        string `json:"funding"`
		Method         string `json:"method"`
		Reference      string `json:"reference"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := c.BodyParser(&body); err != nil || body.ZoneID == 0 || body.PackageID == 0 {
		return utils.ErrorResponse(c, "zone_id and package_id are required.", "", fiber.StatusUnprocessableEntity)
	}
	if !canIssueToAgent(claims, agent, body.ZoneID) {
		return utils.ErrorResponse(c, "Unauthorized to issue vouchers to this agent.", "Forbidden.", fiber.StatusForbidden)
	}
	if body.Funding == "" {
		body.Funding = "credit"
	}
	res, err := services.IssueToAgent(config.DB, voucherSvcGlobal, agentActor(c), services.AgentIssueRequest{
		AgentID: agent.ID, ZoneID: body.ZoneID, PackageID: body.PackageID, Quantity: body.Quantity,
		Funding: body.Funding, Method: body.Method, Reference: body.Reference,
		Key: idempotencyKey(c, body.IdempotencyKey),
	})
	if err != nil {
		return ledgerFail(c, err)
	}
	if res.Replayed {
		return utils.SuccessResponse(c, fiber.Map{"batch": res.Batch, "vouchers": res.Vouchers, "entries": res.Entries, "replayed": true}, "Already issued.")
	}
	auditAgent(c, "agent_vouchers_issued", "AgentVoucherBatch", res.Batch.ID, res.Batch)
	return utils.SuccessResponse(c, fiber.Map{"batch": res.Batch, "vouchers": res.Vouchers, "entries": res.Entries},
		fmt.Sprintf("%d vouchers issued to %s.", res.Batch.Quantity, agent.Name), fiber.StatusCreated)
}

// AgentReturn takes back unsold vouchers, voiding them and reversing what the
// agent owed for them (owner, or the zone's manager).
func AgentReturn(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	var body struct {
		VoucherIDs     []uint `json:"voucher_ids"`
		BatchID        uint   `json:"batch_id"`
		Note           string `json:"note"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if !isAgentOwner(claims.Role) {
		// A zone manager may only take back vouchers of their own zone.
		if claims.Role != "zone_manager" || claims.ZoneID == nil || !agentInZone(agent, *claims.ZoneID) {
			return utils.ErrorResponse(c, "Unauthorized to take returns from this agent.", "Forbidden.", fiber.StatusForbidden)
		}
		var other int64
		q := config.DB.Model(&models.AgentVoucher{}).Joins("JOIN vouchers ON vouchers.id = agent_vouchers.voucher_id").
			Where("agent_vouchers.agent_id = ? AND vouchers.zone_id <> ?", agent.ID, *claims.ZoneID)
		if len(body.VoucherIDs) > 0 {
			q = q.Where("agent_vouchers.voucher_id IN ?", body.VoucherIDs)
		} else {
			q = q.Where("agent_vouchers.batch_id = ?", body.BatchID)
		}
		q.Count(&other)
		if other > 0 {
			return utils.ErrorResponse(c, "Some of those vouchers are for another zone.", "Forbidden.", fiber.StatusForbidden)
		}
	}
	entry, replay, err := services.ReturnAgentVouchers(config.DB, agentActor(c), services.AgentReturnRequest{
		AgentID: agent.ID, VoucherIDs: body.VoucherIDs, BatchID: body.BatchID, Note: body.Note,
		Key: idempotencyKey(c, body.IdempotencyKey),
	})
	if err != nil {
		return ledgerFail(c, err)
	}
	if replay {
		return utils.SuccessResponse(c, fiber.Map{"entry": entry, "replayed": true}, "Already returned.")
	}
	auditAgent(c, "agent_vouchers_returned", "AgentLedgerEntry", entry.ID, entry)
	return utils.SuccessResponse(c, fiber.Map{"entry": entry}, fmt.Sprintf("%d vouchers returned and voided.", entry.VoucherCount), fiber.StatusCreated)
}

// AgentHandoverStore records cash/M-Pesa received from an agent (owner only).
func AgentHandoverStore(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if !isAgentOwner(claims.Role) {
		return utils.ErrorResponse(c, "Only the account owner can confirm cash handovers.", "Forbidden.", fiber.StatusForbidden)
	}
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	var body struct {
		AmountCents    int64  `json:"amount_cents"`
		Method         string `json:"method"`
		Reference      string `json:"reference"`
		Note           string `json:"note"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	entry, replay, err := services.RecordHandover(config.DB, agentActor(c), services.AgentHandoverRequest{
		AgentID: agent.ID, AmountCents: body.AmountCents, Method: body.Method, Reference: body.Reference,
		Note: body.Note, Key: idempotencyKey(c, body.IdempotencyKey),
	})
	if err != nil {
		return ledgerFail(c, err)
	}
	if replay {
		return utils.SuccessResponse(c, fiber.Map{"entry": entry, "replayed": true}, "Already recorded.")
	}
	auditAgent(c, "agent_handover", "AgentLedgerEntry", entry.ID, entry)
	return utils.SuccessResponse(c, fiber.Map{"entry": entry}, "Handover recorded.", fiber.StatusCreated)
}

// AgentAdjustmentStore records an owner correction (owner only).
func AgentAdjustmentStore(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if !isAgentOwner(claims.Role) {
		return utils.ErrorResponse(c, "Only the account owner can adjust an agent's balance.", "Forbidden.", fiber.StatusForbidden)
	}
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	var body struct {
		AmountCents    int64  `json:"amount_cents"`
		Note           string `json:"note"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	entry, replay, err := services.RecordAdjustment(config.DB, agentActor(c), agent.ID, body.AmountCents, body.Note, idempotencyKey(c, body.IdempotencyKey))
	if err != nil {
		return ledgerFail(c, err)
	}
	if replay {
		return utils.SuccessResponse(c, fiber.Map{"entry": entry, "replayed": true}, "Already recorded.")
	}
	auditAgent(c, "agent_adjustment", "AgentLedgerEntry", entry.ID, entry)
	return utils.SuccessResponse(c, fiber.Map{"entry": entry}, "Adjustment recorded.", fiber.StatusCreated)
}

// AgentEntryReverse reverses a handover or adjustment (owner only).
func AgentEntryReverse(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if !isAgentOwner(claims.Role) {
		return utils.ErrorResponse(c, "Only the account owner can reverse entries.", "Forbidden.", fiber.StatusForbidden)
	}
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	entryID, _ := c.ParamsInt("entryId")
	var body struct {
		Note           string `json:"note"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	c.BodyParser(&body)
	entry, replay, err := services.ReverseEntry(config.DB, agentActor(c), agent.ID, uint(entryID), body.Note, idempotencyKey(c, body.IdempotencyKey))
	if err != nil {
		return ledgerFail(c, err)
	}
	if replay {
		return utils.SuccessResponse(c, fiber.Map{"entry": entry, "replayed": true}, "Already reversed.")
	}
	auditAgent(c, "agent_entry_reversed", "AgentLedgerEntry", entry.ID, entry)
	return utils.SuccessResponse(c, fiber.Map{"entry": entry}, "Entry reversed.", fiber.StatusCreated)
}

// AgentHandoverReceipt returns what a printed receipt for one handover shows.
func AgentHandoverReceipt(c *fiber.Ctx) error {
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	var e models.AgentLedgerEntry
	if err := config.DB.Preload("Creator", func(q *gorm.DB) *gorm.DB { return q.Select("id", "name") }).
		Where("id = ? AND agent_id = ? AND organization_id = ? AND type = ?", c.Params("entryId"), agent.ID, agent.OrganizationID, models.AgentEntryHandover).
		First(&e).Error; err != nil {
		return utils.ErrorResponse(c, "Receipt not found.", "", fiber.StatusNotFound)
	}
	var balanceAfter int64
	config.DB.Model(&models.AgentLedgerEntry{}).Where("agent_id = ? AND id <= ?", agent.ID, e.ID).
		Select("COALESCE(SUM(amount_cents),0)").Scan(&balanceAfter)
	var reversed int64
	config.DB.Model(&models.AgentLedgerEntry{}).Where("reverses_id = ?", e.ID).Count(&reversed)
	var org models.Organization
	config.DB.Select("id", "name", "contact_phone").First(&org, agent.OrganizationID)
	return utils.SuccessResponse(c, fiber.Map{
		"receipt_no":          fmt.Sprintf("AH-%06d", e.ID),
		"organization":        org.Name,
		"organization_phone":  org.ContactPhone,
		"agent":               fiber.Map{"id": agent.ID, "name": agent.Name, "phone": agent.Phone},
		"amount_cents":        -e.AmountCents,
		"method":              e.Method,
		"reference":           e.Reference,
		"note":                e.Note,
		"received_by":         e.Creator,
		"received_at":         e.CreatedAt,
		"balance_after_cents": balanceAfter,
		"reversed":            reversed > 0,
	}, "")
}

// parseStatementRange reads from/to (YYYY-MM-DD, East Africa dates, both
// inclusive). Default: the 1st of this month to today.
func parseStatementRange(c *fiber.Ctx) (time.Time, time.Time, error) {
	now := utils.Kenya(time.Now())
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, utils.EAT)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, utils.EAT)
	if s := c.Query("from"); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, utils.EAT)
		if err != nil {
			return from, to, err
		}
		from = t
	}
	if s := c.Query("to"); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, utils.EAT)
		if err != nil {
			return from, to, err
		}
		to = t
	}
	if to.Before(from) {
		return from, to, fmt.Errorf("to before from")
	}
	return from, to.AddDate(0, 0, 1), nil
}

// AgentStatementShow returns an agent's statement for a period, or a CSV
// file with ?format=csv.
func AgentStatementShow(c *fiber.Ctx) error {
	agent, err := findAgentForCaller(c)
	if agent == nil {
		return err
	}
	from, to, err := parseStatementRange(c)
	if err != nil {
		return utils.ErrorResponse(c, "Dates must be YYYY-MM-DD, with from on or before to.", "", fiber.StatusUnprocessableEntity)
	}
	st, err := services.BuildAgentStatement(config.DB, agent.OrganizationID, agent.ID, from, to)
	if err != nil {
		return utils.ErrorResponse(c, "This statement doesn't add up — nothing is wrong with the money, but please report it.", err.Error(), fiber.StatusInternalServerError)
	}
	if c.Query("format") == "csv" {
		return sendStatementCSV(c, agent, st)
	}
	return utils.SuccessResponse(c, fiber.Map{"agent": agent, "statement": st}, "")
}

func sendStatementCSV(c *fiber.Ctx, agent *models.Agent, st *services.AgentStatement) error {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	kes := services.FormatCents
	lastDay := st.To.AddDate(0, 0, -1)
	w.Write([]string{"Agent statement", agent.Name, agent.Phone})
	w.Write([]string{"Period (EAT)", utils.Kenya(st.From).Format("2006-01-02"), utils.Kenya(lastDay).Format("2006-01-02")})
	w.Write(nil)
	w.Write([]string{"Date (EAT)", "Type", "Vouchers", "Method", "Reference", "Note", "Amount (KES)", "Balance (KES)"})
	w.Write([]string{"", "Opening balance", "", "", "", "", "", kes(st.OpeningCents)})
	for _, l := range st.Lines {
		w.Write([]string{
			utils.Kenya(l.CreatedAt).Format("2006-01-02 15:04"), l.Type, fmt.Sprint(l.VoucherCount),
			l.Method, l.Reference, csvSafe(l.Note), kes(l.AmountCents), kes(l.BalanceAfterCents),
		})
	}
	w.Write([]string{"", "Closing balance", "", "", "", "", "", kes(st.ClosingCents)})
	w.Write(nil)
	w.Write([]string{"Summary", "KES"})
	for _, r := range [][2]string{
		{"Opening balance", kes(st.OpeningCents)},
		{fmt.Sprintf("Issued (%d vouchers)", st.IssuedCount), kes(st.IssuedCents)},
		{"Commission", kes(st.CommissionCents)},
		{fmt.Sprintf("Unsold returned (%d vouchers)", st.ReturnedCount), kes(st.ReturnedCents)},
		{"Commission reversed on returns", kes(st.CommissionReversedCents)},
		{"Handovers", kes(st.HandoverCents)},
		{"Adjustments", kes(st.AdjustmentCents)},
		{"Closing balance", kes(st.ClosingCents)},
		{fmt.Sprintf("Memo: sold to customers (%d vouchers)", st.SoldCount), kes(st.SoldCents)},
	} {
		w.Write([]string{r[0], r[1]})
	}
	w.Flush()
	name := fmt.Sprintf("agent-%d-statement-%s.csv", agent.ID, utils.Kenya(st.From).Format("20060102"))
	c.Set("Content-Type", "text/csv; charset=utf-8")
	c.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	return c.Send(buf.Bytes())
}

// csvSafe stops a note starting with =, +, - or @ being run as a formula
// when the CSV is opened in a spreadsheet.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}
