package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
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
	var targetZone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&targetZone, pkg.ZoneID).Error; err != nil {
		return utils.ErrorResponse(c, "Invalid zone for this organization.", "", fiber.StatusUnprocessableEntity)
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
	var body map[string]interface{}
	c.BodyParser(&body)
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

		newPkg := models.Package{
			Name:              pkg.Name,
			Type:              pkg.Type,
			Category:          pkg.Category,
			DeviceLimit:       pkg.DeviceLimit,
			Price:             pkg.Price,
			SpeedUploadKbps:   pkg.SpeedUploadKbps,
			SpeedDownloadKbps: pkg.SpeedDownloadKbps,
			TimeLimitMinutes:  pkg.TimeLimitMinutes,
			BillingCycle:      pkg.BillingCycle,
			Status:            pkg.Status,
			ZoneID:            zoneID,
		}

		if err := config.DB.Create(&newPkg).Error; err == nil {
			duplicatedCount++
		}
	}

	return utils.SuccessResponse(c, fiber.Map{
		"duplicated_count": duplicatedCount,
	}, "Package duplicated successfully to target zones.")
}
