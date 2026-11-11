package handlers

import (
	"errors"
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

// Areas group zones (router sites) into estates/parts of town. The owner
// (super_admin) creates areas, puts zones in them, keeps one price list per
// area and pushes it to every zone. Area managers see their areas' zones.

// ---- scope helpers used by existing handlers ----

// areaOnlyZones is the zone list an area manager is limited to (nil for
// everyone else, meaning "no extra limit").
func areaOnlyZones(c *fiber.Ctx) []uint {
	if !middleware.IsAreaManager(c) {
		return nil
	}
	return middleware.AreaZoneIDs(c)
}

// keepAreaAlerts drops alerts outside an area manager's zones (and the
// ISP-wide summaries, which cover zones they can't see).
func keepAreaAlerts(c *fiber.Ctx, alerts []models.OpsAlert) []models.OpsAlert {
	ids := areaOnlyZones(c)
	if ids == nil {
		return alerts
	}
	in := map[uint]bool{}
	for _, id := range ids {
		in[id] = true
	}
	out := make([]models.OpsAlert, 0, len(alerts))
	for _, a := range alerts {
		if in[a.ZoneID] {
			out = append(out, a)
		}
	}
	return out
}

// areaPlanChanged reports whether an edit changes an area-linked plan (price,
// speed, limits, name, status…), which makes it the zone's own price.
func areaPlanChanged(before, after *models.Package) bool {
	return before.AreaPackageID != nil && !before.AreaOverride && services.AreaPlanDiffers(before, after)
}

// ---- who sees which areas ----

func isOwner(c *fiber.Ctx) bool {
	cl := middleware.GetClaims(c)
	return cl != nil && cl.Role == "super_admin"
}

// moneyZoneIDs is every zone whose money this caller may see, soft-deleted
// zones included: owner/finance the whole organization, an area manager the
// zones of their areas, a single-zone manager only their own zone. Used by
// both the area summaries and the money-by-area report.
func moneyZoneIDs(c *fiber.Ctx) ([]uint, error) {
	cl := middleware.GetClaims(c)
	ids, err := middleware.OrgZoneIDsWithDeleted(c)
	if err != nil {
		return nil, err
	}
	if cl.Role == "zone_manager" && !cl.AreaManager {
		if cl.ZoneID == nil {
			return []uint{}, nil
		}
		for _, id := range ids {
			if id == *cl.ZoneID {
				return []uint{id}, nil
			}
		}
		return []uint{}, nil
	}
	return ids, nil
}

func seesAreaMoney(c *fiber.Ctx) bool {
	cl := middleware.GetClaims(c)
	return cl != nil && (cl.Role == "super_admin" || cl.Role == "finance" || cl.Role == "zone_manager")
}

// visibleAreas returns the organization's areas the caller may see: all for
// the owner, finance and field agents (names only); an area manager their
// assigned areas; a zone manager the area of their zone.
func visibleAreas(c *fiber.Ctx) ([]models.Area, error) {
	cl := middleware.GetClaims(c)
	q := config.DB.Where("organization_id = ?", cl.OrganizationID)
	switch {
	case cl.AreaManager:
		q = q.Where("id IN (?)", config.DB.Model(&models.UserArea{}).Select("area_id").Where("user_id = ?", cl.UserID))
	case cl.Role == "zone_manager" && cl.ZoneID != nil:
		q = q.Where("id IN (?)", config.DB.Model(&models.Zone{}).Select("area_id").Where("id = ?", *cl.ZoneID))
	case cl.Role == "zone_manager":
		q = q.Where("1 = 0")
	}
	var areas []models.Area
	err := q.Order("name ASC").Find(&areas).Error
	return areas, err
}

func findVisibleArea(c *fiber.Ctx) (*models.Area, error) {
	id, err := strconv.ParseUint(c.Params("id"), 10, 64)
	if err != nil {
		return nil, utils.ErrorResponse(c, "Area not found.", "", fiber.StatusNotFound)
	}
	areas, err := visibleAreas(c)
	if err != nil {
		return nil, utils.ErrorResponse(c, "Could not load areas.", "", fiber.StatusInternalServerError)
	}
	for i := range areas {
		if areas[i].ID == uint(id) {
			return &areas[i], nil
		}
	}
	return nil, utils.ErrorResponse(c, "Area not found.", "", fiber.StatusNotFound)
}

// notOwner writes a 403 and returns true unless the caller is the owner.
// (utils.ErrorResponse returns nil after writing, so callers must test the
// bool, not an error.)
func notOwner(c *fiber.Ctx) bool {
	if !isOwner(c) {
		_ = utils.ErrorResponse(c, "Only the owner can change areas.", "Forbidden.", fiber.StatusForbidden)
		return true
	}
	return false
}

// ---- summaries ----

type areaZoneInfo struct {
	ID          uint       `json:"id"`
	Name        string     `json:"name"`
	Location    string     `json:"location"`
	Status      string     `json:"status"`
	RouterState string     `json:"router_state"` // online | offline | never_seen | disabled
	LastSeenAt  *time.Time `json:"last_seen_at"`
}

type areaSummary struct {
	models.Area
	ZonesCount     int            `json:"zones_count"`
	RoutersOnline  int            `json:"routers_online"`
	RoutersOffline int            `json:"routers_offline"`
	RoutersNever   int            `json:"routers_never_seen"`
	PlansCount     int64          `json:"plans_count"`
	Managers       []string       `json:"managers"`
	TakingsToday   *float64       `json:"takings_today,omitempty"`
	TakingsMonth   *float64       `json:"takings_month,omitempty"`
	Zones          []areaZoneInfo `json:"zones,omitempty"`
}

func routerState(z models.Zone, now time.Time) string {
	switch {
	case z.Status != "" && z.Status != "active":
		return "disabled"
	case z.LastSeenAt == nil:
		return "never_seen"
	case now.Sub(*z.LastSeenAt) > services.RouterOfflineAfter:
		return "offline"
	}
	return "online"
}

func buildAreaSummaries(c *fiber.Ctx, areas []models.Area, withZones bool) ([]areaSummary, error) {
	cl := middleware.GetClaims(c)
	now := time.Now()
	out := make([]areaSummary, 0, len(areas))
	if len(areas) == 0 {
		return out, nil
	}
	areaIDs := make([]uint, 0, len(areas))
	for _, a := range areas {
		areaIDs = append(areaIDs, a.ID)
	}
	var zones []models.Zone
	zq := config.DB.Select("id", "name", "location", "status", "last_seen_at", "area_id").
		Where("organization_id = ? AND area_id IN ?", cl.OrganizationID, areaIDs)
	zq = middleware.ScopeZones(c, zq, "id")
	if err := zq.Order("name ASC").Find(&zones).Error; err != nil {
		return nil, err
	}
	type cnt struct {
		AreaID uint
		N      int64
	}
	var plans []cnt
	config.DB.Model(&models.AreaPackage{}).Select("area_id, COUNT(*) AS n").Where("area_id IN ?", areaIDs).Group("area_id").Scan(&plans)
	planN := map[uint]int64{}
	for _, p := range plans {
		planN[p.AreaID] = p.N
	}
	type mgr struct {
		AreaID uint
		Name   string
	}
	var mgrs []mgr
	config.DB.Table("user_areas").Select("user_areas.area_id, users.name").
		Joins("JOIN users ON users.id = user_areas.user_id AND users.deleted_at IS NULL").
		Where("user_areas.area_id IN ? AND users.organization_id = ? AND users.role = ?", areaIDs, cl.OrganizationID, middleware.RoleAreaManager).
		Order("users.name").Scan(&mgrs)

	var today, month map[uint]float64
	if seesAreaMoney(c) {
		ids, err := moneyZoneIDs(c)
		if err != nil {
			return nil, err
		}
		t0 := services.StartOfDayEAT(now)
		m0 := time.Date(t0.Year(), t0.Month(), 1, 0, 0, 0, 0, services.EAT)
		today, err = takingsByArea(cl.OrganizationID, ids, areas, t0, t0.AddDate(0, 0, 1), now)
		if err != nil {
			return nil, err
		}
		month, err = takingsByArea(cl.OrganizationID, ids, areas, m0, t0.AddDate(0, 0, 1), now)
		if err != nil {
			return nil, err
		}
	}

	for _, a := range areas {
		s := areaSummary{Area: a, PlansCount: planN[a.ID], Managers: []string{}}
		for _, z := range zones {
			if z.AreaID == nil || *z.AreaID != a.ID {
				continue
			}
			st := routerState(z, now)
			s.ZonesCount++
			switch st {
			case "online":
				s.RoutersOnline++
			case "offline":
				s.RoutersOffline++
			case "never_seen":
				s.RoutersNever++
			}
			if withZones {
				s.Zones = append(s.Zones, areaZoneInfo{ID: z.ID, Name: z.Name, Location: z.Location, Status: z.Status, RouterState: st, LastSeenAt: z.LastSeenAt})
			}
		}
		if withZones && s.Zones == nil {
			s.Zones = []areaZoneInfo{}
		}
		for _, m := range mgrs {
			if m.AreaID == a.ID {
				s.Managers = append(s.Managers, m.Name)
			}
		}
		if today != nil {
			t, mo := today[a.ID], month[a.ID]
			s.TakingsToday, s.TakingsMonth = &t, &mo
		}
		out = append(out, s)
	}
	return out, nil
}

func takingsByArea(orgID uint, zoneIDs []uint, areas []models.Area, start, end, now time.Time) (map[uint]float64, error) {
	rep, err := services.BuildAreaMoney(config.DB, orgID, zoneIDs, areas, start, end, now)
	if err != nil {
		return nil, err
	}
	out := map[uint]float64{}
	for _, a := range rep.Areas {
		if a.AreaID != nil {
			out[*a.AreaID] = a.Takings
		}
	}
	return out, nil
}

// ---- area CRUD ----

// AreaIndex lists the areas the caller can see, with zones count, routers
// online/offline and (money roles) takings today and this month (EAT).
func AreaIndex(c *fiber.Ctx) error {
	areas, err := visibleAreas(c)
	if err != nil {
		return utils.ErrorResponse(c, "Could not load areas.", "", fiber.StatusInternalServerError)
	}
	out, err := buildAreaSummaries(c, areas, false)
	if err != nil {
		return utils.ErrorResponse(c, "Could not load areas.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, out, "")
}

// AreaShow is one area with its zones and price list.
func AreaShow(c *fiber.Ctx) error {
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	out, err := buildAreaSummaries(c, []models.Area{*area}, true)
	if err != nil || len(out) != 1 {
		return utils.ErrorResponse(c, "Could not load the area.", "", fiber.StatusInternalServerError)
	}
	var plans []models.AreaPackage
	config.DB.Where("area_id = ?", area.ID).Order("type ASC, price ASC, id ASC").Find(&plans)
	return utils.SuccessResponse(c, fiber.Map{"area": out[0], "plans": plans}, "")
}

type areaBody struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

func (b *areaBody) clean() string {
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return "Area name is required."
	}
	if len(b.Name) > 255 {
		return "Area name is too long."
	}
	if b.Description != nil {
		d := strings.TrimSpace(*b.Description)
		b.Description = &d
		if d == "" {
			b.Description = nil
		}
	}
	return ""
}

func areaNameTaken(orgID uint, name string, exceptID uint) bool {
	var n int64
	config.DB.Model(&models.Area{}).Where("organization_id = ? AND LOWER(name) = ? AND id <> ?", orgID, strings.ToLower(name), exceptID).Count(&n)
	return n > 0
}

// AreaStore creates an area (owner only).
func AreaStore(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	var body areaBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if msg := body.clean(); msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	cl := middleware.GetClaims(c)
	if areaNameTaken(cl.OrganizationID, body.Name, 0) {
		return utils.ErrorResponse(c, "You already have an area with that name.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	a := models.Area{OrganizationID: cl.OrganizationID, Name: body.Name, Description: body.Description}
	if err := config.DB.Create(&a).Error; err != nil {
		return utils.ErrorResponse(c, "Could not create the area.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, a, "Area created.", fiber.StatusCreated)
}

// AreaUpdate renames an area (owner only).
func AreaUpdate(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	var body areaBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if msg := body.clean(); msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if areaNameTaken(area.OrganizationID, body.Name, area.ID) {
		return utils.ErrorResponse(c, "You already have an area with that name.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if err := config.DB.Model(area).Updates(map[string]interface{}{"name": body.Name, "description": body.Description}).Error; err != nil {
		return utils.ErrorResponse(c, "Could not save the area.", "", fiber.StatusInternalServerError)
	}
	config.DB.First(area, area.ID)
	return utils.SuccessResponse(c, area, "Area saved.")
}

// AreaDestroy removes an area. Its zones keep their plans and move to "No area".
func AreaDestroy(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	if err := services.DeleteArea(config.DB, area.OrganizationID, area.ID); err != nil {
		return utils.ErrorResponse(c, "Could not remove the area.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Area removed. Its zones keep their plans.")
}

// ---- zones in an area ----

// AreaAssignZones puts zones in the area and applies its price list to them.
func AreaAssignZones(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	var body struct {
		ZoneIDs []uint `json:"zone_ids"`
	}
	if err := c.BodyParser(&body); err != nil || len(body.ZoneIDs) == 0 {
		return utils.ErrorResponse(c, "Pick at least one zone.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	res, err := services.AssignZonesToArea(config.DB, area.OrganizationID, area.ID, body.ZoneIDs)
	if errors.Is(err, services.ErrAreaNotFound) {
		return utils.ErrorResponse(c, "One of those zones is not yours.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if err != nil {
		return utils.ErrorResponse(c, "Could not add the zones.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, res, "Zones added and area prices applied.")
}

// AreaRemoveZone takes a zone out of the area; its plans stay on sale as its own.
func AreaRemoveZone(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	zoneID, perr := strconv.ParseUint(c.Params("zoneId"), 10, 64)
	var z models.Zone
	if perr != nil || config.DB.Where("id = ? AND organization_id = ? AND area_id = ?", zoneID, area.OrganizationID, area.ID).First(&z).Error != nil {
		return utils.ErrorResponse(c, "That zone is not in this area.", "", fiber.StatusNotFound)
	}
	if err := services.DetachZoneFromArea(config.DB, area.OrganizationID, z.ID); err != nil {
		return utils.ErrorResponse(c, "Could not remove the zone.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Zone removed from the area. Its plans stay on sale.")
}

// ---- price list ----

type areaPlanBody struct {
	Name              string  `json:"name"`
	Type              string  `json:"type"`
	Category          string  `json:"category"`
	DeviceLimit       int     `json:"device_limit"`
	Price             float64 `json:"price"`
	TimeLimitMinutes  *int    `json:"time_limit_minutes"`
	DataLimitMB       *int    `json:"data_limit_mb"`
	FairUseKbps       *int    `json:"fair_use_kbps"`
	SpeedUploadKbps   int     `json:"speed_upload_kbps"`
	SpeedDownloadKbps int     `json:"speed_download_kbps"`
	BillingCycle      string  `json:"billing_cycle"`
	Status            string  `json:"status"`
}

func (b *areaPlanBody) toTemplate(t *models.AreaPackage) string {
	t.Name = strings.TrimSpace(b.Name)
	t.Type = b.Type
	t.Category = b.Category
	if t.Category == "" {
		t.Category = "single"
	}
	t.DeviceLimit = b.DeviceLimit
	if t.DeviceLimit <= 0 {
		t.DeviceLimit = 1
	}
	t.Price = b.Price
	t.TimeLimitMinutes, t.DataLimitMB, t.FairUseKbps = nilIfZero(b.TimeLimitMinutes), nilIfZero(b.DataLimitMB), nilIfZero(b.FairUseKbps)
	t.SpeedUploadKbps, t.SpeedDownloadKbps = b.SpeedUploadKbps, b.SpeedDownloadKbps
	t.BillingCycle = b.BillingCycle
	t.Status = b.Status
	if t.Status == "" {
		t.Status = "active"
	}
	if t.Category != "single" && t.Category != "multi" {
		return "Category must be single or multi."
	}
	if t.SpeedDownloadKbps <= 0 || t.SpeedUploadKbps <= 0 {
		return "Set the download and upload speed."
	}
	p := models.Package{}
	services.AreaPlanFields(&p, t)
	return validatePackage(&p)
}

func nilIfZero(p *int) *int {
	if p == nil || *p == 0 {
		return nil
	}
	return p
}

// AreaPlanIndex is the area's price list.
func AreaPlanIndex(c *fiber.Ctx) error {
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	var plans []models.AreaPackage
	config.DB.Where("area_id = ?", area.ID).Order("type ASC, price ASC, id ASC").Find(&plans)
	return utils.SuccessResponse(c, plans, "")
}

// AreaPlanStore adds a plan to the price list (not pushed until "Apply").
func AreaPlanStore(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	var body areaPlanBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	t := models.AreaPackage{AreaID: area.ID, OrganizationID: area.OrganizationID}
	if msg := body.toTemplate(&t); msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if err := config.DB.Create(&t).Error; err != nil {
		return utils.ErrorResponse(c, "Could not add the plan.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, t, "Plan added. Apply to zones to start selling it.", fiber.StatusCreated)
}

func findAreaPlan(c *fiber.Ctx, area *models.Area) (*models.AreaPackage, error) {
	var t models.AreaPackage
	if err := config.DB.Where("id = ? AND area_id = ? AND organization_id = ?", c.Params("planId"), area.ID, area.OrganizationID).First(&t).Error; err != nil {
		return nil, utils.ErrorResponse(c, "Plan not found.", "", fiber.StatusNotFound)
	}
	return &t, nil
}

// AreaPlanUpdate edits a plan on the price list (not pushed until "Apply").
func AreaPlanUpdate(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	t, err := findAreaPlan(c, area)
	if t == nil {
		return err
	}
	var body areaPlanBody
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if msg := body.toTemplate(t); msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if err := config.DB.Model(t).Select("name", "type", "category", "device_limit", "price", "time_limit_minutes", "data_limit_mb",
		"fair_use_kbps", "speed_upload_kbps", "speed_download_kbps", "billing_cycle", "status").Updates(t).Error; err != nil {
		return utils.ErrorResponse(c, "Could not save the plan.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, t, "Plan saved. Apply to zones to push the change.")
}

// AreaPlanDestroy removes a plan from the list and stops selling it in the
// zones that follow the area price (never hard-deletes zone packages).
func AreaPlanDestroy(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	t, err := findAreaPlan(c, area)
	if t == nil {
		return err
	}
	off, err := services.DeleteAreaTemplate(config.DB, area.OrganizationID, area.ID, t.ID)
	if err != nil {
		return utils.ErrorResponse(c, "Could not remove the plan.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, fiber.Map{"turned_off": off}, "Plan removed from the area.")
}

// AreaApplyPreview shows what "Apply to all zones" would change.
func AreaApplyPreview(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	res, err := services.ApplyAreaPriceList(config.DB, area.OrganizationID, area.ID, 0, true)
	if err != nil {
		return utils.ErrorResponse(c, "Could not work out the changes.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, res, "")
}

// AreaApply pushes the price list to every zone in the area.
func AreaApply(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	area, err := findVisibleArea(c)
	if area == nil {
		return err
	}
	res, err := services.ApplyAreaPriceList(config.DB, area.OrganizationID, area.ID, 0, false)
	if err != nil {
		return utils.ErrorResponse(c, "Could not apply the prices.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, res, "Area prices applied.")
}

// PackageUseAreaPrice clears a zone's own price on an area-linked plan and
// puts the area price back.
func PackageUseAreaPrice(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var pkg models.Package
	if err := config.DB.Where("zone_id IN (?)", orgZoneIDs).First(&pkg, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Package not found.", "", fiber.StatusNotFound)
	}
	if !packageZoneAllowed(c, pkg.ZoneID) {
		return utils.ErrorResponse(c, "You can only manage packages in your own zone.", "", fiber.StatusForbidden)
	}
	var zone models.Zone
	if pkg.AreaPackageID == nil || config.DB.First(&zone, pkg.ZoneID).Error != nil || zone.AreaID == nil {
		return utils.ErrorResponse(c, "This plan does not come from an area price list.", "", fiber.StatusUnprocessableEntity)
	}
	var tpl models.AreaPackage
	if config.DB.Where("id = ? AND area_id = ?", *pkg.AreaPackageID, *zone.AreaID).First(&tpl).Error != nil {
		return utils.ErrorResponse(c, "The area no longer has this plan.", "", fiber.StatusUnprocessableEntity)
	}
	if err := config.DB.Model(&models.Package{}).Where("id = ?", pkg.ID).Update("area_override", false).Error; err != nil {
		return utils.ErrorResponse(c, "Could not reset the price.", "", fiber.StatusInternalServerError)
	}
	if _, err := services.ApplyAreaPriceList(config.DB, zone.OrganizationID, *zone.AreaID, zone.ID, false); err != nil {
		return utils.ErrorResponse(c, "Could not apply the area price.", "", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zone").First(&pkg, pkg.ID)
	return utils.SuccessResponse(c, pkg, "Area price is back on this plan.")
}

// ---- money by area ----

// ReportAreas is money per area and per zone inside it for a range of EAT
// days (?from=YYYY-MM-DD&to=YYYY-MM-DD, default this month), plus "No area".
// Deleted zones that took money in the range are included.
func ReportAreas(c *fiber.Ctx) error {
	cl := middleware.GetClaims(c)
	now := time.Now()
	today := now.In(utils.EAT)
	from := c.Query("from", time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, utils.EAT).Format("2006-01-02"))
	to := c.Query("to", today.Format("2006-01-02"))
	start, end, err := eatDayRange(from, to)
	if err != nil {
		return utils.ErrorResponse(c, "from and to must be YYYY-MM-DD.", "", fiber.StatusUnprocessableEntity)
	}
	if !start.Before(end) {
		return utils.ErrorResponse(c, "from must be on or before to.", "", fiber.StatusUnprocessableEntity)
	}
	if end.Sub(start) > 400*24*time.Hour {
		return utils.ErrorResponse(c, "Pick at most 400 days.", "", fiber.StatusUnprocessableEntity)
	}
	ids, err := moneyZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	areas, err := visibleAreas(c)
	if err != nil {
		return utils.ErrorResponse(c, "Could not load areas.", "", fiber.StatusInternalServerError)
	}
	if a := c.Query("area_id"); a != "" {
		aid, _ := strconv.ParseUint(a, 10, 64)
		var keep []models.Area
		for _, ar := range areas {
			if ar.ID == uint(aid) {
				keep = append(keep, ar)
			}
		}
		if len(keep) == 0 {
			return utils.ErrorResponse(c, "Area not found.", "", fiber.StatusNotFound)
		}
		areas = keep
		var inArea []uint
		config.DB.Unscoped().Model(&models.Zone{}).Where("id IN ? AND area_id = ?", ids, aid).Pluck("id", &inArea)
		ids = inArea
	}
	rep, err := services.BuildAreaMoney(config.DB, cl.OrganizationID, ids, areas, start, end, now)
	if err != nil {
		return utils.ErrorResponse(c, "Could not load the report.", "", fiber.StatusInternalServerError)
	}
	rep.From, rep.To = from, to
	return utils.SuccessResponse(c, rep, "")
}

// ---- area managers ----

// UserAreasIndex lists every area-manager assignment in the organization.
func UserAreasIndex(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	cl := middleware.GetClaims(c)
	var rows []models.UserArea
	config.DB.Where("user_id IN (?)", config.DB.Model(&models.User{}).Select("id").Where("organization_id = ?", cl.OrganizationID)).
		Order("user_id, area_id").Find(&rows)
	return utils.SuccessResponse(c, rows, "")
}

// UserAreasShow lists the areas of one user.
func UserAreasShow(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	cl := middleware.GetClaims(c)
	var u models.User
	if err := config.DB.Where("organization_id = ?", cl.OrganizationID).First(&u, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "", fiber.StatusNotFound)
	}
	ids := []uint{}
	config.DB.Model(&models.UserArea{}).Where("user_id = ?", u.ID).Pluck("area_id", &ids)
	return utils.SuccessResponse(c, fiber.Map{"user_id": u.ID, "area_ids": ids}, "")
}

// UserAreasUpdate sets the areas of an area manager (replaces the list).
func UserAreasUpdate(c *fiber.Ctx) error {
	if notOwner(c) {
		return nil
	}
	cl := middleware.GetClaims(c)
	var u models.User
	if err := config.DB.Where("organization_id = ?", cl.OrganizationID).First(&u, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "User not found.", "", fiber.StatusNotFound)
	}
	var body struct {
		AreaIDs []uint `json:"area_ids"`
	}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	ids := []uint{}
	seen := map[uint]bool{}
	for _, id := range body.AreaIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 && u.Role != middleware.RoleAreaManager {
		return utils.ErrorResponse(c, "Only an area manager can be given areas. Change the role first.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if len(ids) > 0 {
		var n int64
		config.DB.Model(&models.Area{}).Where("id IN ? AND organization_id = ?", ids, cl.OrganizationID).Count(&n)
		if int(n) != len(ids) {
			return utils.ErrorResponse(c, "One of those areas is not yours.", "Validation failed.", fiber.StatusUnprocessableEntity)
		}
	}
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", u.ID).Delete(&models.UserArea{}).Error; err != nil {
			return err
		}
		for _, id := range ids {
			if err := tx.Create(&models.UserArea{UserID: u.ID, AreaID: id}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return utils.ErrorResponse(c, "Could not save the areas.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, fiber.Map{"user_id": u.ID, "area_ids": ids}, "Areas saved.")
}
