package handlers

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// PlatformOverview aggregates cross-tenant totals for the SA dashboard:
// how many ISPs and end-customers are on the platform, how much revenue
// those ISPs have collected from their own customers, how much Zyra Net
// has actually invoiced/collected via PlatformInvoice, and a live estimate
// of what Zyra Net would earn at the configured default commission
// percentage (a preview figure, independent of actual generated invoices,
// since invoices can use a per-org flat rate instead).
func PlatformOverview(c *fiber.Ctx) error {
	var totalOrgs int64
	config.DB.Model(&models.Organization{}).Count(&totalOrgs)

	var activeOrgs int64
	config.DB.Model(&models.Organization{}).Where("status = ?", "active").Count(&activeOrgs)

	var totalZones int64
	config.DB.Model(&models.Zone{}).Count(&totalZones)

	var zonesOnline int64
	config.DB.Model(&models.Zone{}).Where("last_status = ?", "online").Count(&zonesOnline)

	var totalClients int64
	config.DB.Model(&models.Customer{}).Where("status = ?", "active").Count(&totalClients)

	var totalRevenue float64
	config.DB.Model(&models.Payment{}).Where("status = ?", "completed").
		Select("COALESCE(SUM(amount), 0)").Scan(&totalRevenue)

	var totalInvoiced float64
	config.DB.Model(&models.PlatformInvoice{}).
		Select("COALESCE(SUM(total), 0)").Scan(&totalInvoiced)

	var totalCollected float64
	config.DB.Model(&models.PlatformInvoice{}).Where("status = ?", "paid").
		Select("COALESCE(SUM(total), 0)").Scan(&totalCollected)

	commissionPercent, _ := strconv.ParseFloat(GetPlatformSetting("default_commission_percent"), 64)
	estimatedEarnings := totalRevenue * (commissionPercent / 100)

	return utils.SuccessResponse(c, fiber.Map{
		"total_organizations":  totalOrgs,
		"active_organizations": activeOrgs,
		"total_zones":          totalZones,
		"zones_online":         zonesOnline,
		"total_clients":        totalClients,
		"total_revenue":        totalRevenue,
		"total_invoiced":       totalInvoiced,
		"total_collected":      totalCollected,
		"commission_percent":   commissionPercent,
		"estimated_earnings":   estimatedEarnings,
	}, "")
}
