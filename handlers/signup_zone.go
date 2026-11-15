package handlers

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// signupTarget picks the WiFi zone and starter plan a brand-new portal
// account is filed under. The zone is the one the router sent the phone from
// (zone_id in the body or query). It used to be "the first zone in the
// database" with "the first hotspot plan", which files a new customer — and
// their later payments — under another ISP as soon as there are two.
//
// With no zone given, the first zone is still used ONLY when the whole
// install has a single ISP (an old portal page that doesn't pass the zone);
// otherwise nothing is guessed.
func signupTarget(c *fiber.Ctx, zoneRaw string) (*models.Zone, *models.Package) {
	zone := zoneByID(zoneRaw)
	if zone == nil {
		zone = zoneByID(c.Query("zone_id"))
	}
	if zone == nil {
		var orgs int64
		config.DB.Model(&models.Zone{}).Distinct("organization_id").Count(&orgs)
		if orgs != 1 {
			return nil, nil
		}
		var z models.Zone
		if err := config.DB.Order("id").First(&z).Error; err != nil {
			return nil, nil
		}
		zone = &z
	}
	// A plan of that zone; failing that, of the same ISP. Never another ISP's.
	var pkg models.Package
	if config.DB.Where("zone_id = ? AND type = ? AND status = ?", zone.ID, "hotspot", "active").Order("id").First(&pkg).Error != nil {
		ispZones := config.DB.Model(&models.Zone{}).Select("id").Where("organization_id = ?", zone.OrganizationID)
		if config.DB.Where("zone_id IN (?)", ispZones).Order("type = 'hotspot' DESC, id").First(&pkg).Error != nil {
			return zone, nil
		}
	}
	return zone, &pkg
}

const signupNeedsZoneMsg = "Open this page from the WiFi login screen to sign up."
