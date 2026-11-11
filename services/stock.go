package services

import (
	"errors"
	"strings"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Stock ledger. stock_moves is append-only; stock_items.on_hand moves in the
// same transaction through a conditional UPDATE, so stock can't go negative
// even when two crews take the last unit at once.

// CanManageStock: buying, counting and writing off stock is the owner's.
func CanManageStock(role string) bool { return role == "super_admin" }

// CanViewStock: office roles that see stock levels and value.
func CanViewStock(role string) bool {
	return role == "super_admin" || role == "zone_manager" || role == "finance"
}

// StockItemInput creates or edits a stock item.
type StockItemInput struct {
	Name          string  `json:"name"`
	Unit          string  `json:"unit"`
	ReorderLevel  float64 `json:"reorder_level"`
	TracksSerials bool    `json:"tracks_serials"`
	Active        *bool   `json:"active"`
}

func (in *StockItemInput) clean() error {
	in.Name = clip(in.Name, 120)
	if in.Name == "" {
		return installErr(422, "Give the item a name.")
	}
	in.Unit = strings.ToLower(strings.TrimSpace(in.Unit))
	switch in.Unit {
	case "", "pcs", "pieces":
		in.Unit = "pcs"
	case "m", "metres", "meters":
		in.Unit = "metres"
	default:
		return installErr(422, "Unit must be pieces or metres.")
	}
	if in.ReorderLevel < 0 || toHundredths(in.ReorderLevel) > maxQtyHundredths {
		return installErr(422, "Reorder level can't be negative.")
	}
	if in.TracksSerials && in.Unit != "pcs" {
		return installErr(422, "Only items counted in pieces can have serial numbers.")
	}
	return nil
}

// CreateStockItem adds a kind of stock (starts at zero; record a purchase to add some).
func CreateStockItem(db *gorm.DB, a InstallActor, in StockItemInput) (*models.StockItem, error) {
	if !CanManageStock(a.Role) {
		return nil, installErr(403, "Only the owner can add stock items.")
	}
	if err := in.clean(); err != nil {
		return nil, err
	}
	var n int64
	db.Model(&models.StockItem{}).Where("organization_id = ? AND name = ?", a.OrgID, in.Name).Count(&n)
	if n > 0 {
		return nil, installErr(409, "You already have an item called %s.", in.Name)
	}
	item := models.StockItem{OrganizationID: a.OrgID, Name: in.Name, Unit: in.Unit,
		ReorderLevel: hundredthsToQty(toHundredths(in.ReorderLevel)), TracksSerials: in.TracksSerials, Active: true}
	if err := db.Create(&item).Error; err != nil {
		return nil, err
	}
	return &item, nil
}

// UpdateStockItem edits name, reorder level and whether it is still used.
// Unit and serial tracking can't change once the item has moves.
func UpdateStockItem(db *gorm.DB, a InstallActor, id uint, in StockItemInput) (*models.StockItem, error) {
	if !CanManageStock(a.Role) {
		return nil, installErr(403, "Only the owner can edit stock items.")
	}
	if err := in.clean(); err != nil {
		return nil, err
	}
	var item models.StockItem
	if err := db.Where("id = ? AND organization_id = ?", id, a.OrgID).First(&item).Error; err != nil {
		return nil, installErr(404, "Stock item not found.")
	}
	var moves int64
	db.Model(&models.StockMove{}).Where("stock_item_id = ?", item.ID).Count(&moves)
	if moves > 0 && (in.Unit != item.Unit || in.TracksSerials != item.TracksSerials) {
		return nil, installErr(409, "This item already has stock history; its unit and serial tracking can't change.")
	}
	upd := map[string]interface{}{"name": in.Name, "unit": in.Unit, "reorder_level": hundredthsToQty(toHundredths(in.ReorderLevel)), "tracks_serials": in.TracksSerials}
	if in.Active != nil {
		upd["active"] = *in.Active
	}
	if err := db.Model(&item).Updates(upd).Error; err != nil {
		return nil, err
	}
	db.First(&item, item.ID)
	return &item, nil
}

func lockItem(tx *gorm.DB, orgID, itemID uint) (*models.StockItem, error) {
	var item models.StockItem
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", itemID, orgID).First(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, installErr(404, "Stock item not found.")
	}
	return &item, err
}

// takeOut lowers on_hand only if there is enough (the DB-checked guard).
func takeOut(tx *gorm.DB, item *models.StockItem, qtyH int64) error {
	qty := hundredthsToQty(qtyH)
	res := tx.Model(&models.StockItem{}).Where("id = ? AND on_hand >= ?", item.ID, qty).
		Update("on_hand", gorm.Expr("on_hand - ?", qty))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		var cur models.StockItem
		tx.Select("on_hand").First(&cur, item.ID)
		return installErr(409, "Not enough %s in stock: %s %s left, %s needed.", item.Name,
			trimQty(cur.OnHand), item.Unit, trimQty(qty))
	}
	return nil
}

func putIn(tx *gorm.DB, item *models.StockItem, qtyH int64) error {
	return tx.Model(&models.StockItem{}).Where("id = ?", item.ID).
		Update("on_hand", gorm.Expr("on_hand + ?", hundredthsToQty(qtyH))).Error
}

func trimQty(q float64) string {
	return FormatKES(toHundredths(q)) // hundredths print like cents: 1,250 or 12.50
}

func actorUID(a InstallActor) *uint {
	if a.UserID == 0 {
		return nil
	}
	u := a.UserID
	return &u
}

// useStock takes stock out onto a job at the item's average cost and returns
// the cost in cents.
func useStock(tx *gorm.DB, a InstallActor, itemID uint, qtyH int64, jobID, ontID *uint, note string) (int64, error) {
	item, err := lockItem(tx, a.OrgID, itemID)
	if err != nil {
		return 0, err
	}
	if !item.Active {
		return 0, installErr(422, "%s is no longer used.", item.Name)
	}
	if err := takeOut(tx, item, qtyH); err != nil {
		return 0, err
	}
	unit := toCents(item.UnitCost)
	mv := models.StockMove{OrganizationID: a.OrgID, StockItemID: item.ID, Type: models.MoveUsed, Direction: -1,
		Quantity: hundredthsToQty(qtyH), UnitCost: centsToKES(unit), JobID: jobID, OntUnitID: ontID, UserID: actorUID(a), Note: clip(note, 500)}
	if err := tx.Create(&mv).Error; err != nil {
		return 0, err
	}
	return lineCents(qtyH, unit), nil
}

// PurchaseInput records stock bought.
type PurchaseInput struct {
	Quantity float64  `json:"quantity"`
	UnitCost float64  `json:"unit_cost"`
	Serials  []string `json:"serials"`
	Note     string   `json:"note"`
}

// PurchaseStock adds bought stock at its price and re-averages the unit cost.
// For serial-tracked items every serial is registered as an in-stock ONT.
func PurchaseStock(db *gorm.DB, a InstallActor, itemID uint, in PurchaseInput) (*models.StockItem, error) {
	if !CanManageStock(a.Role) {
		return nil, installErr(403, "Only the owner can record stock purchases.")
	}
	qtyH, unit := toHundredths(in.Quantity), toCents(in.UnitCost)
	if unit < 0 || unit > maxUnitCostCents {
		return nil, installErr(422, "Unit cost must be between KES 0 and KES 1,000,000.")
	}
	var out models.StockItem
	err := db.Transaction(func(tx *gorm.DB) error {
		item, err := lockItem(tx, a.OrgID, itemID)
		if err != nil {
			return err
		}
		var serials []string
		if item.TracksSerials {
			seen := map[string]bool{}
			for _, s := range in.Serials {
				s = strings.ToUpper(strings.TrimSpace(s))
				if s == "" {
					continue
				}
				if len(s) > 64 {
					return installErr(422, "Serial %s is too long.", s[:20])
				}
				if seen[s] {
					return installErr(422, "Serial %s is listed twice.", s)
				}
				seen[s] = true
				serials = append(serials, s)
			}
			if len(serials) == 0 {
				return installErr(422, "Enter the serial number of every %s bought.", item.Name)
			}
			if in.Quantity != 0 && qtyH != int64(len(serials))*100 {
				return installErr(422, "You entered %d serials but a quantity of %s.", len(serials), trimQty(in.Quantity))
			}
			qtyH = int64(len(serials)) * 100
			var taken []string
			tx.Model(&models.OntUnit{}).Where("organization_id = ? AND serial IN ?", a.OrgID, serials).Pluck("serial", &taken)
			if len(taken) > 0 {
				return installErr(409, "Already in your list: %s.", strings.Join(taken, ", "))
			}
		}
		if qtyH <= 0 || qtyH > maxQtyHundredths {
			return installErr(422, "Quantity must be more than zero.")
		}
		if item.Unit == "pcs" && qtyH%100 != 0 {
			return installErr(422, "%s is counted in whole pieces.", item.Name)
		}
		avg := weightedAverageCents(toHundredths(item.OnHand), toCents(item.UnitCost), qtyH, unit)
		if err := tx.Model(&models.StockItem{}).Where("id = ?", item.ID).Updates(map[string]interface{}{
			"on_hand": gorm.Expr("on_hand + ?", hundredthsToQty(qtyH)), "unit_cost": centsToKES(avg),
		}).Error; err != nil {
			return err
		}
		mv := models.StockMove{OrganizationID: a.OrgID, StockItemID: item.ID, Type: models.MovePurchase, Direction: 1,
			Quantity: hundredthsToQty(qtyH), UnitCost: centsToKES(unit), UserID: actorUID(a), Note: clip(in.Note, 500)}
		if err := tx.Create(&mv).Error; err != nil {
			return err
		}
		for _, s := range serials {
			if err := tx.Create(&models.OntUnit{OrganizationID: a.OrgID, Serial: s, StockItemID: item.ID, Status: models.OntInStock}).Error; err != nil {
				return err
			}
		}
		return tx.First(&out, item.ID).Error
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// StockChangeInput is a count correction, a write-off or a return.
type StockChangeInput struct {
	Quantity float64 `json:"quantity"` // adjustment: + adds, - removes; others: positive
	JobID    *uint   `json:"job_id"`   // returns only: leftover material back from this job
	Note     string  `json:"note"`
}

// ChangeStock records an adjustment, write-off (faulty/lost) or return to the
// store. Serial-tracked items move only through their serials (ONT actions),
// so the serial list and the count can't disagree.
func ChangeStock(db *gorm.DB, a InstallActor, itemID uint, kind string, in StockChangeInput) (*models.StockItem, error) {
	if !CanManageStock(a.Role) {
		return nil, installErr(403, "Only the owner can change stock.")
	}
	note := clip(in.Note, 500)
	if note == "" {
		return nil, installErr(422, "Add a short note saying why.")
	}
	qtyH := toHundredths(in.Quantity)
	dir := 1
	switch kind {
	case models.MoveAdjustment:
		if qtyH < 0 {
			dir, qtyH = -1, -qtyH
		}
	case models.MoveWriteOff:
		dir = -1
	case models.MoveReturned:
	default:
		return nil, installErr(422, "Unknown stock change.")
	}
	if qtyH <= 0 || qtyH > maxQtyHundredths {
		return nil, installErr(422, "Quantity must be more than zero.")
	}
	var out models.StockItem
	err := db.Transaction(func(tx *gorm.DB) error {
		item, err := lockItem(tx, a.OrgID, itemID)
		if err != nil {
			return err
		}
		if item.TracksSerials {
			return installErr(422, "%s is tracked by serial. Use the ONT list to take one back or write it off.", item.Name)
		}
		if item.Unit == "pcs" && qtyH%100 != 0 {
			return installErr(422, "%s is counted in whole pieces.", item.Name)
		}
		var jobID *uint
		if kind == models.MoveReturned && in.JobID != nil && *in.JobID != 0 {
			// Only what the job actually used can come back to it.
			var used struct{ UsedQty, BackQty float64 }
			tx.Model(&models.StockMove{}).Select(
				"COALESCE(SUM(CASE WHEN type = 'used' THEN quantity ELSE 0 END),0) AS used_qty, COALESCE(SUM(CASE WHEN type = 'returned' THEN quantity ELSE 0 END),0) AS back_qty").
				Where("organization_id = ? AND job_id = ? AND stock_item_id = ?", a.OrgID, *in.JobID, item.ID).Scan(&used)
			if toHundredths(used.UsedQty)-toHundredths(used.BackQty) < qtyH {
				return installErr(422, "That job used only %s %s of %s.", trimQty(used.UsedQty-used.BackQty), item.Unit, item.Name)
			}
			jobID = in.JobID
		}
		if dir < 0 {
			if err := takeOut(tx, item, qtyH); err != nil {
				return err
			}
		} else if err := putIn(tx, item, qtyH); err != nil {
			return err
		}
		mv := models.StockMove{OrganizationID: a.OrgID, StockItemID: item.ID, Type: kind, Direction: dir,
			Quantity: hundredthsToQty(qtyH), UnitCost: item.UnitCost, JobID: jobID, UserID: actorUID(a), Note: note}
		if jobID != nil {
			// Returned leftovers are credited back at the cost they went out at.
			var last models.StockMove
			if tx.Where("job_id = ? AND stock_item_id = ? AND type = ?", *jobID, item.ID, models.MoveUsed).Order("id DESC").First(&last).Error == nil {
				mv.UnitCost = last.UnitCost
			}
		}
		if err := tx.Create(&mv).Error; err != nil {
			return err
		}
		if jobID != nil {
			if err := RecomputeJobCost(tx, *jobID); err != nil {
				return err
			}
		}
		return tx.First(&out, item.ID).Error
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// JobMaterialsCents is the cost of stock booked to a job: used minus returned.
func JobMaterialsCents(tx *gorm.DB, jobID uint) (int64, error) {
	var moves []models.StockMove
	if err := tx.Where("job_id = ? AND type IN ?", jobID, []string{models.MoveUsed, models.MoveReturned}).Find(&moves).Error; err != nil {
		return 0, err
	}
	var total int64
	for _, m := range moves {
		c := lineCents(toHundredths(m.Quantity), toCents(m.UnitCost))
		if m.Type == models.MoveUsed {
			total += c
		} else {
			total -= c
		}
	}
	return total, nil
}

// RecomputeJobCost sets materials_cost and total_cost from the ledger.
func RecomputeJobCost(tx *gorm.DB, jobID uint) error {
	var job models.InstallJob
	if err := tx.First(&job, jobID).Error; err != nil {
		return err
	}
	mat, err := JobMaterialsCents(tx, jobID)
	if err != nil {
		return err
	}
	return tx.Model(&models.InstallJob{}).Where("id = ?", jobID).Updates(map[string]interface{}{
		"materials_cost": centsToKES(mat), "total_cost": centsToKES(toCents(job.LabourCost) + mat),
	}).Error
}

// OntAction changes one serial-tracked unit: "faulty" (write it off),
// "take_back" (removed from a home, back in the store to be checked) or
// "good" (checked after a take-back, ready to fit again).
func OntAction(db *gorm.DB, a InstallActor, ontID uint, action, note string) (*models.OntUnit, error) {
	if !CanManageStock(a.Role) {
		return nil, installErr(403, "Only the owner can change ONT records.")
	}
	note = clip(note, 500)
	var out models.OntUnit
	err := db.Transaction(func(tx *gorm.DB) error {
		var ont models.OntUnit
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", ontID, a.OrgID).First(&ont).Error; err != nil {
			return installErr(404, "ONT not found.")
		}
		item, err := lockItem(tx, a.OrgID, ont.StockItemID)
		if err != nil {
			return err
		}
		mv := models.StockMove{OrganizationID: a.OrgID, StockItemID: item.ID, Quantity: 1, UnitCost: item.UnitCost,
			OntUnitID: &ont.ID, UserID: actorUID(a), Note: strings.TrimSpace("ONT " + ont.Serial + ". " + note)}
		var to string
		switch action {
		case "faulty":
			if ont.Status != models.OntInStock && ont.Status != models.OntReturned {
				return installErr(409, "Only an ONT in the store can be written off.")
			}
			if err := takeOut(tx, item, 100); err != nil {
				return err
			}
			mv.Type, mv.Direction, to = models.MoveWriteOff, -1, models.OntFaulty
		case "take_back":
			if ont.Status != models.OntInstalled {
				return installErr(409, "Only an installed ONT can be taken back.")
			}
			if err := putIn(tx, item, 100); err != nil {
				return err
			}
			mv.Type, mv.Direction, to = models.MoveReturned, 1, models.OntReturned
		case "good":
			if ont.Status != models.OntReturned {
				return installErr(409, "Only a taken-back ONT can be marked ready.")
			}
			to = models.OntInStock
		default:
			return installErr(422, "Unknown ONT action.")
		}
		if mv.Type != "" {
			if err := tx.Create(&mv).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&ont).Update("status", to).Error; err != nil {
			return err
		}
		return tx.Preload("StockItem").First(&out, ont.ID).Error
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// StockOnHandFromMoves is the ledger sum for an item (a check on on_hand).
func StockOnHandFromMoves(db *gorm.DB, itemID uint) float64 {
	var moves []models.StockMove
	db.Where("stock_item_id = ?", itemID).Find(&moves)
	var h int64
	for _, m := range moves {
		h += int64(m.Direction) * toHundredths(m.Quantity)
	}
	return hundredthsToQty(h)
}

// StockValueKES is on hand × average unit cost, to the cent.
func StockValueKES(onHand, unitCost float64) float64 {
	return centsToKES(lineCents(toHundredths(onHand), toCents(unitCost)))
}
