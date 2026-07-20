package handlers

import (
	"encoding/json"
	"fmt"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
	"time"
)

var mikrotikSvc *services.MikroTikService
var scriptSvc *services.MikroTikScriptService

// InitZoneServices injects MikroTik services.
func InitZoneServices(mt *services.MikroTikService, sc *services.MikroTikScriptService) {
	mikrotikSvc = mt
	scriptSvc = sc
}

// ZoneIndex lists all zones (paginated).
func ZoneIndex(c *fiber.Ctx) error {
	page, perPage := utils.ParsePage(c)
	var zones []models.Zone
	var total int64

	query := config.DB.Model(&models.Zone{}).Preload("Manager")

	if s := c.Query("status"); s != "" {
		query = query.Where("status = ?", s)
	}
	if search := c.Query("search"); search != "" {
		query = query.Where("name LIKE ? OR location LIKE ?", "%"+search+"%", "%"+search+"%")
	}

	// Zone managers only see their own zone
	claims := middleware.GetClaims(c)
	if claims != nil && claims.Role == "zone_manager" && claims.ZoneID != nil {
		query = query.Where("id = ?", *claims.ZoneID)
	}

	query.Count(&total)
	query.Order("created_at DESC").Limit(perPage).Offset(utils.Offset(page, perPage)).Find(&zones)

	return utils.PaginatedResponse(c, zones, total, page, perPage)
}

// ZoneStore creates a new zone (super_admin only).
func ZoneStore(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to create zones.", "", fiber.StatusForbidden)
	}

	var body models.Zone
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	if err := config.DB.Create(&body).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to create zone.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, body, "Zone created successfully.", fiber.StatusCreated)
}

// ZoneShow returns a single zone.
func ZoneShow(c *fiber.Ctx) error {
	id := c.Params("id")
	var zone models.Zone
	if err := config.DB.Preload("Manager").First(&zone, id).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	return utils.SuccessResponse(c, zone, "")
}

// ZoneUpdate updates a zone.
func ZoneUpdate(c *fiber.Ctx) error {
	id := c.Params("id")
	claims := middleware.GetClaims(c)

	var zone models.Zone
	if err := config.DB.First(&zone, id).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}
	if claims.Role != "super_admin" && (claims.ZoneID == nil || *claims.ZoneID != zone.ID) {
		return utils.ErrorResponse(c, "Unauthorized to update this zone.", "", fiber.StatusForbidden)
	}

	var body map[string]interface{}
	if err := c.BodyParser(&body); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}

	if err := config.DB.Model(&zone).Updates(body).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Update failed.", fiber.StatusInternalServerError)
	}
	config.DB.Preload("Manager").First(&zone, id)
	return utils.SuccessResponse(c, zone, "Zone updated successfully.")
}

// ZoneDestroy deletes a zone (super_admin only).
func ZoneDestroy(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" {
		return utils.ErrorResponse(c, "Unauthorized to delete zones.", "", fiber.StatusForbidden)
	}
	id := c.Params("id")
	if err := config.DB.Delete(&models.Zone{}, id).Error; err != nil {
		return utils.ErrorResponse(c, err.Error(), "Delete failed.", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, nil, "Zone deleted successfully.")
}

// ZoneStatus fetches live router health for a zone.
func ZoneStatus(c *fiber.Ctx) error {
	zone, err := findZoneOrFail(c)
	if err != nil {
		return err
	}
	if !canAccessZone(c, zone) {
		return utils.ErrorResponse(c, "Unauthorized to view this zone.", "", fiber.StatusForbidden)
	}
	status, _ := mikrotikSvc.GetStatus(zone)
	return utils.SuccessResponse(c, status, "")
}

// ZoneTestConnection tests connectivity to the zone's router.
func ZoneTestConnection(c *fiber.Ctx) error {
	zone, err := findZoneOrFail(c)
	if err != nil {
		return err
	}
	if !canAccessZone(c, zone) {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}
	result, _ := mikrotikSvc.TestConnection(zone)
	return utils.SuccessResponse(c, result, "")
}

// ZonePushConfig pushes hotspot and PPPoE config to the router.
func ZonePushConfig(c *fiber.Ctx) error {
	zone, err := findZoneOrFail(c)
	if err != nil {
		return err
	}
	if !canAccessZone(c, zone) {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	result, _ := mikrotikSvc.PushFullConfig(zone)

	// Audit log
	claims := middleware.GetClaims(c)
	newVals, _ := json.Marshal(result)
	newValsStr := string(newVals)
	config.DB.Create(&models.AuditLog{
		UserID:    &claims.UserID,
		Action:    "push_config",
		Model:     "Zone",
		ModelID:   zone.ID,
		NewValues: &newValsStr,
	})

	return utils.SuccessResponse(c, result, "Configuration pushed to router.")
}

// ZoneDisconnectClient kicks a client by MAC address.
func ZoneDisconnectClient(c *fiber.Ctx) error {
	zone, err := findZoneOrFail(c)
	if err != nil {
		return err
	}
	if !canAccessZone(c, zone) {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var body struct {
		MacAddress string `json:"mac_address"`
	}
	c.BodyParser(&body)
	if body.MacAddress == "" {
		return utils.ErrorResponse(c, "mac_address is required.", "", fiber.StatusUnprocessableEntity)
	}

	if err := mikrotikSvc.DisconnectClient(zone, body.MacAddress); err != nil {
		return utils.ErrorResponse(c, err.Error(), "Failed to disconnect client.", fiber.StatusBadRequest)
	}
	return utils.SuccessResponse(c, nil, "Client disconnected successfully.")
}

// ZoneActiveSessions returns live sessions from the router.
func ZoneActiveSessions(c *fiber.Ctx) error {
	zone, err := findZoneOrFail(c)
	if err != nil {
		return err
	}
	if !canAccessZone(c, zone) {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}
	sessions, _ := mikrotikSvc.GetActiveSessions(zone)
	return utils.SuccessResponse(c, sessions, "")
}

// ZoneStatsHistory returns zone stats from the last 24 hours.
func ZoneStatsHistory(c *fiber.Ctx) error {
	zone, err := findZoneOrFail(c)
	if err != nil {
		return err
	}
	if !canAccessZone(c, zone) {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var stats []models.ZoneStat
	config.DB.Where("zone_id = ? AND recorded_at >= ?", zone.ID, time.Now().Add(-24*time.Hour)).
		Order("recorded_at ASC").Find(&stats)
	return utils.SuccessResponse(c, stats, "")
}

// ZoneAlerts returns zone alerts.
func ZoneAlerts(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	filter := c.Query("filter", "unresolved")

	query := config.DB.Model(&models.ZoneAlert{}).Preload("Zone")
	if filter == "unresolved" {
		query = query.Where("resolved_at IS NULL")
	}
	if claims.Role != "super_admin" && claims.ZoneID != nil {
		query = query.Where("zone_id = ?", *claims.ZoneID)
	}

	var alerts []models.ZoneAlert
	query.Order("created_at DESC").Find(&alerts)
	return utils.SuccessResponse(c, alerts, "")
}

// ZoneResolveAlert marks an alert as resolved.
func ZoneResolveAlert(c *fiber.Ctx) error {
	id := c.Params("id")
	var alert models.ZoneAlert
	if err := config.DB.Preload("Zone").First(&alert, id).Error; err != nil {
		return utils.ErrorResponse(c, "Alert not found.", "", fiber.StatusNotFound)
	}
	claims := middleware.GetClaims(c)
	if claims.Role != "super_admin" && (claims.ZoneID == nil || *claims.ZoneID != alert.ZoneID) {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}
	now := time.Now()
	config.DB.Model(&alert).Update("resolved_at", now)
	alert.ResolvedAt = &now
	return utils.SuccessResponse(c, alert, "Alert marked as resolved.")
}

// ZoneScript generates and downloads a .rsc script for the zone.
func ZoneScript(c *fiber.Ctx) error {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		return utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
	}

	content, filename, err := scriptSvc.GenerateScript(zone.ID)
	if err != nil {
		return utils.ErrorResponse(c, err.Error(), "Script generation failed.", fiber.StatusBadRequest)
	}

	c.Set("Content-Type", "text/plain")
	c.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	return c.SendString(content)
}

// ZoneExecCommand executes a remote command on the zone's MikroTik router.
func ZoneExecCommand(c *fiber.Ctx) error {
	zone, err := findZoneOrFail(c)
	if err != nil {
		return err
	}
	if !canAccessZone(c, zone) {
		return utils.ErrorResponse(c, "Unauthorized.", "", fiber.StatusForbidden)
	}

	var body struct {
		Command string `json:"command"`
	}
	if err := c.BodyParser(&body); err != nil || body.Command == "" {
		return utils.ErrorResponse(c, "Command is required.", "", fiber.StatusBadRequest)
	}

	claims := middleware.GetClaims(c)
	cmdVals := fmt.Sprintf(`{"command":"%s"}`, body.Command)
	config.DB.Create(&models.AuditLog{
		UserID:    &claims.UserID,
		Action:    "exec_command",
		Model:     "Zone",
		ModelID:   zone.ID,
		NewValues: &cmdVals,
	})

	output, err := mikrotikSvc.ExecCommand(zone, body.Command)
	if err != nil {
		return utils.SuccessResponse(c, fmt.Sprintf("Command '%s' dispatched to %s (%s).\nStatus: %s", body.Command, zone.RouterName, zone.RouterIP, err.Error()), "")
	}
	return utils.SuccessResponse(c, output, "Command executed successfully.")
}

// helpers

func findZoneOrFail(c *fiber.Ctx) (*models.Zone, error) {
	var zone models.Zone
	if err := config.DB.First(&zone, c.Params("id")).Error; err != nil {
		utils.ErrorResponse(c, "Zone not found.", "", fiber.StatusNotFound)
		return nil, fiber.ErrNotFound
	}
	return &zone, nil
}

func canAccessZone(c *fiber.Ctx, zone *models.Zone) bool {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return false
	}
	if claims.Role == "super_admin" {
		return true
	}
	return zone.ManagerID != nil && *zone.ManagerID == claims.UserID
}
