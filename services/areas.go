package services

import (
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

// ---------------------------------------------------------------------------
// Area price list: plan templates (area_packages) copied into every zone of
// the area as ordinary zone packages linked by packages.area_package_id.
//
// Rules:
//   - A zone package with area_override = true (the zone set its own price)
//     is never touched by "apply".
//   - A linked package the zone deleted stays deleted (skipped).
//   - If a zone has no linked copy yet but already sells an unlinked plan with
//     the same name and type, that plan is adopted (linked and updated)
//     instead of creating a duplicate next to it.
//   - A deleted template turns its linked zone packages inactive; packages are
//     never hard-deleted (payments reference them).
//   - Everything runs in one transaction with the area row locked, so two
//     clicks on "Apply" can't interleave; running it twice changes nothing
//     the second time.
// ---------------------------------------------------------------------------

// AreaPlanFields copies every template-managed field from a template onto a
// zone package.
func AreaPlanFields(p *models.Package, t *models.AreaPackage) {
	p.Name = t.Name
	p.Type = t.Type
	p.Category = t.Category
	p.DeviceLimit = t.DeviceLimit
	p.Price = t.Price
	p.BillingCycle = t.BillingCycle
	p.TimeLimitMinutes = cloneInt(t.TimeLimitMinutes)
	p.DataLimitMB = cloneInt(t.DataLimitMB)
	p.FairUseKbps = cloneInt(t.FairUseKbps)
	p.SpeedUploadKbps = t.SpeedUploadKbps
	p.SpeedDownloadKbps = t.SpeedDownloadKbps
	p.Status = t.Status
}

// AreaPlanDiffers reports whether any template-managed field of a and b differ.
func AreaPlanDiffers(a, b *models.Package) bool {
	return a.Name != b.Name || a.Type != b.Type || a.Category != b.Category ||
		a.DeviceLimit != b.DeviceLimit || math.Abs(a.Price-b.Price) > 0.004 ||
		a.BillingCycle != b.BillingCycle || !eqIntPtr(a.TimeLimitMinutes, b.TimeLimitMinutes) ||
		!eqIntPtr(a.DataLimitMB, b.DataLimitMB) || !eqIntPtr(a.FairUseKbps, b.FairUseKbps) ||
		a.SpeedUploadKbps != b.SpeedUploadKbps || a.SpeedDownloadKbps != b.SpeedDownloadKbps ||
		normStatus(a.Status) != normStatus(b.Status)
}

func normStatus(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

func eqIntPtr(a, b *int) bool {
	av, bv := 0, 0
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// AreaApplyChange is one line of the "what will change" preview.
type AreaApplyChange struct {
	ZoneID        uint   `json:"zone_id"`
	ZoneName      string `json:"zone_name"`
	AreaPackageID uint   `json:"area_package_id"`
	PlanName      string `json:"plan_name"`
	PackageID     *uint  `json:"package_id"`
	// Action: add | update | same | own_price | removed_here | turn_off
	Action   string   `json:"action"`
	OldPrice *float64 `json:"old_price,omitempty"`
	NewPrice float64  `json:"new_price"`
	Changes  []string `json:"changes,omitempty"`
}

// AreaApplyResult sums up an apply (or its preview).
type AreaApplyResult struct {
	AreaID  uint              `json:"area_id"`
	DryRun  bool              `json:"dry_run"`
	Added   int               `json:"added"`
	Updated int               `json:"updated"`
	Same    int               `json:"same"`
	Skipped int               `json:"skipped"` // own price or removed by the zone
	Off     int               `json:"turned_off"`
	Changes []AreaApplyChange `json:"changes"`
}

// ErrAreaNotFound is returned when the area isn't the organization's.
var ErrAreaNotFound = errors.New("area not found")

// ApplyAreaPriceList pushes the area's price list into its zones (or only
// into onlyZoneID when non-zero). dryRun computes the same result without
// writing anything.
func ApplyAreaPriceList(db *gorm.DB, orgID, areaID, onlyZoneID uint, dryRun bool) (*AreaApplyResult, error) {
	res := &AreaApplyResult{AreaID: areaID, DryRun: dryRun, Changes: []AreaApplyChange{}}
	run := func(tx *gorm.DB) error {
		var area models.Area
		q := tx
		if !dryRun {
			q = tx.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.Where("id = ? AND organization_id = ?", areaID, orgID).First(&area).Error; err != nil {
			return ErrAreaNotFound
		}
		var zones []models.Zone
		zq := tx.Select("id", "name").Where("organization_id = ? AND area_id = ?", orgID, areaID)
		if onlyZoneID != 0 {
			zq = zq.Where("id = ?", onlyZoneID)
		}
		if err := zq.Order("name ASC").Find(&zones).Error; err != nil {
			return err
		}
		var templates []models.AreaPackage
		if err := tx.Where("area_id = ? AND organization_id = ?", areaID, orgID).Order("price ASC, id ASC").Find(&templates).Error; err != nil {
			return err
		}
		var deletedTemplates []uint
		tx.Unscoped().Model(&models.AreaPackage{}).
			Where("area_id = ? AND organization_id = ? AND deleted_at IS NOT NULL", areaID, orgID).
			Pluck("id", &deletedTemplates)

		for _, z := range zones {
			for i := range templates {
				t := &templates[i]
				ch, err := applyTemplateToZone(tx, z, t, dryRun)
				if err != nil {
					return err
				}
				res.count(ch)
			}
			if len(deletedTemplates) > 0 {
				chs, err := turnOffDeletedTemplatePlans(tx, z, deletedTemplates, dryRun)
				if err != nil {
					return err
				}
				for _, ch := range chs {
					res.count(ch)
				}
			}
		}
		return nil
	}
	var err error
	if dryRun {
		err = run(db)
	} else {
		err = db.Transaction(run)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (r *AreaApplyResult) count(ch AreaApplyChange) {
	switch ch.Action {
	case "add":
		r.Added++
	case "update":
		r.Updated++
	case "same":
		r.Same++
	case "own_price", "removed_here":
		r.Skipped++
	case "turn_off":
		r.Off++
	}
	r.Changes = append(r.Changes, ch)
}

func applyTemplateToZone(tx *gorm.DB, z models.Zone, t *models.AreaPackage, dryRun bool) (AreaApplyChange, error) {
	ch := AreaApplyChange{ZoneID: z.ID, ZoneName: z.Name, AreaPackageID: t.ID, PlanName: t.Name, NewPrice: t.Price}

	var pkg models.Package
	err := tx.Unscoped().Where("zone_id = ? AND area_package_id = ?", z.ID, t.ID).Order("id ASC").First(&pkg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Adopt an unlinked plan with the same name and type, else add one.
		err = tx.Where("zone_id = ? AND area_package_id IS NULL AND LOWER(name) = ? AND type = ?",
			z.ID, strings.ToLower(strings.TrimSpace(t.Name)), t.Type).Order("id ASC").First(&pkg).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			ch.Action = "add"
			if !dryRun {
				p := models.Package{ZoneID: z.ID, AreaPackageID: &t.ID}
				AreaPlanFields(&p, t)
				if err := tx.Create(&p).Error; err != nil {
					return ch, err
				}
				ch.PackageID = &p.ID
			}
			return ch, nil
		} else if err != nil {
			return ch, err
		}
		// Adopted: linked from now on, even if nothing else changes.
		if !dryRun {
			if err := tx.Model(&models.Package{}).Where("id = ?", pkg.ID).
				Updates(map[string]interface{}{"area_package_id": t.ID, "area_override": false}).Error; err != nil {
				return ch, err
			}
		}
		pkg.AreaOverride = false
	} else if err != nil {
		return ch, err
	}

	id := pkg.ID
	ch.PackageID = &id
	old := pkg.Price
	ch.OldPrice = &old
	if pkg.DeletedAt.Valid {
		ch.Action = "removed_here"
		return ch, nil
	}
	if pkg.AreaOverride {
		ch.Action = "own_price"
		return ch, nil
	}
	want := pkg
	AreaPlanFields(&want, t)
	if !AreaPlanDiffers(&pkg, &want) {
		ch.Action = "same"
		return ch, nil
	}
	ch.Action = "update"
	ch.Changes = describePlanChanges(&pkg, &want)
	if !dryRun {
		if err := tx.Model(&models.Package{}).Where("id = ?", pkg.ID).Updates(planUpdateMap(&want)).Error; err != nil {
			return ch, err
		}
	}
	return ch, nil
}

func turnOffDeletedTemplatePlans(tx *gorm.DB, z models.Zone, templateIDs []uint, dryRun bool) ([]AreaApplyChange, error) {
	var pkgs []models.Package
	if err := tx.Where("zone_id = ? AND area_package_id IN ? AND status = ? AND area_override = ?", z.ID, templateIDs, "active", false).
		Find(&pkgs).Error; err != nil {
		return nil, err
	}
	out := make([]AreaApplyChange, 0, len(pkgs))
	for _, p := range pkgs {
		id := p.ID
		price := p.Price
		out = append(out, AreaApplyChange{ZoneID: z.ID, ZoneName: z.Name, AreaPackageID: *p.AreaPackageID, PlanName: p.Name,
			PackageID: &id, Action: "turn_off", OldPrice: &price, NewPrice: p.Price, Changes: []string{"stops selling"}})
		if !dryRun {
			if err := tx.Model(&models.Package{}).Where("id = ?", p.ID).Update("status", "inactive").Error; err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// planUpdateMap is the column map for an Updates call (a map, so zero values
// such as a cleared data cap are written too).
func planUpdateMap(p *models.Package) map[string]interface{} {
	return map[string]interface{}{
		"name": p.Name, "type": p.Type, "category": p.Category, "device_limit": p.DeviceLimit,
		"price": p.Price, "billing_cycle": p.BillingCycle, "time_limit_minutes": p.TimeLimitMinutes,
		"data_limit_mb": p.DataLimitMB, "fair_use_kbps": p.FairUseKbps,
		"speed_upload_kbps": p.SpeedUploadKbps, "speed_download_kbps": p.SpeedDownloadKbps,
		"status": normStatus(p.Status),
	}
}

func describePlanChanges(a, b *models.Package) []string {
	var out []string
	if math.Abs(a.Price-b.Price) > 0.004 {
		out = append(out, fmt.Sprintf("price KES %s → %s", fmtKES(a.Price), fmtKES(b.Price)))
	}
	if a.SpeedDownloadKbps != b.SpeedDownloadKbps || a.SpeedUploadKbps != b.SpeedUploadKbps {
		out = append(out, fmt.Sprintf("speed %s/%s → %s/%s", fmtSpeed(a.SpeedDownloadKbps), fmtSpeed(a.SpeedUploadKbps), fmtSpeed(b.SpeedDownloadKbps), fmtSpeed(b.SpeedUploadKbps)))
	}
	if a.Name != b.Name {
		out = append(out, fmt.Sprintf("name %q → %q", a.Name, b.Name))
	}
	if a.BillingCycle != b.BillingCycle {
		out = append(out, fmt.Sprintf("billing %s → %s", a.BillingCycle, b.BillingCycle))
	}
	if !eqIntPtr(a.TimeLimitMinutes, b.TimeLimitMinutes) {
		out = append(out, "time limit")
	}
	if !eqIntPtr(a.DataLimitMB, b.DataLimitMB) || !eqIntPtr(a.FairUseKbps, b.FairUseKbps) {
		out = append(out, "data cap")
	}
	if a.Type != b.Type || a.Category != b.Category || a.DeviceLimit != b.DeviceLimit {
		out = append(out, "plan type / devices")
	}
	if normStatus(a.Status) != normStatus(b.Status) {
		out = append(out, fmt.Sprintf("status %s → %s", normStatus(a.Status), normStatus(b.Status)))
	}
	return out
}

func fmtKES(v float64) string {
	if v == math.Trunc(v) {
		return commaInt(int64(v))
	}
	return fmt.Sprintf("%s.%02d", commaInt(int64(v)), int64(math.Round((v-math.Trunc(v))*100)))
}

func commaInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return "-" + commaInt(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func fmtSpeed(kbps int) string {
	// 1 Mbps = 1024 kbps, as the admin's package pages show it.
	if kbps >= 1024 && kbps%1024 == 0 {
		return fmt.Sprintf("%d Mbps", kbps/1024)
	}
	if kbps >= 1024 {
		return fmt.Sprintf("%.1f Mbps", float64(kbps)/1024)
	}
	return fmt.Sprintf("%d kbps", kbps)
}

// DeleteAreaTemplate soft-deletes a template and turns off its linked zone
// packages that still follow the area (own-price ones keep selling).
func DeleteAreaTemplate(db *gorm.DB, orgID, areaID, templateID uint) (int64, error) {
	var off int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var t models.AreaPackage
		if err := tx.Where("id = ? AND area_id = ? AND organization_id = ?", templateID, areaID, orgID).First(&t).Error; err != nil {
			return ErrAreaNotFound
		}
		if err := tx.Delete(&t).Error; err != nil {
			return err
		}
		res := tx.Model(&models.Package{}).
			Where("area_package_id = ? AND area_override = ? AND status = ?", t.ID, false, "active").
			Where("zone_id IN (?)", tx.Unscoped().Model(&models.Zone{}).Select("id").Where("organization_id = ?", orgID)).
			Update("status", "inactive")
		off = res.RowsAffected
		return res.Error
	})
	return off, err
}

// DetachZoneFromArea removes a zone from its area. Its area-linked plans stay
// on sale unchanged and simply become the zone's own plans.
func DetachZoneFromArea(tx *gorm.DB, orgID, zoneID uint) error {
	if err := tx.Model(&models.Package{}).Where("zone_id = ? AND area_package_id IS NOT NULL", zoneID).
		Updates(map[string]interface{}{"area_package_id": nil, "area_override": false}).Error; err != nil {
		return err
	}
	return tx.Unscoped().Model(&models.Zone{}).Where("id = ? AND organization_id = ?", zoneID, orgID).
		Update("area_id", nil).Error
}

// AssignZonesToArea moves zones into an area (detaching them from any other
// area first) and applies the area's price list to them, in one transaction.
func AssignZonesToArea(db *gorm.DB, orgID, areaID uint, zoneIDs []uint) (*AreaApplyResult, error) {
	total := &AreaApplyResult{AreaID: areaID, Changes: []AreaApplyChange{}}
	err := db.Transaction(func(tx *gorm.DB) error {
		var area models.Area
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND organization_id = ?", areaID, orgID).First(&area).Error; err != nil {
			return ErrAreaNotFound
		}
		var zones []models.Zone
		if err := tx.Where("organization_id = ? AND id IN ?", orgID, zoneIDs).Find(&zones).Error; err != nil {
			return err
		}
		if len(zones) != len(uniqueIDs(zoneIDs)) {
			return ErrAreaNotFound
		}
		for _, z := range zones {
			if z.AreaID != nil && *z.AreaID == areaID {
				continue
			}
			if z.AreaID != nil {
				if err := DetachZoneFromArea(tx, orgID, z.ID); err != nil {
					return err
				}
			}
			if err := tx.Model(&models.Zone{}).Where("id = ?", z.ID).Update("area_id", areaID).Error; err != nil {
				return err
			}
		}
		for _, z := range zones {
			r, err := applyInTx(tx, orgID, areaID, z.ID)
			if err != nil {
				return err
			}
			total.Added += r.Added
			total.Updated += r.Updated
			total.Same += r.Same
			total.Skipped += r.Skipped
			total.Off += r.Off
			total.Changes = append(total.Changes, r.Changes...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return total, nil
}

// applyInTx runs the apply inside an existing transaction (no nested lock).
func applyInTx(tx *gorm.DB, orgID, areaID, zoneID uint) (*AreaApplyResult, error) {
	return ApplyAreaPriceList(tx.Session(&gorm.Session{}), orgID, areaID, zoneID, false)
}

func uniqueIDs(ids []uint) []uint {
	seen := map[uint]bool{}
	out := []uint{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// DeleteArea soft-deletes an area. Its zones (deleted ones too, so money
// history moves to "No area" consistently) leave the area and keep selling
// their plans as their own; staff lose that area; templates go with it.
func DeleteArea(db *gorm.DB, orgID, areaID uint) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var area models.Area
		if err := tx.Where("id = ? AND organization_id = ?", areaID, orgID).First(&area).Error; err != nil {
			return ErrAreaNotFound
		}
		var zoneIDs []uint
		tx.Unscoped().Model(&models.Zone{}).Where("organization_id = ? AND area_id = ?", orgID, areaID).Pluck("id", &zoneIDs)
		for _, zid := range zoneIDs {
			if err := DetachZoneFromArea(tx, orgID, zid); err != nil {
				return err
			}
		}
		if err := tx.Where("area_id = ?", areaID).Delete(&models.UserArea{}).Error; err != nil {
			return err
		}
		if err := tx.Where("area_id = ?", areaID).Delete(&models.AreaPackage{}).Error; err != nil {
			return err
		}
		return tx.Delete(&area).Error
	})
}

// ---------------------------------------------------------------------------
// Money by area
// ---------------------------------------------------------------------------

// AreaMoneyZone is one zone's line in the money-by-area report.
type AreaMoneyZone struct {
	ZoneID          uint    `json:"zone_id"`
	Name            string  `json:"name"`
	Removed         bool    `json:"removed"`
	Takings         float64 `json:"takings"`
	Payments        int64   `json:"payments"`
	PayingCustomers int64   `json:"paying_customers"`
	ActiveCustomers int64   `json:"active_customers"`
}

// AreaMoney is one area (or the "No area" bucket, AreaID nil).
type AreaMoney struct {
	AreaID          *uint           `json:"area_id"`
	Name            string          `json:"name"`
	Takings         float64         `json:"takings"`
	Payments        int64           `json:"payments"`
	PayingCustomers int64           `json:"paying_customers"` // distinct phones across the area
	ActiveCustomers int64           `json:"active_customers"`
	Zones           []AreaMoneyZone `json:"zones"`
}

// AreaMoneyReport is the whole report.
type AreaMoneyReport struct {
	From  string      `json:"from"`
	To    string      `json:"to"`
	Areas []AreaMoney `json:"areas"`
	Total AreaMoney   `json:"total"`
}

// BuildAreaMoney sums completed payments in [start, end) per zone and per
// area for the given zone ids (which should include soft-deleted zones, so
// money already earned never vanishes). areas are the areas to list even if
// empty. Active customers = status active and not yet expired at now.
func BuildAreaMoney(db *gorm.DB, orgID uint, zoneIDs []uint, areas []models.Area, start, end, now time.Time) (*AreaMoneyReport, error) {
	rep := &AreaMoneyReport{Areas: []AreaMoney{}, Total: AreaMoney{Name: "All areas", Zones: []AreaMoneyZone{}}}
	// Stored timestamps are in the server's zone (see eatDayRange).
	start, end = start.In(time.Local), end.In(time.Local)

	var zones []models.Zone
	if len(zoneIDs) > 0 {
		if err := db.Unscoped().Select("id", "name", "area_id", "deleted_at").
			Where("organization_id = ? AND id IN ?", orgID, zoneIDs).Order("name ASC").Find(&zones).Error; err != nil {
			return nil, err
		}
	}
	ids := make([]uint, 0, len(zones))
	for _, z := range zones {
		ids = append(ids, z.ID)
	}

	type payRow struct {
		ZoneID  uint
		Takings float64
		N       int64
		Buyers  int64
	}
	pay := map[uint]payRow{}
	active := map[uint]int64{}
	areaBuyers := map[uint]int64{} // area id (0 = none) → distinct phones
	var totalBuyers int64
	if len(ids) > 0 {
		var rows []payRow
		if err := db.Model(&models.Payment{}).
			Select("zone_id, COALESCE(SUM(amount),0) AS takings, COUNT(*) AS n, COUNT(DISTINCT NULLIF(phone,'')) AS buyers").
			Where("status = ? AND zone_id IN ? AND created_at >= ? AND created_at < ?", "completed", ids, start, end).
			Group("zone_id").Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			pay[r.ZoneID] = r
		}
		type cnt struct {
			ZoneID uint
			N      int64
		}
		var crow []cnt
		if err := db.Model(&models.Customer{}).Select("zone_id, COUNT(*) AS n").
			Where("zone_id IN ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)", ids, "active", now).
			Group("zone_id").Scan(&crow).Error; err != nil {
			return nil, err
		}
		for _, r := range crow {
			active[r.ZoneID] = r.N
		}
		type abRow struct {
			AreaID *uint
			N      int64
		}
		var ab []abRow
		if err := db.Table("payments").
			Select("zones.area_id AS area_id, COUNT(DISTINCT NULLIF(payments.phone,'')) AS n").
			Joins("JOIN zones ON zones.id = payments.zone_id").
			Where("payments.status = ? AND payments.zone_id IN ? AND payments.created_at >= ? AND payments.created_at < ?", "completed", ids, start, end).
			Group("zones.area_id").Scan(&ab).Error; err != nil {
			return nil, err
		}
		for _, r := range ab {
			k := uint(0)
			if r.AreaID != nil {
				k = *r.AreaID
			}
			areaBuyers[k] = r.N
		}
		if err := db.Model(&models.Payment{}).Select("COUNT(DISTINCT NULLIF(phone,''))").
			Where("status = ? AND zone_id IN ? AND created_at >= ? AND created_at < ?", "completed", ids, start, end).
			Scan(&totalBuyers).Error; err != nil {
			return nil, err
		}
	}

	byArea := map[uint]*AreaMoney{}
	order := []uint{}
	for _, a := range areas {
		id := a.ID
		byArea[a.ID] = &AreaMoney{AreaID: &id, Name: a.Name, Zones: []AreaMoneyZone{}}
		order = append(order, a.ID)
	}
	none := &AreaMoney{Name: "No area", Zones: []AreaMoneyZone{}}

	for _, z := range zones {
		p := pay[z.ID]
		removed := z.DeletedAt.Valid
		if removed && p.N == 0 {
			continue // a removed zone only shows when it took money in the range
		}
		line := AreaMoneyZone{ZoneID: z.ID, Name: z.Name, Removed: removed, Takings: round2(p.Takings),
			Payments: p.N, PayingCustomers: p.Buyers, ActiveCustomers: active[z.ID]}
		target := none
		if z.AreaID != nil {
			if a, ok := byArea[*z.AreaID]; ok {
				target = a
			}
		}
		target.Zones = append(target.Zones, line)
		target.Takings += line.Takings
		target.Payments += line.Payments
		target.ActiveCustomers += line.ActiveCustomers
		rep.Total.Takings += line.Takings
		rep.Total.Payments += line.Payments
		rep.Total.ActiveCustomers += line.ActiveCustomers
	}
	for _, id := range order {
		a := byArea[id]
		a.PayingCustomers = areaBuyers[id]
		a.Takings = round2(a.Takings)
		rep.Areas = append(rep.Areas, *a)
	}
	sort.SliceStable(rep.Areas, func(i, j int) bool { return rep.Areas[i].Name < rep.Areas[j].Name })
	if len(none.Zones) > 0 {
		noneIDs := make([]uint, 0, len(none.Zones))
		for _, z := range none.Zones {
			noneIDs = append(noneIDs, z.ZoneID)
		}
		if err := db.Model(&models.Payment{}).Select("COUNT(DISTINCT NULLIF(phone,''))").
			Where("status = ? AND zone_id IN ? AND created_at >= ? AND created_at < ?", "completed", noneIDs, start, end).
			Scan(&none.PayingCustomers).Error; err != nil {
			return nil, err
		}
		none.Takings = round2(none.Takings)
		rep.Areas = append(rep.Areas, *none)
	}
	rep.Total.Takings = round2(rep.Total.Takings)
	rep.Total.PayingCustomers = totalBuyers
	return rep, nil
}
