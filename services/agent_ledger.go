package services

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Agent ledger: an ISP issues vouchers to resellers ("agents"), who sell them
// for cash and hand the cash over later. Every money movement is an
// AgentLedgerEntry (append-only, integer cents); an agent's balance is always
// SUM(amount_cents) of their entries — positive means the agent owes the ISP,
// negative is prepaid float.
//
// Every write follows the same pattern inside one DB transaction, run at READ
// COMMITTED on MySQL so each read sees the latest committed rows:
//  1. lock the agent row (SELECT ... FOR UPDATE), which serializes all money
//     writes for that agent — double-clicks and concurrent requests queue;
//  2. look up the request's idempotency key and return the first result if it
//     was already done (everything for this agent is written under the lock
//     we now hold, so this read can't miss a committed result);
//  3. read the balance, validate, write the entries;
//  4. store the idempotency key. Its unique index is the last line of defence
//     if two requests somehow get past step 2 together.
//
// Reads after step 1 are deliberately plain, not FOR UPDATE: under MySQL's
// default REPEATABLE READ, locking reads take gap locks, and two agents'
// requests inserting next to each other then deadlock.

// LedgerError is a refusal the caller can show as-is, with an HTTP status.
type LedgerError struct {
	Status int
	Msg    string
}

func (e *LedgerError) Error() string { return e.Msg }

func ledgerErr(status int, format string, args ...interface{}) error {
	return &LedgerError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// ErrAgentNotFound is returned when the agent isn't in the caller's ISP.
var ErrAgentNotFound = &LedgerError{Status: 404, Msg: "Agent not found."}

const (
	// MaxAgentBatch caps one issue, so a typo can't mint 50,000 vouchers.
	MaxAgentBatch = 500
	// maxHandoverCents is a sanity cap per handover (KES 10 million).
	maxHandoverCents = 10_000_000_00
)

// AgentActor is who is acting, from the session — never from the request body.
type AgentActor struct {
	OrgID  uint
	UserID uint
	// Role is the actor's staff role. Recording that money was received
	// (an upfront-paid issue) needs the owner role, whoever the caller is.
	Role string
}

// IsOwner reports whether the actor is the ISP owner (super_admin), the only
// role that may confirm cash or M-Pesa was received.
func (a AgentActor) IsOwner() bool { return a.Role == "super_admin" }

// CentsFromKES converts a decimal(10,2) shilling amount to integer cents.
func CentsFromKES(kes float64) int64 { return int64(math.Round(kes * 100)) }

// CommissionFor is the commission on one voucher of faceCents for an agent,
// in cents. Percent rates are in basis points and round half up to the cent;
// the result never exceeds the face value.
func CommissionFor(commissionType string, value, faceCents int64) int64 {
	var c int64
	switch commissionType {
	case "fixed":
		c = value
	default: // percent, basis points
		c = (faceCents*value + 5000) / 10000
	}
	if c < 0 {
		return 0
	}
	if c > faceCents {
		return faceCents
	}
	return c
}

// lockAgent loads the agent of the caller's ISP and locks its row for the
// rest of the transaction.
func lockAgent(tx *gorm.DB, orgID, agentID uint) (*models.Agent, error) {
	var a models.Agent
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND organization_id = ?", agentID, orgID).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAgentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// AgentBalance is what the agent owes right now (negative = float), the sum
// of all their ledger entries. Inside a write it's read after lockAgent, so
// no other entry for this agent can be in flight.
func AgentBalance(tx *gorm.DB, agentID uint) (int64, error) {
	var sum int64
	err := tx.Model(&models.AgentLedgerEntry{}).
		Where("agent_id = ?", agentID).Select("COALESCE(SUM(amount_cents), 0)").Scan(&sum).Error
	return sum, err
}

// ledgerTx runs fn in a transaction, at READ COMMITTED on MySQL (see the top
// of this file). SQLite (tests) serializes writers anyway.
func ledgerTx(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	if db.Dialector.Name() == "mysql" {
		return db.Transaction(fn, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	return db.Transaction(fn)
}

// requestHash fingerprints a request, so a reused idempotency key with a
// different request is refused instead of replaying the wrong thing.
func requestHash(parts ...interface{}) string {
	h := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(h[:])
}

// replayed returns the earlier result of this idempotency key, if any.
func replayed(tx *gorm.DB, orgID uint, key, op string, agentID uint, hash string) (uint, bool, error) {
	var k models.AgentIdempotencyKey
	res := tx.Where("organization_id = ? AND idem_key = ?", orgID, key).Limit(1).Find(&k)
	if res.Error != nil {
		return 0, false, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, false, nil
	}
	if k.Operation != op || k.AgentID != agentID || k.RequestHash != hash {
		return 0, false, ledgerErr(409, "This request key was already used for a different request. Refresh and try again.")
	}
	return k.ResultID, true, nil
}

func saveKey(tx *gorm.DB, orgID uint, key, op string, agentID uint, hash string, resultID uint) error {
	return tx.Create(&models.AgentIdempotencyKey{
		OrganizationID: orgID, Key: key, AgentID: agentID, Operation: op,
		RequestHash: hash, ResultID: resultID, CreatedAt: time.Now().UTC(),
	}).Error
}

// runIdempotent runs fn in a transaction guarded by the agent lock and the
// idempotency key. If the key was already used, fn doesn't run and the first
// result's ID comes back with replay=true. If the transaction fails on commit
// because a concurrent request stored the same key first, that request's
// result is returned instead of an error.
func runIdempotent(db *gorm.DB, actor AgentActor, agentID uint, key, op, hash string,
	fn func(tx *gorm.DB, agent *models.Agent) (uint, error)) (resultID uint, replay bool, err error) {
	if !ValidIdempotencyKey(key) {
		return 0, false, ledgerErr(400, "A request key (Idempotency-Key, 8-64 letters, digits, - or _) is required.")
	}
	err = ledgerTx(db, func(tx *gorm.DB) error {
		agent, err := lockAgent(tx, actor.OrgID, agentID)
		if err != nil {
			return err
		}
		id, done, err := replayed(tx, actor.OrgID, key, op, agentID, hash)
		if err != nil {
			return err
		}
		if done {
			resultID, replay = id, true
			return nil
		}
		id, err = fn(tx, agent)
		if err != nil {
			return err
		}
		if err := saveKey(tx, actor.OrgID, key, op, agentID, hash, id); err != nil {
			return err
		}
		resultID = id
		return nil
	})
	if err != nil {
		var le *LedgerError
		if !errors.As(err, &le) {
			// Lost a race on the key's unique index: the other request won.
			if id, done, rerr := replayed(db, actor.OrgID, key, op, agentID, hash); rerr == nil && done {
				return id, true, nil
			}
		}
		return 0, false, err
	}
	return resultID, replay, nil
}

// ValidIdempotencyKey reports whether key is 8-64 chars of [A-Za-z0-9_-].
func ValidIdempotencyKey(key string) bool {
	if len(key) < 8 || len(key) > 64 {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func newEntry(actor AgentActor, agentID uint, typ string, amount int64) models.AgentLedgerEntry {
	return models.AgentLedgerEntry{
		OrganizationID: actor.OrgID, AgentID: agentID, Type: typ, AmountCents: amount,
		CreatedBy: actor.UserID, CreatedAt: time.Now().UTC(),
	}
}

// agentHasZone reports whether the agent sells in zoneID.
func agentHasZone(tx *gorm.DB, agentID, zoneID uint) bool {
	var n int64
	tx.Table("agent_zones").Where("agent_id = ? AND zone_id = ?", agentID, zoneID).Count(&n)
	return n > 0
}

// ---- Issue ----

// AgentIssueRequest issues Quantity vouchers of PackageID to an agent.
type AgentIssueRequest struct {
	AgentID   uint
	ZoneID    uint
	PackageID uint
	Quantity  int
	Funding   string // credit | float | upfront
	// Method/Reference record the upfront payment (Funding = upfront).
	Method    string
	Reference string
	Key       string
}

// AgentIssueResult is the batch, its vouchers and the entries it wrote.
type AgentIssueResult struct {
	Batch    models.AgentVoucherBatch
	Vouchers []models.Voucher
	Entries  []models.AgentLedgerEntry
	Replayed bool
}

// IssueToAgent generates a batch of vouchers for an agent and records what
// they owe for it: + face value, - commission. On "credit" the new balance
// must stay within the agent's credit limit; on "float" it must stay at or
// below zero (covered by money already handed over); "upfront" also records
// the payment, so the balance doesn't move.
func IssueToAgent(db *gorm.DB, vs *VoucherService, actor AgentActor, req AgentIssueRequest) (*AgentIssueResult, error) {
	req.Method = strings.ToLower(strings.TrimSpace(req.Method))
	req.Reference = strings.ToUpper(strings.TrimSpace(req.Reference))
	if req.Quantity < 1 || req.Quantity > MaxAgentBatch {
		return nil, ledgerErr(422, "Quantity must be between 1 and %d.", MaxAgentBatch)
	}
	switch req.Funding {
	case "credit", "float":
	case "upfront":
		// Upfront records a handover: only the owner confirms money received.
		// Anyone else issues on credit or float, within the limits.
		if !actor.IsOwner() {
			return nil, ledgerErr(403, "Only the account owner can record an upfront payment. Issue on credit or float instead.")
		}
		if err := validMethod(req.Method, req.Reference); err != nil {
			return nil, err
		}
	default:
		return nil, ledgerErr(422, "Funding must be credit, float or upfront.")
	}
	hash := requestHash("issue", req.AgentID, req.ZoneID, req.PackageID, req.Quantity, req.Funding, req.Method, req.Reference)

	batchID, replay, err := runIdempotent(db, actor, req.AgentID, req.Key, "issue", hash, func(tx *gorm.DB, agent *models.Agent) (uint, error) {
		if agent.Status != "active" {
			return 0, ledgerErr(422, "This agent is inactive.")
		}
		var zone models.Zone
		if err := tx.Where("id = ? AND organization_id = ?", req.ZoneID, actor.OrgID).First(&zone).Error; err != nil {
			return 0, ledgerErr(422, "Invalid zone for this organization.")
		}
		if !agentHasZone(tx, agent.ID, zone.ID) {
			return 0, ledgerErr(422, "This agent doesn't sell in %s. Add the zone to the agent first.", zone.Name)
		}
		var pkg models.Package
		if err := tx.Where("id = ? AND zone_id = ?", req.PackageID, zone.ID).First(&pkg).Error; err != nil {
			return 0, ledgerErr(422, "That package isn't in this zone.")
		}
		if pkg.Status != "active" {
			return 0, ledgerErr(422, "That package is inactive.")
		}
		face := CentsFromKES(pkg.Price)
		if face <= 0 {
			return 0, ledgerErr(422, "Free packages can't be issued to agents.")
		}
		commission := CommissionFor(agent.CommissionType, agent.CommissionValue, face)
		qty := int64(req.Quantity)
		gross, comm := face*qty, commission*qty
		net := gross - comm

		balance, err := AgentBalance(tx, agent.ID)
		if err != nil {
			return 0, err
		}
		switch req.Funding {
		case "credit":
			if balance+net > agent.CreditLimitCents {
				return 0, ledgerErr(422, "Over the credit limit: this batch costs KES %s, the agent can take KES %s more on credit.",
					FormatCents(net), FormatCents(max64(agent.CreditLimitCents-balance, 0)))
			}
		case "float":
			if balance+net > 0 {
				return 0, ledgerErr(422, "Not enough float: this batch costs KES %s, the agent has KES %s float.",
					FormatCents(net), FormatCents(max64(-balance, 0)))
			}
		}

		batch := models.AgentVoucherBatch{
			OrganizationID: actor.OrgID, AgentID: agent.ID, ZoneID: zone.ID, PackageID: pkg.ID,
			Quantity: req.Quantity, FaceValueCents: face, CommissionCents: commission,
			Funding: req.Funding, CreatedBy: actor.UserID, CreatedAt: time.Now().UTC(),
		}
		if err := tx.Create(&batch).Error; err != nil {
			return 0, err
		}
		for i := 0; i < req.Quantity; i++ {
			v, err := vs.GenerateTx(tx, zone.ID, pkg.ID, "single_use", 1)
			if err != nil {
				return 0, err
			}
			if err := tx.Create(&models.AgentVoucher{
				OrganizationID: actor.OrgID, AgentID: agent.ID, BatchID: batch.ID, VoucherID: v.ID,
				FaceValueCents: face, CommissionCents: commission, Status: "issued", CreatedAt: time.Now().UTC(),
			}).Error; err != nil {
				return 0, err
			}
		}

		note := fmt.Sprintf("%d x %s @ KES %s", req.Quantity, pkg.Name, FormatCents(face))
		entries := []models.AgentLedgerEntry{newEntry(actor, agent.ID, models.AgentEntryIssue, gross)}
		if comm > 0 {
			entries = append(entries, newEntry(actor, agent.ID, models.AgentEntryCommission, -comm))
		}
		if req.Funding == "upfront" && net > 0 {
			h := newEntry(actor, agent.ID, models.AgentEntryHandover, -net)
			h.Method, h.Reference, h.Note = req.Method, req.Reference, "Paid upfront for batch"
			entries = append(entries, h)
		}
		for i := range entries {
			entries[i].BatchID = &batch.ID
			entries[i].VoucherCount = req.Quantity
			if entries[i].Note == "" {
				entries[i].Note = note
			}
			if err := tx.Create(&entries[i]).Error; err != nil {
				return 0, err
			}
		}
		return batch.ID, nil
	})
	if err != nil {
		return nil, err
	}
	res := &AgentIssueResult{Replayed: replay}
	if err := db.Preload("Package").Preload("Zone").First(&res.Batch, batchID).Error; err != nil {
		return nil, err
	}
	db.Model(&models.Voucher{}).
		Where("id IN (?)", db.Model(&models.AgentVoucher{}).Select("voucher_id").Where("batch_id = ?", batchID)).
		Order("id").Find(&res.Vouchers)
	db.Where("batch_id = ? AND type IN ?", batchID, []string{models.AgentEntryIssue, models.AgentEntryCommission, models.AgentEntryHandover}).
		Order("id").Find(&res.Entries)
	return res, nil
}

// ---- Return ----

// AgentReturnRequest returns unsold vouchers: listed VoucherIDs, or every
// unsold voucher of BatchID.
type AgentReturnRequest struct {
	AgentID    uint
	VoucherIDs []uint
	BatchID    uint
	Note       string
	Key        string
}

// ReturnAgentVouchers voids unsold vouchers an agent hands back and reverses
// what they owed for them: - face value, + the commission they'd been given.
// All or nothing: if any voucher was sold (or redeemed) in the meantime, the
// whole return is refused and nothing changes.
func ReturnAgentVouchers(db *gorm.DB, actor AgentActor, req AgentReturnRequest) (*models.AgentLedgerEntry, bool, error) {
	ids := append([]uint(nil), req.VoucherIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 && req.BatchID == 0 {
		return nil, false, ledgerErr(422, "Pick the vouchers (or batch) being returned.")
	}
	hash := requestHash("return", req.AgentID, ids, req.BatchID, req.Note)

	entryID, replay, err := runIdempotent(db, actor, req.AgentID, req.Key, "return", hash, func(tx *gorm.DB, agent *models.Agent) (uint, error) {
		// A plain read: the rows are claimed below by conditional UPDATEs,
		// voucher first and then agent_voucher — the same order a redemption
		// locks them in, so the two can't deadlock.
		var held []models.AgentVoucher
		q := tx.Preload("Voucher").Where("agent_id = ? AND organization_id = ?", agent.ID, actor.OrgID)
		if len(ids) > 0 {
			q = q.Where("voucher_id IN ?", ids)
		} else {
			q = q.Where("batch_id = ? AND status = ?", req.BatchID, "issued")
		}
		if err := q.Order("id").Find(&held).Error; err != nil {
			return 0, err
		}
		if len(held) == 0 {
			return 0, ledgerErr(422, "No unsold vouchers to return.")
		}
		if len(ids) > 0 && len(held) != len(dedupe(ids)) {
			return 0, ledgerErr(422, "Some of those vouchers aren't held by this agent.")
		}

		var face, comm int64
		var batchIDs = map[uint]bool{}
		now := time.Now().UTC()
		for _, av := range held {
			code := fmt.Sprint(av.VoucherID)
			if av.Voucher != nil {
				code = av.Voucher.Code
			}
			if av.Status != "issued" {
				return 0, ledgerErr(422, "Voucher %s is already %s, it can't be returned.", code, av.Status)
			}
			// Void the voucher only if nobody has used it. Atomic and
			// conditional, so it can't race a redemption (which locks the
			// voucher row and checks status too): exactly one of them wins.
			res := tx.Model(&models.Voucher{}).
				Where("id = ? AND status = ? AND usage_count = 0", av.VoucherID, "unused").
				Update("status", VoucherStatusVoid)
			if res.Error != nil {
				return 0, res.Error
			}
			if res.RowsAffected != 1 {
				return 0, ledgerErr(422, "Voucher %s has been used, it can't be returned.", code)
			}
			res = tx.Model(&models.AgentVoucher{}).Where("id = ? AND status = ?", av.ID, "issued").
				Updates(map[string]interface{}{"status": "returned", "returned_at": now})
			if res.Error != nil {
				return 0, res.Error
			}
			if res.RowsAffected != 1 {
				return 0, ledgerErr(409, "Voucher %s changed while returning it. Try again.", code)
			}
			face += av.FaceValueCents
			comm += av.CommissionCents
			batchIDs[av.BatchID] = true
		}

		n := len(held)
		note := strings.TrimSpace(req.Note)
		if note == "" {
			note = fmt.Sprintf("%d unsold voucher(s) returned", n)
		}
		var batchID *uint
		if len(batchIDs) == 1 {
			for id := range batchIDs {
				id := id
				batchID = &id
			}
		}
		ret := newEntry(actor, agent.ID, models.AgentEntryReturn, -face)
		ret.VoucherCount, ret.Note, ret.BatchID = n, note, batchID
		if err := tx.Create(&ret).Error; err != nil {
			return 0, err
		}
		if comm > 0 {
			rev := newEntry(actor, agent.ID, models.AgentEntryCommissionReversal, comm)
			rev.VoucherCount, rev.Note, rev.BatchID = n, note, batchID
			if err := tx.Create(&rev).Error; err != nil {
				return 0, err
			}
		}
		return ret.ID, nil
	})
	if err != nil {
		return nil, false, err
	}
	var e models.AgentLedgerEntry
	if err := db.First(&e, entryID).Error; err != nil {
		return nil, false, err
	}
	return &e, replay, nil
}

func dedupe(ids []uint) []uint {
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

// MarkAgentVoucherSold records that an agent's voucher was redeemed by a
// customer. Called inside the redemption transaction; a no-op for vouchers no
// agent holds. Selling doesn't move the agent's balance — they already owe
// for the voucher from when it was issued — it moves value from "unsold
// stock" to "cash the agent should be holding".
func MarkAgentVoucherSold(tx *gorm.DB, voucherID uint) error {
	return tx.Model(&models.AgentVoucher{}).
		Where("voucher_id = ? AND status = ?", voucherID, "issued").
		Updates(map[string]interface{}{"status": "sold", "sold_at": time.Now().UTC()}).Error
}

// ---- Handover, adjustment, reversal ----

func validMethod(method, reference string) error {
	switch method {
	case "cash":
		return nil
	case "mpesa":
		if len(reference) < 8 || len(reference) > 20 {
			return ledgerErr(422, "Enter the M-Pesa confirmation code.")
		}
		return nil
	}
	return ledgerErr(422, "Method must be cash or mpesa.")
}

// AgentHandoverRequest records money received from an agent.
type AgentHandoverRequest struct {
	AgentID     uint
	AmountCents int64
	Method      string // cash | mpesa
	Reference   string
	Note        string
	Key         string
}

// RecordHandover records cash or M-Pesa received from an agent, reducing
// what they owe. Paying more than they owe leaves the rest as float. An
// M-Pesa code already recorded for this ISP (and not reversed) is refused.
func RecordHandover(db *gorm.DB, actor AgentActor, req AgentHandoverRequest) (*models.AgentLedgerEntry, bool, error) {
	req.Method = strings.ToLower(strings.TrimSpace(req.Method))
	req.Reference = strings.ToUpper(strings.TrimSpace(req.Reference))
	req.Note = strings.TrimSpace(req.Note)
	if req.AmountCents <= 0 || req.AmountCents > maxHandoverCents {
		return nil, false, ledgerErr(422, "Enter an amount above zero.")
	}
	if err := validMethod(req.Method, req.Reference); err != nil {
		return nil, false, err
	}
	hash := requestHash("handover", req.AgentID, req.AmountCents, req.Method, req.Reference, req.Note)

	entryID, replay, err := runIdempotent(db, actor, req.AgentID, req.Key, "handover", hash, func(tx *gorm.DB, agent *models.Agent) (uint, error) {
		if req.Method == "mpesa" {
			var n int64
			tx.Model(&models.AgentLedgerEntry{}).
				Where("organization_id = ? AND type = ? AND reference = ?", actor.OrgID, models.AgentEntryHandover, req.Reference).
				Where("id NOT IN (?)", tx.Model(&models.AgentLedgerEntry{}).Select("reverses_id").Where("reverses_id IS NOT NULL")).
				Count(&n)
			if n > 0 {
				return 0, ledgerErr(409, "M-Pesa code %s is already recorded.", req.Reference)
			}
		}
		e := newEntry(actor, agent.ID, models.AgentEntryHandover, -req.AmountCents)
		e.Method, e.Reference, e.Note = req.Method, req.Reference, req.Note
		if err := tx.Create(&e).Error; err != nil {
			return 0, err
		}
		return e.ID, nil
	})
	if err != nil {
		return nil, false, err
	}
	var e models.AgentLedgerEntry
	if err := db.First(&e, entryID).Error; err != nil {
		return nil, false, err
	}
	return &e, replay, nil
}

// RecordAdjustment records an owner correction (positive: the agent owes
// more; negative: less). A note saying why is required.
func RecordAdjustment(db *gorm.DB, actor AgentActor, agentID uint, amountCents int64, note, key string) (*models.AgentLedgerEntry, bool, error) {
	note = strings.TrimSpace(note)
	if amountCents == 0 || amountCents > maxHandoverCents || amountCents < -maxHandoverCents {
		return nil, false, ledgerErr(422, "Enter a non-zero amount.")
	}
	if note == "" {
		return nil, false, ledgerErr(422, "Say why in the note.")
	}
	hash := requestHash("adjustment", agentID, amountCents, note)
	entryID, replay, err := runIdempotent(db, actor, agentID, key, "adjustment", hash, func(tx *gorm.DB, agent *models.Agent) (uint, error) {
		e := newEntry(actor, agent.ID, models.AgentEntryAdjustment, amountCents)
		e.Note = note
		if err := tx.Create(&e).Error; err != nil {
			return 0, err
		}
		return e.ID, nil
	})
	if err != nil {
		return nil, false, err
	}
	var e models.AgentLedgerEntry
	if err := db.First(&e, entryID).Error; err != nil {
		return nil, false, err
	}
	return &e, replay, nil
}

// ReverseEntry cancels a handover or adjustment recorded by mistake with an
// equal and opposite entry. Each entry can be reversed once (unique
// reverses_id). Issues are undone by returning the vouchers instead.
func ReverseEntry(db *gorm.DB, actor AgentActor, agentID, entryID uint, note, key string) (*models.AgentLedgerEntry, bool, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, false, ledgerErr(422, "Say why in the note.")
	}
	hash := requestHash("reversal", agentID, entryID, note)
	revID, replay, err := runIdempotent(db, actor, agentID, key, "reversal", hash, func(tx *gorm.DB, agent *models.Agent) (uint, error) {
		var orig models.AgentLedgerEntry
		if err := tx.Where("id = ? AND agent_id = ? AND organization_id = ?", entryID, agent.ID, actor.OrgID).First(&orig).Error; err != nil {
			return 0, ledgerErr(404, "Entry not found.")
		}
		if orig.Type != models.AgentEntryHandover && orig.Type != models.AgentEntryAdjustment {
			return 0, ledgerErr(422, "Only handovers and adjustments can be reversed. Return unsold vouchers to undo an issue.")
		}
		var n int64
		tx.Model(&models.AgentLedgerEntry{}).Where("reverses_id = ?", orig.ID).Count(&n)
		if n > 0 {
			return 0, ledgerErr(409, "That entry is already reversed.")
		}
		e := newEntry(actor, agent.ID, models.AgentEntryReversal, -orig.AmountCents)
		e.ReversesID = &orig.ID
		e.Method, e.Reference, e.BatchID = orig.Method, orig.Reference, orig.BatchID
		e.Note = note
		if err := tx.Create(&e).Error; err != nil {
			return 0, err
		}
		return e.ID, nil
	})
	if err != nil {
		return nil, false, err
	}
	var e models.AgentLedgerEntry
	if err := db.First(&e, revID).Error; err != nil {
		return nil, false, err
	}
	return &e, replay, nil
}

// ---- Read side ----

// AgentSummary is the at-a-glance position of one agent.
type AgentSummary struct {
	// BalanceCents is what the agent owes in total (negative = float).
	BalanceCents int64 `json:"balance_cents"`
	// UnsoldCount/UnsoldNetCents: vouchers still in the agent's hands and
	// what they'll owe for them (face - commission).
	UnsoldCount    int64 `json:"unsold_count"`
	UnsoldNetCents int64 `json:"unsold_net_cents"`
	// CashDueCents = BalanceCents - UnsoldNetCents: money the agent should be
	// holding from vouchers already sold, not yet handed over.
	CashDueCents  int64 `json:"cash_due_cents"`
	SoldWeekCount int64 `json:"sold_week_count"`
	SoldWeekCents int64 `json:"sold_week_cents"`
	// CommissionEarnedCents is commission on vouchers actually sold.
	CommissionEarnedCents int64      `json:"commission_earned_cents"`
	LastHandoverAt        *time.Time `json:"last_handover_at"`
	LastHandoverCents     int64      `json:"last_handover_cents"`
}

// AgentSummaries computes summaries for the given agents (of one ISP).
// weekStart is the start of "this week" for the sold-this-week figures.
func AgentSummaries(db *gorm.DB, orgID uint, agentIDs []uint, weekStart time.Time) (map[uint]*AgentSummary, error) {
	out := make(map[uint]*AgentSummary, len(agentIDs))
	if len(agentIDs) == 0 {
		return out, nil
	}
	for _, id := range agentIDs {
		out[id] = &AgentSummary{}
	}

	type row struct {
		AgentID uint
		N       int64
		Cents   int64
		Comm    int64
	}
	var balances []row
	if err := db.Model(&models.AgentLedgerEntry{}).Select("agent_id, COALESCE(SUM(amount_cents),0) AS cents").
		Where("organization_id = ? AND agent_id IN ?", orgID, agentIDs).Group("agent_id").Scan(&balances).Error; err != nil {
		return nil, err
	}
	for _, r := range balances {
		out[r.AgentID].BalanceCents = r.Cents
	}

	var unsold []row
	db.Model(&models.AgentVoucher{}).Select("agent_id, COUNT(*) AS n, COALESCE(SUM(face_value_cents - commission_cents),0) AS cents").
		Where("organization_id = ? AND agent_id IN ? AND status = ?", orgID, agentIDs, "issued").Group("agent_id").Scan(&unsold)
	for _, r := range unsold {
		out[r.AgentID].UnsoldCount, out[r.AgentID].UnsoldNetCents = r.N, r.Cents
	}

	var week []row
	db.Model(&models.AgentVoucher{}).Select("agent_id, COUNT(*) AS n, COALESCE(SUM(face_value_cents),0) AS cents").
		Where("organization_id = ? AND agent_id IN ? AND status = ? AND sold_at >= ?", orgID, agentIDs, "sold", weekStart.UTC()).
		Group("agent_id").Scan(&week)
	for _, r := range week {
		out[r.AgentID].SoldWeekCount, out[r.AgentID].SoldWeekCents = r.N, r.Cents
	}

	var earned []row
	db.Model(&models.AgentVoucher{}).Select("agent_id, COALESCE(SUM(commission_cents),0) AS comm").
		Where("organization_id = ? AND agent_id IN ? AND status = ?", orgID, agentIDs, "sold").Group("agent_id").Scan(&earned)
	for _, r := range earned {
		out[r.AgentID].CommissionEarnedCents = r.Comm
	}

	// Latest handover per agent (not counting ones later reversed).
	var handovers []models.AgentLedgerEntry
	db.Where("organization_id = ? AND agent_id IN ? AND type = ?", orgID, agentIDs, models.AgentEntryHandover).
		Where("id NOT IN (?)", db.Model(&models.AgentLedgerEntry{}).Select("reverses_id").Where("reverses_id IS NOT NULL")).
		Order("id DESC").Find(&handovers)
	for _, h := range handovers {
		s := out[h.AgentID]
		if s.LastHandoverAt == nil {
			t := h.CreatedAt
			s.LastHandoverAt, s.LastHandoverCents = &t, -h.AmountCents
		}
	}

	for _, s := range out {
		s.CashDueCents = s.BalanceCents - s.UnsoldNetCents
	}
	return out, nil
}

// StatementLine is one ledger entry with the balance after it.
type StatementLine struct {
	models.AgentLedgerEntry
	BalanceAfterCents int64 `json:"balance_after_cents"`
}

// AgentStatement is an agent's account for a period. Every money figure is a
// sum of ledger entries in [From, To); ClosingCents is computed separately as
// the sum of every entry before To and must equal Opening + the period's
// lines, or the statement is refused as not reconciling.
type AgentStatement struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"` // exclusive

	OpeningCents            int64 `json:"opening_cents"`
	IssuedCents             int64 `json:"issued_cents"`
	IssuedCount             int64 `json:"issued_count"`
	CommissionCents         int64 `json:"commission_cents"` // negative
	ReturnedCents           int64 `json:"returned_cents"`   // negative
	ReturnedCount           int64 `json:"returned_count"`
	CommissionReversedCents int64 `json:"commission_reversed_cents"`
	HandoverCents           int64 `json:"handover_cents"`   // negative
	AdjustmentCents         int64 `json:"adjustment_cents"` // adjustments and reversals
	ClosingCents            int64 `json:"closing_cents"`

	// Memo (not ledger lines): vouchers customers redeemed in the period.
	SoldCount           int64 `json:"sold_count"`
	SoldCents           int64 `json:"sold_cents"`
	SoldCommissionCents int64 `json:"sold_commission_cents"`

	Lines []StatementLine `json:"lines"`
}

// BuildAgentStatement builds the statement of one agent for [from, to).
func BuildAgentStatement(db *gorm.DB, orgID, agentID uint, from, to time.Time) (*AgentStatement, error) {
	from, to = from.UTC(), to.UTC()
	st := &AgentStatement{From: from, To: to, Lines: []StatementLine{}}
	base := func() *gorm.DB {
		return db.Model(&models.AgentLedgerEntry{}).Where("organization_id = ? AND agent_id = ?", orgID, agentID)
	}
	if err := base().Where("created_at < ?", from).Select("COALESCE(SUM(amount_cents),0)").Scan(&st.OpeningCents).Error; err != nil {
		return nil, err
	}
	var entries []models.AgentLedgerEntry
	if err := base().Preload("Creator", func(q *gorm.DB) *gorm.DB { return q.Select("id", "name") }).
		Where("created_at >= ? AND created_at < ?", from, to).Order("created_at, id").Find(&entries).Error; err != nil {
		return nil, err
	}
	run := st.OpeningCents
	for _, e := range entries {
		run += e.AmountCents
		st.Lines = append(st.Lines, StatementLine{AgentLedgerEntry: e, BalanceAfterCents: run})
		switch e.Type {
		case models.AgentEntryIssue:
			st.IssuedCents += e.AmountCents
			st.IssuedCount += int64(e.VoucherCount)
		case models.AgentEntryCommission:
			st.CommissionCents += e.AmountCents
		case models.AgentEntryReturn:
			st.ReturnedCents += e.AmountCents
			st.ReturnedCount += int64(e.VoucherCount)
		case models.AgentEntryCommissionReversal:
			st.CommissionReversedCents += e.AmountCents
		case models.AgentEntryHandover:
			st.HandoverCents += e.AmountCents
		default:
			st.AdjustmentCents += e.AmountCents
		}
	}
	if err := base().Where("created_at < ?", to).Select("COALESCE(SUM(amount_cents),0)").Scan(&st.ClosingCents).Error; err != nil {
		return nil, err
	}
	if st.ClosingCents != run {
		return nil, fmt.Errorf("agent %d statement does not reconcile: closing %d != opening + lines %d", agentID, st.ClosingCents, run)
	}

	var sold struct {
		N     int64
		Cents int64
		Comm  int64
	}
	db.Model(&models.AgentVoucher{}).
		Select("COUNT(*) AS n, COALESCE(SUM(face_value_cents),0) AS cents, COALESCE(SUM(commission_cents),0) AS comm").
		Where("organization_id = ? AND agent_id = ? AND status = ? AND sold_at >= ? AND sold_at < ?", orgID, agentID, "sold", from, to).
		Scan(&sold)
	st.SoldCount, st.SoldCents, st.SoldCommissionCents = sold.N, sold.Cents, sold.Comm
	return st, nil
}

// FormatCents renders cents as shillings with thousands separators: 123456 -> "1,234.56".
func FormatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	whole, frac := c/100, c%100
	s := fmt.Sprint(whole)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return fmt.Sprintf("%s%s.%02d", sign, s, frac)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
