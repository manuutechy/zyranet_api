package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

type stockItemOut struct {
	models.StockItem
	Value float64 `json:"value"`
	Low   bool    `json:"low"`
}

func stockItemView(item models.StockItem) stockItemOut {
	return stockItemOut{StockItem: item, Value: services.StockValueKES(item.OnHand, item.UnitCost), Low: item.Active && item.ReorderLevel > 0 && item.OnHand <= item.ReorderLevel}
}

// StockItemIndex lists stock with on hand, value and a low-stock flag.
// ?low=1 for the low-stock list only.
func StockItemIndex(c *fiber.Ctx) error {
	a := installActor(c)
	if !services.CanViewStock(a.Role) {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	var items []models.StockItem
	if err := config.DB.Where("organization_id = ?", a.OrgID).Order("active DESC, name ASC").Find(&items).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to load stock.", "", fiber.StatusInternalServerError)
	}
	out := []stockItemOut{}
	var totalCents int64
	low := 0
	for _, it := range items {
		v := stockItemView(it)
		if c.Query("low") == "1" && !v.Low {
			continue
		}
		totalCents += int64(v.Value*100 + 0.5)
		if v.Low {
			low++
		}
		out = append(out, v)
	}
	return c.JSON(fiber.Map{"success": true, "data": out,
		"totals": fiber.Map{"value": float64(totalCents) / 100, "low_count": low}})
}

// StockItemStore adds an item.
func StockItemStore(c *fiber.Ctx) error {
	var in services.StockItemInput
	if err := c.BodyParser(&in); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	item, err := services.CreateStockItem(config.DB, installActor(c), in)
	if err != nil {
		return installFail(c, err)
	}
	return utils.SuccessResponse(c, stockItemView(*item), "Item added.", fiber.StatusCreated)
}

// StockItemUpdate edits an item.
func StockItemUpdate(c *fiber.Ctx) error {
	var in services.StockItemInput
	if err := c.BodyParser(&in); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	item, err := services.UpdateStockItem(config.DB, installActor(c), paramID(c, "id"), in)
	if err != nil {
		return installFail(c, err)
	}
	return utils.SuccessResponse(c, stockItemView(*item), "Saved.")
}

// StockPurchase: {quantity, unit_cost, serials[], note}.
func StockPurchase(c *fiber.Ctx) error {
	var in services.PurchaseInput
	if err := c.BodyParser(&in); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	a := installActor(c)
	item, err := services.PurchaseStock(config.DB, a, paramID(c, "id"), in)
	if err != nil {
		return installFail(c, err)
	}
	auditAgent(c, "stock_purchase", "StockItem", item.ID, fiber.Map{"quantity": in.Quantity, "unit_cost": in.UnitCost, "serials": len(in.Serials)})
	return utils.SuccessResponse(c, stockItemView(*item), "Purchase recorded.")
}

// StockChange: POST /stock/items/:id/{adjust|write-off|return} {quantity, note, job_id?}.
func StockChange(kind string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var in services.StockChangeInput
		if err := c.BodyParser(&in); err != nil {
			return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
		}
		item, err := services.ChangeStock(config.DB, installActor(c), paramID(c, "id"), kind, in)
		if err != nil {
			return installFail(c, err)
		}
		auditAgent(c, "stock_"+kind, "StockItem", item.ID, in)
		return utils.SuccessResponse(c, stockItemView(*item), "Stock updated.")
	}
}

// StockMoves is an item's ledger, newest first.
func StockMoves(c *fiber.Ctx) error {
	a := installActor(c)
	if !services.CanViewStock(a.Role) {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	var item models.StockItem
	if err := config.DB.Where("id = ? AND organization_id = ?", paramID(c, "id"), a.OrgID).First(&item).Error; err != nil {
		return utils.ErrorResponse(c, "Stock item not found.", "", fiber.StatusNotFound)
	}
	page, perPage := utils.ParsePage(c)
	var total int64
	q := config.DB.Model(&models.StockMove{}).Where("stock_moves.stock_item_id = ? AND stock_moves.organization_id = ?", item.ID, a.OrgID)
	q.Count(&total)
	type moveOut struct {
		models.StockMove
		UserName string `json:"user_name"`
	}
	var moves []moveOut
	q.Select("stock_moves.*, users.name AS user_name").Joins("LEFT JOIN users ON users.id = stock_moves.user_id").
		Order("stock_moves.id DESC").Offset(utils.Offset(page, perPage)).Limit(perPage).Scan(&moves)
	if moves == nil {
		moves = []moveOut{}
	}
	return c.JSON(fiber.Map{"success": true, "data": moves, "item": stockItemView(item),
		"meta": fiber.Map{"total": total, "page": page, "per_page": perPage}})
}

// OntIndex lists serial-tracked units. ?status=&q=
func OntIndex(c *fiber.Ctx) error {
	a := installActor(c)
	if !services.CanViewStock(a.Role) {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	q := config.DB.Model(&models.OntUnit{}).Where("organization_id = ?", a.OrgID)
	if s := strings.TrimSpace(c.Query("status")); s != "" && s != "all" {
		q = q.Where("status = ?", s)
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		q = q.Where("serial LIKE ?", "%"+strings.ToUpper(s)+"%")
	}
	var total int64
	q.Count(&total)
	page, perPage := utils.ParsePage(c)
	var onts []models.OntUnit
	q.Preload("StockItem").Order("CASE status WHEN 'returned' THEN 0 WHEN 'in_stock' THEN 1 WHEN 'installed' THEN 2 ELSE 3 END, serial").
		Offset(utils.Offset(page, perPage)).Limit(perPage).Find(&onts)
	var counts []struct {
		Status string
		N      int
	}
	config.DB.Model(&models.OntUnit{}).Select("status, COUNT(*) AS n").Where("organization_id = ?", a.OrgID).Group("status").Scan(&counts)
	cm := map[string]int{}
	for _, r := range counts {
		cm[r.Status] = r.N
	}
	return c.JSON(fiber.Map{"success": true, "data": onts, "counts": cm,
		"meta": fiber.Map{"total": total, "page": page, "per_page": perPage}})
}

// OntActionHandler: POST /stock/onts/:id/{faulty|take-back|good} {note}.
func OntActionHandler(action string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var b struct {
			Note string `json:"note"`
		}
		_ = c.BodyParser(&b)
		ont, err := services.OntAction(config.DB, installActor(c), paramID(c, "id"), action, b.Note)
		if err != nil {
			return installFail(c, err)
		}
		auditAgent(c, "ont_"+action, "OntUnit", ont.ID, b)
		return utils.SuccessResponse(c, ont, "ONT updated.")
	}
}
