package handlers

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// scopedZoneID returns the zone_id a report should be restricted to: a
// zone_manager is always pinned to their own zone, regardless of what was
// requested; everyone else may filter by the requested zone_id (or none).
func scopedZoneID(c *fiber.Ctx, requested string) string {
	claims := middleware.GetClaims(c)
	if claims != nil && claims.Role == "zone_manager" && claims.ZoneID != nil {
		return strconv.FormatUint(uint64(*claims.ZoneID), 10)
	}
	return requested
}

// ReportRevenue returns revenue summary and daily breakdown.
func ReportRevenue(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}

	dateFrom := c.Query("date_from", time.Now().AddDate(0, 0, -30).Format("2006-01-02"))
	dateTo := c.Query("date_to", time.Now().Format("2006-01-02"))
	zoneID := scopedZoneID(c, c.Query("zone_id"))

	query := config.DB.Model(&models.Payment{}).
		Where("status = ?", "completed").
		Where("zone_id IN (?)", orgZoneIDs).
		Where("DATE(created_at) >= ?", dateFrom).
		Where("DATE(created_at) <= ?", dateTo)

	if zoneID != "" {
		query = query.Where("zone_id = ?", zoneID)
	}

	type DailyRevenue struct {
		Date  string  `json:"date"`
		Total float64 `json:"total"`
	}

	dailyQuery := config.DB.Model(&models.Payment{}).
		Select("DATE(created_at) as date, SUM(amount) as total").
		Where("status = ?", "completed").
		Where("zone_id IN (?)", orgZoneIDs).
		Where("DATE(created_at) >= ?", dateFrom).
		Where("DATE(created_at) <= ?", dateTo)
	if zoneID != "" {
		dailyQuery = dailyQuery.Where("zone_id = ?", zoneID)
	}
	var daily []DailyRevenue
	dailyQuery.Group("DATE(created_at)").Order("date ASC").Find(&daily)

	var totalRevenue float64
	var totalPayments int64
	query.Select("COALESCE(SUM(amount), 0)").Scan(&totalRevenue)
	query.Count(&totalPayments)

	from, _ := time.Parse("2006-01-02", dateFrom)
	to, _ := time.Parse("2006-01-02", dateTo)
	daysCount := int(to.Sub(from).Hours()/24) + 1
	avgPerDay := 0.0
	if daysCount > 0 {
		avgPerDay = totalRevenue / float64(daysCount)
	}

	highestDay := ""
	highestAmount := 0.0
	for _, d := range daily {
		if d.Total > highestAmount {
			highestAmount = d.Total
			highestDay = d.Date
		}
	}

	return utils.SuccessResponse(c, fiber.Map{
		"summary": fiber.Map{
			"total_revenue":       totalRevenue,
			"total_payments":      totalPayments,
			"days_count":          daysCount,
			"avg_per_day":         roundFloat(avgPerDay, 2),
			"highest_day_amount":  highestAmount,
			"highest_day_date":    highestDay,
		},
		"chart_data": daily,
	}, "")
}

// ReportVouchers returns voucher status summary and package breakdown.
func ReportVouchers(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}

	zoneID := scopedZoneID(c, c.Query("zone_id"))

	query := config.DB.Model(&models.Voucher{}).Where("zone_id IN (?)", orgZoneIDs)
	if zoneID != "" {
		query = query.Where("zone_id = ?", zoneID)
	}

	type StatusCount struct {
		Status string `json:"status"`
		Count  int64  `json:"count"`
	}
	var statusCounts []StatusCount
	query.Select("status, count(*) as count").Group("status").Scan(&statusCounts)

	statusSummary := map[string]int64{
		"unused":   0,
		"active":   0,
		"expired":  0,
		"depleted": 0,
	}
	for _, sc := range statusCounts {
		statusSummary[sc.Status] = sc.Count
	}

	type PackageBreakdown struct {
		PackageName string `json:"package_name"`
		Generated   int64  `json:"generated"`
		Used        int64  `json:"used"`
		Remaining   int64  `json:"remaining"`
	}
	var pkgBreakdown []PackageBreakdown
	baseQuery := config.DB.Model(&models.Voucher{}).
		Joins("JOIN packages ON packages.id = vouchers.package_id").
		Where("vouchers.zone_id IN (?)", orgZoneIDs).
		Select("packages.name as package_name, count(*) as generated, " +
			"SUM(CASE WHEN vouchers.status IN ('active','depleted') THEN 1 ELSE 0 END) as used, " +
			"SUM(CASE WHEN vouchers.status = 'unused' THEN 1 ELSE 0 END) as remaining").
		Group("packages.id, packages.name")

	if zoneID != "" {
		baseQuery = baseQuery.Where("vouchers.zone_id = ?", zoneID)
	}
	baseQuery.Scan(&pkgBreakdown)

	return utils.SuccessResponse(c, fiber.Map{
		"status_summary":    statusSummary,
		"package_breakdown": pkgBreakdown,
	}, "")
}

// ReportZones returns zone performance comparison.
func ReportZones(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var zones []models.Zone
	zoneQuery := config.DB.Model(&models.Zone{}).Where("organization_id = ?", claims.OrganizationID)
	if claims.Role == "zone_manager" && claims.ZoneID != nil {
		zoneQuery = zoneQuery.Where("id = ?", *claims.ZoneID)
	}
	zoneQuery.Find(&zones)

	type ZoneComparison struct {
		ZoneID         uint    `json:"zone_id"`
		Name           string  `json:"name"`
		Location       string  `json:"location"`
		Revenue        float64 `json:"revenue"`
		CustomersCount int64   `json:"customers_count"`
		VouchersSold   int64   `json:"vouchers_sold"`
		SessionsCount  int64   `json:"sessions_count"`
		Status         string  `json:"status"`
	}

	comparison := []ZoneComparison{}
	for _, zone := range zones {
		var revenue float64
		var activeCustomers, vouchersSold, activeSessions int64

		config.DB.Model(&models.Payment{}).
			Where("zone_id = ? AND status = ?", zone.ID, "completed").
			Select("COALESCE(SUM(amount), 0)").Scan(&revenue)

		config.DB.Model(&models.Customer{}).
			Where("zone_id = ? AND status = ?", zone.ID, "active").Count(&activeCustomers)

		config.DB.Model(&models.Voucher{}).
			Where("zone_id = ? AND status IN ?", zone.ID, []string{"active", "depleted"}).Count(&vouchersSold)

		config.DB.Model(&models.Session{}).
			Where("zone_id = ?", zone.ID).
			Where("ended_at IS NULL").Count(&activeSessions)

		comparison = append(comparison, ZoneComparison{
			ZoneID:         zone.ID,
			Name:           zone.Name,
			Location:       zone.Location,
			Revenue:        revenue,
			CustomersCount: activeCustomers,
			VouchersSold:   vouchersSold,
			SessionsCount:  activeSessions,
			Status:         zone.Status,
		})
	}

	return utils.SuccessResponse(c, comparison, "")
}

// ReportServiceTypes breaks down hotspot vs PPPoE performance: active
// sessions right now, sessions started today, today's revenue, weekly revenue,
// monthly revenue, active customers, and total customers.
func ReportServiceTypes(c *fiber.Ctx) error {
	orgZoneIDs, err := middleware.OrgZoneIDs(c)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to resolve organization zones.", "", fiber.StatusInternalServerError)
	}
	zoneID := scopedZoneID(c, c.Query("zone_id"))
	now := time.Now()
	today := now.Format("2006-01-02")
	weekStart := now.AddDate(0, 0, -7).Format("2006-01-02")
	monthStart := now.Format("2006-01") + "-01"

	type ServiceTypeStats struct {
		Type             string  `json:"type"`
		ActiveSessions   int64   `json:"active_sessions"`
		SessionsToday    int64   `json:"sessions_today"`
		RevenueToday     float64 `json:"revenue_today"`
		RevenueThisWeek  float64 `json:"revenue_this_week"`
		RevenueThisMonth float64 `json:"revenue_this_month"`
		ActiveCustomers  int64   `json:"active_customers"`
		TotalCustomers   int64   `json:"total_customers"`
	}

	stats := []ServiceTypeStats{}
	for _, t := range []string{"hotspot", "pppoe"} {
		s := ServiceTypeStats{Type: t}

		customerQuery := config.DB.Model(&models.Customer{}).Where("type = ? AND zone_id IN (?)", t, orgZoneIDs)
		if zoneID != "" {
			customerQuery = customerQuery.Where("zone_id = ?", zoneID)
		}
		customerQuery.Count(&s.TotalCustomers)

		activeCustomerQuery := config.DB.Model(&models.Customer{}).Where("type = ? AND status = ? AND zone_id IN (?)", t, "active", orgZoneIDs)
		if zoneID != "" {
			activeCustomerQuery = activeCustomerQuery.Where("zone_id = ?", zoneID)
		}
		activeCustomerQuery.Count(&s.ActiveCustomers)

		activeSessionsQuery := config.DB.Model(&models.Session{}).
			Joins("JOIN customers ON customers.id = sessions.customer_id").
			Where("customers.type = ? AND sessions.ended_at IS NULL AND sessions.zone_id IN (?)", t, orgZoneIDs)
		if zoneID != "" {
			activeSessionsQuery = activeSessionsQuery.Where("sessions.zone_id = ?", zoneID)
		}
		activeSessionsQuery.Count(&s.ActiveSessions)

		sessionsTodayQuery := config.DB.Model(&models.Session{}).
			Joins("JOIN customers ON customers.id = sessions.customer_id").
			Where("customers.type = ? AND DATE(sessions.started_at) = ? AND sessions.zone_id IN (?)", t, today, orgZoneIDs)
		if zoneID != "" {
			sessionsTodayQuery = sessionsTodayQuery.Where("sessions.zone_id = ?", zoneID)
		}
		sessionsTodayQuery.Count(&s.SessionsToday)

		// Revenue Today
		revTodayQuery := config.DB.Model(&models.Payment{}).
			Joins("JOIN packages ON packages.id = payments.package_id").
			Where("packages.type = ? AND payments.status = ? AND DATE(payments.created_at) = ? AND payments.zone_id IN (?)", t, "completed", today, orgZoneIDs)
		if zoneID != "" {
			revTodayQuery = revTodayQuery.Where("payments.zone_id = ?", zoneID)
		}
		revTodayQuery.Select("COALESCE(SUM(payments.amount), 0)").Scan(&s.RevenueToday)

		// Revenue This Week
		revWeekQuery := config.DB.Model(&models.Payment{}).
			Joins("JOIN packages ON packages.id = payments.package_id").
			Where("packages.type = ? AND payments.status = ? AND DATE(payments.created_at) >= ? AND payments.zone_id IN (?)", t, "completed", weekStart, orgZoneIDs)
		if zoneID != "" {
			revWeekQuery = revWeekQuery.Where("payments.zone_id = ?", zoneID)
		}
		revWeekQuery.Select("COALESCE(SUM(payments.amount), 0)").Scan(&s.RevenueThisWeek)

		// Revenue This Month
		revMonthQuery := config.DB.Model(&models.Payment{}).
			Joins("JOIN packages ON packages.id = payments.package_id").
			Where("packages.type = ? AND payments.status = ? AND DATE(payments.created_at) >= ? AND payments.zone_id IN (?)", t, "completed", monthStart, orgZoneIDs)
		if zoneID != "" {
			revMonthQuery = revMonthQuery.Where("payments.zone_id = ?", zoneID)
		}
		revMonthQuery.Select("COALESCE(SUM(payments.amount), 0)").Scan(&s.RevenueThisMonth)

		stats = append(stats, s)
	}

	return utils.SuccessResponse(c, stats, "")
}

func roundFloat(f float64, precision int) float64 {
	p := 1.0
	for i := 0; i < precision; i++ {
		p *= 10
	}
	return float64(int(f*p+0.5)) / p
}
