package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// MikroTikScriptGenerate generates and downloads a .rsc RouterOS config file.
// Scoped to the caller's organization (via the shared `scriptSvc`/`mikrotikSvc`
// globals injected by InitZoneServices in zones.go) — the zone lookup used to
// have no organization_id filter, which let any authenticated admin from any
// tenant download another tenant's router script (including plaintext PPPoE
// passwords) just by guessing a zone ID. This mirrors the scoping already
// used by the sibling ZoneScript/findZoneOrFail pattern in zones.go.
func MikroTikScriptGenerate(c *fiber.Ctx) error {
	claims := middleware.GetClaims(c)
	var zone models.Zone
	if err := config.DB.Where("organization_id = ?", claims.OrganizationID).First(&zone, c.Params("id")).Error; err != nil {
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
