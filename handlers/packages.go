package handlers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
)

// IsFreeTierAvailableNow checks whether a package with scheduled free tier hours is currently open.
func IsFreeTierAvailableNow(pkg *models.Package) (bool, string) {
	if !pkg.IsFreeTier && pkg.Price > 0 {
		return false, "Not a free tier package."
	}

	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		loc = time.FixedZone("EAT", 3*3600)
	}
	now := time.Now().In(loc)

	// Check day of week
	weekday := now.Weekday() // 0 = Sunday, 6 = Saturday
	if pkg.FreeTierDays == "weekdays" && (weekday == time.Saturday || weekday == time.Sunday) {
		return false, "Free trial is only available on weekdays (Monday to Friday)."
	}
	if pkg.FreeTierDays == "weekends" && (weekday != time.Saturday && weekday != time.Sunday) {
		return false, "Free trial is only available on weekends (Saturday & Sunday)."
	}

	// Check time of day window (HH:MM in 24-hour format)
	if pkg.FreeTierStartTime != nil && *pkg.FreeTierStartTime != "" &&
		pkg.FreeTierEndTime != nil && *pkg.FreeTierEndTime != "" {

		startParts := strings.Split(strings.TrimSpace(*pkg.FreeTierStartTime), ":")
		endParts := strings.Split(strings.TrimSpace(*pkg.FreeTierEndTime), ":")
		if len(startParts) >= 2 && len(endParts) >= 2 {
			startHour, _ := strconv.Atoi(startParts[0])
			startMin, _ := strconv.Atoi(startParts[1])
			endHour, _ := strconv.Atoi(endParts[0])
			endMin, _ := strconv.Atoi(endParts[1])

			currentMinuteOfDay := now.Hour()*60 + now.Minute()
			startMinuteOfDay := startHour*60 + startMin
			endMinuteOfDay := endHour*60 + endMin

			if startMinuteOfDay <= endMinuteOfDay {
				// E.g. 14:00 to 18:00
				if currentMinuteOfDay < startMinuteOfDay || currentMinuteOfDay > endMinuteOfDay {
					return false, fmt.Sprintf("Free trial is only available between %s and %s (EAT).", *pkg.FreeTierStartTime, *pkg.FreeTierEndTime)
				}
			} else {
				// Overnight range e.g. 22:00 to 06:00
				if currentMinuteOfDay < startMinuteOfDay && currentMinuteOfDay > endMinuteOfDay {
					return false, fmt.Sprintf("Free trial is only available between %s and %s (EAT).", *pkg.FreeTierStartTime, *pkg.FreeTierEndTime)
				}
			}
		}
	}

	return true, ""
}

// PackageIndex lists all packages (paginated with filters).
func PackageIndex(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}

	page, perPage := utils.ParsePage(c)
	var pkgs []models.Package
	var total int64

	query := config.DB.Model(&models.Package{}).Preload("Zone").Where("zone_id IN (?)", orgZoneIDs)
	if z := c.Query("zone_id"); z != "" {
		query = query.Where("zone_id = ?", z)
	}
	if t := c.Query("type"); t != "" {
		query = query.Where("type = ?", t)
	}
	if s := c.Query("status"); s != "" {
		query = query.Where("status = ?", s)
	}

	query.Count(&total)
	query.Order("zone_id, price ASC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&pkgs)
	return utils.PaginatedResponse(c, pkgs, total, page, perPage)
}

// PackagePublic returns active packages (no auth needed, for portal). Does
// NOT preload Zone — Zone carries router admin credentials
// (router_username/router_password) and internal network config
// (router_ip, hotspot_address, lan_ports), which must never reach an
// unauthenticated endpoint. Callers needing zone context already have
// zone_id on the package itself.
func PackagePublic(c *fiber.Ctx) error {
	var pkgs []models.Package
	query := config.DB.Where("status = ?", "active")
	if z := c.Query("zone_id"); z != "" {
		query = query.Where("zone_id = ?", z)
	}
	query.Order("price ASC").Find(&pkgs)

	type PublicPackageItem struct {
		models.Package
		IsAvailableNow bool   `json:"is_available_now"`
		ScheduleReason string `json:"schedule_reason,omitempty"`
	}

	res := make([]PublicPackageItem, len(pkgs))
	for i, p := range pkgs {
		avail := true
		reason := ""
		if p.IsFreeTier || p.Price == 0 {
			avail, reason = IsFreeTierAvailableNow(&p)
		}
		res[i] = PublicPackageItem{
			Package:        p,
			IsAvailableNow: avail,
			ScheduleReason: reason,
		}
	}

	return utils.SuccessResponse(c, res, "")
}

// PackageStore creates a new package.
func PackageStore(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var pkg models.Package
	if err := c.BodyParser(&pkg); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	pkg.ID = 0
	var targetZone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&targetZone, pkg.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid zone for this organization.", "", fiber.StatusUnprocessableEntity)
	}
	if !packageZoneAllowed(c, pkg.ZoneID) {
		return utils.ErrorResponse(c, "You can only manage packages in your own zone.", "", fiber.StatusForbidden)
	}
	if msg := validatePackage(&pkg); msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if err := config.DB.Create(&pkg).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create package.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zone").First(&pkg, pkg.ID)
	return utils.SuccessResponse(c, pkg, "Package created successfully.", fiber.StatusCreated)
}

// PackageShow returns a single package.
func PackageShow(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var pkg models.Package
	if err := config.DB.Preload("Zone").Where("zone_id IN (?)", orgZoneIDs).First(&pkg, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Package not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, pkg, "")
}

// PackageUpdate updates a package.
func PackageUpdate(c *fiber.Ctx) error {
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
	var raw map[string]interface{}
	if err := c.BodyParser(&raw); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	body := map[string]interface{}{}
	for k, v := range raw {
		if packageEditableFields[k] {
			body[k] = v
		}
	}
	if len(body) == 0 {
		return utils.ErrorResponse(c, "Nothing to update.", "", fiber.StatusUnprocessableEntity)
	}
	// Validate the package as it will be after the change.
	merged := pkg
	if b, err := json.Marshal(body); err != nil || json.Unmarshal(b, &merged) != nil {
		return utils.ErrorResponse(c, "Invalid package fields.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if merged.ZoneID != pkg.ZoneID {
		inOrg := false
		for _, id := range orgZoneIDs {
			if id == merged.ZoneID {
				inOrg = true
			}
		}
		if !inOrg || !packageZoneAllowed(c, merged.ZoneID) {
			return utils.ErrorResponse(c, "Invalid zone for this organization.", "", fiber.StatusUnprocessableEntity)
		}
	}
	if msg := validatePackage(&merged); msg != "" {
		return utils.ErrorResponse(c, msg, "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	if err := config.DB.Model(&pkg).Updates(body).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zone").First(&pkg, pkg.ID)
	return utils.SuccessResponse(c, pkg, "Package updated successfully.")
}

// PackageDestroy soft-deletes a package.
func PackageDestroy(c *fiber.Ctx) error {
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
	if err := config.DB.Delete(&models.Package{}, pkg.ID).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Package deleted successfully.")
}

// PackageDuplicate duplicates a package to multiple target zones.
func PackageDuplicate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	var pkg models.Package
	if err := config.DB.Where("zone_id IN (?)", orgZoneIDs).First(&pkg, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Source package not found.", "", fiber.StatusNotFound)
	}

	var body struct {
		ZoneIDs []uint `json:"zone_ids"`
	}
	if err := c.BodyParser(&body); err != nil || len(body.ZoneIDs) == 0 {
		return utils.ErrorResponse(c, "Target zone_ids list is required.", "Validation failed.", fiber.StatusUnprocessableEntity)
	}

	duplicatedCount := 0
	for _, zoneID := range body.ZoneIDs {
		// Verify the target zone exists and belongs to the same Organization
		var zone models.Zone
		if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, zoneID).Error; err != nil {
			continue
		}
		if !packageZoneAllowed(c, zoneID) {
			continue
		}

		// Copy every plan rule (data cap, free-trial window, …) — the copy
		// used to drop the data limit and free-trial settings, selling an
		// uncapped plan at the capped price.
		newPkg := pkg
		newPkg.ID = 0
		newPkg.CreatedAt, newPkg.UpdatedAt = time.Time{}, time.Time{}
		newPkg.DeletedAt = gorm.DeletedAt{}
		newPkg.Zone = nil
		newPkg.ZoneID = zoneID

		if err := config.DB.Create(&newPkg).Error; err == nil {
			duplicatedCount++
		}
	}

	return utils.SuccessResponse(c, fiber.Map{
		"duplicated_count": duplicatedCount,
	}, "Package duplicated successfully to target zones.")
}

// packageEditableFields are the only keys PUT /packages/:id may change.
// Anything else (id, timestamps, the nested zone object) is ignored instead of
// reaching GORM, where it either errored (500) or rewrote server fields.
var packageEditableFields = map[string]bool{
	"zone_id": true, "name": true, "type": true, "category": true, "device_limit": true,
	"price": true, "is_free_tier": true, "free_tier_cooldown_hours": true,
	"free_tier_start_time": true, "free_tier_end_time": true, "free_tier_days": true,
	"time_limit_minutes": true, "data_limit_mb": true, "fair_use_kbps": true, "speed_upload_kbps": true,
	"speed_download_kbps": true, "billing_cycle": true, "status": true,
}

// validatePackage rejects plans the portal can't sell: no name, an unknown
// type/billing cycle/status, or a negative price.
func validatePackage(p *models.Package) string {
	switch {
	case strings.TrimSpace(p.Name) == "":
		return "Package name is required."
	case p.Type != "hotspot" && p.Type != "pppoe":
		return "Type must be hotspot or pppoe."
	case p.Price < 0:
		return "Price cannot be negative."
	case p.SpeedUploadKbps < 0 || p.SpeedDownloadKbps < 0:
		return "Speeds cannot be negative."
	case p.TimeLimitMinutes != nil && *p.TimeLimitMinutes < 0, p.DataLimitMB != nil && *p.DataLimitMB < 0:
		return "Time and data limits cannot be negative."
	case p.FairUseKbps != nil && *p.FairUseKbps != 0 && (*p.FairUseKbps < 64 || *p.FairUseKbps > 1024000):
		return "Speed after the data cap must be between 0.064 and 1000 Mbps (or empty)."
	}
	switch p.BillingCycle {
	case "hourly", "daily", "weekly", "monthly":
	default:
		return "Billing cycle must be hourly, daily, weekly or monthly."
	}
	switch p.Status {
	case "", "active", "inactive":
	default:
		return "Status must be active or inactive."
	}
	return ""
}

// packageZoneAllowed: a zone manager only manages their own zone's plans.
func packageZoneAllowed(c *fiber.Ctx, zoneID uint) bool {
	claims := middleware.GetClaims(c)
	if claims != nil && claims.Role == "zone_manager" && claims.ZoneID != nil {
		return *claims.ZoneID == zoneID
	}
	return true
}
