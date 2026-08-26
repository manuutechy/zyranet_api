package handlers

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// DailySale holds day-by-day sales data for charts.
type DailySale struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
	Count  int64   `json:"count"`
}

// PlatformOverview aggregates cross-tenant totals for the SA dashboard:
// real-time sales breakdowns (today, month, lifetime), 7-day trend, recent payments,
// top tenant leaderboard, and infrastructure statistics.
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
	if commissionPercent == 0 {
		commissionPercent = 5.0 // default 5% platform fee
	}
	estimatedEarnings := totalRevenue * (commissionPercent / 100)

	// Time boundaries
	now := time.Now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var todayRevenue float64
	var todayTransactions int64
	config.DB.Model(&models.Payment{}).Where("status = ? AND created_at >= ?", "completed", startOfToday).
		Select("COALESCE(SUM(amount), 0)").Scan(&todayRevenue)
	config.DB.Model(&models.Payment{}).Where("status = ? AND created_at >= ?", "completed", startOfToday).
		Count(&todayTransactions)

	var thisMonthRevenue float64
	var thisMonthTransactions int64
	config.DB.Model(&models.Payment{}).Where("status = ? AND created_at >= ?", "completed", startOfMonth).
		Select("COALESCE(SUM(amount), 0)").Scan(&thisMonthRevenue)
	config.DB.Model(&models.Payment{}).Where("status = ? AND created_at >= ?", "completed", startOfMonth).
		Count(&thisMonthTransactions)

	// 7-day sales breakdown
	dailySales := make([]DailySale, 7)
	for i := 6; i >= 0; i-- {
		dayStart := startOfToday.AddDate(0, 0, -i)
		dayEnd := dayStart.AddDate(0, 0, 1)
		dateLabel := dayStart.Format("2006-01-02")

		var dayAmount float64
		var dayCount int64
		config.DB.Model(&models.Payment{}).
			Where("status = ? AND created_at >= ? AND created_at < ?", "completed", dayStart, dayEnd).
			Select("COALESCE(SUM(amount), 0)").Scan(&dayAmount)
		config.DB.Model(&models.Payment{}).
			Where("status = ? AND created_at >= ? AND created_at < ?", "completed", dayStart, dayEnd).
			Count(&dayCount)

		dailySales[6-i] = DailySale{
			Date:   dateLabel,
			Amount: dayAmount,
			Count:  dayCount,
		}
	}

	// Recent 10 completed payments across all tenants
	var recentPayments []models.Payment
	config.DB.Preload("Customer").Preload("Zone").Preload("Package").
		Where("status = ?", "completed").
		Order("created_at DESC").
		Limit(10).
		Find(&recentPayments)

	// Top ISP organizations
	var orgs []models.Organization
	config.DB.Where("status = ?", "active").Find(&orgs)
	summaries := make([]OrganizationSummary, 0, len(orgs))
	for _, org := range orgs {
		summaries = append(summaries, organizationSummary(org))
	}

	return utils.SuccessResponse(c, fiber.Map{
		"total_organizations":     totalOrgs,
		"active_organizations":    activeOrgs,
		"total_zones":             totalZones,
		"zones_online":            zonesOnline,
		"total_clients":           totalClients,
		"total_revenue":           totalRevenue,
		"today_revenue":           todayRevenue,
		"today_transactions":      todayTransactions,
		"this_month_revenue":      thisMonthRevenue,
		"this_month_transactions": thisMonthTransactions,
		"total_invoiced":          totalInvoiced,
		"total_collected":         totalCollected,
		"commission_percent":      commissionPercent,
		"estimated_earnings":      estimatedEarnings,
		"daily_sales":             dailySales,
		"recent_payments":         recentPayments,
		"top_organizations":       summaries,
	}, "")
}
