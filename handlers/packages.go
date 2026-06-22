package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// PackageIndex lists all packages (paginated with filters).
func PackageIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var pkgs []models.Package
	var total int64

	query := config.DB.Model(&models.Package{}).Preload("Zone")
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

// PackagePublic returns active packages (no auth needed, for portal).
func PackagePublic(c *fiber.Ctx) error {
	var pkgs []models.Package
	query := config.DB.Where("status = ?", "active").Preload("Zone")
	if z := c.Query("zone_id"); z != "" {
		query = query.Where("zone_id = ?", z)
	}
	query.Order("price ASC").Find(&pkgs)
	return utils.SuccessResponse(c, pkgs, "")
}

// PackageStore creates a new package.
func PackageStore(c *fiber.Ctx) error {
	var pkg models.Package
	if err := c.BodyParser(&pkg); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if err := config.DB.Create(&pkg).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create package.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Zone").First(&pkg, pkg.ID)
	return utils.SuccessResponse(c, pkg, "Package created successfully.", fiber.StatusCreated)
}

// PackageShow returns a single package.
func PackageShow(c *fiber.Ctx) error {
	var pkg models.Package
	if err := config.DB.Preload("Zone").First(&pkg, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Package not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, pkg, "")
}

// PackageUpdate updates a package.
func PackageUpdate(c *fiber.Ctx) error {
	var pkg models.Package
	if err := config.DB.First(&pkg, c.Params("id")).Error; err != nil {
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
	if err := config.DB.Delete(&models.Package{}, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Package deleted successfully.")
}
