package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// ReportGrowth is Reports › Growth: the ISP's real numbers per EAT week
// (Mon–Sun) next to the owner's growth plan. ?weeks=12 (1–104). The plan is
// for the whole ISP, so this report is ISP-wide (owner and finance only).
func ReportGrowth(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	weeks := c.QueryInt("weeks", 12)
	if weeks < 1 {
		weeks = 1
	}
	if weeks > 104 {
		weeks = 104
	}
	out, err := services.BuildGrowth(config.DB, claims.OrganizationID, weeks, time.Now())
	if err != nil {
		return utils.ErrorResponse(c, "Could not load the growth report.", "", fiber.StatusInternalServerError)
	}
	c.Set("Cache-Control", "no-store")
	return utils.SuccessResponse(c, out, "")
}

// ReportGrowthSettings saves the ISP's plan numbers (owner only).
func ReportGrowthSettings(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var body services.GrowthPlanSettings
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if err := services.ValidateGrowthPlan(body); err != nil {
		return utils.ErrorResponse(c, err.Error(), "Validation failed.", fiber.StatusUnprocessableEntity)
	}
	uid := claims.UserID
	if err := services.SaveGrowthPlan(config.DB, claims.OrganizationID, body, &uid); err != nil {
		return utils.ErrorResponse(c, "Could not save the plan.", "", fiber.StatusInternalServerError)
	}
	s, set := services.LoadGrowthPlan(config.DB, claims.OrganizationID)
	return utils.SuccessResponse(c, fiber.Map{"settings": s, "start_date_set": set}, "Plan saved.")
}
