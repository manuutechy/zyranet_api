package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
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

// errPhoneAmbiguous: the phone has accounts with more than one ISP and the
// portal didn't say which WiFi it is on, so we refuse rather than guess.
var errPhoneAmbiguous = errors.New("phone belongs to more than one ISP")

const phoneAmbiguousMsg = "This number has accounts with more than one WiFi provider. Open this page from the WiFi login screen and try again."

// portalZone is the zone a portal request came from (body, then query).
func portalZone(c *fiber.Ctx, zoneRaw string) *models.Zone {
	if z := zoneByID(zoneRaw); z != nil {
		return z
	}
	return zoneByID(c.Query("zone_id"))
}

// customerByPhoneForPortal finds the account a portal phone sign-in belongs
// to, inside ONE ISP. The same phone can be a customer of two ISPs; the old
// global "phone = ?" lookup signed people into (and filed payments under)
// whichever ISP's row came first.
//
//   - zone known → only that zone's ISP (deleted zones of that ISP count, so
//     an old customer keeps their account).
//   - zone unknown → fine while every account with this phone is in one ISP;
//     if they span ISPs it returns errPhoneAmbiguous.
//
// (nil, nil) means no account in scope (the caller may sign them up).
func customerByPhoneForPortal(c *fiber.Ctx, phone, zoneRaw string) (*models.Customer, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return nil, nil
	}
	if zone := portalZone(c, zoneRaw); zone != nil {
		ispZones := config.DB.Unscoped().Model(&models.Zone{}).Select("id").Where("organization_id = ?", zone.OrganizationID)
		var cust models.Customer
		if err := config.DB.Where("phone = ? AND zone_id IN (?)", phone, ispZones).Order("id").First(&cust).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil
			}
			return nil, err
		}
		return &cust, nil
	}
	var orgs []uint
	if err := config.DB.Unscoped().Model(&models.Zone{}).Distinct("organization_id").
		Where("id IN (?)", config.DB.Model(&models.Customer{}).Select("zone_id").Where("phone = ?", phone)).
		Pluck("organization_id", &orgs).Error; err != nil {
		return nil, err
	}
	if len(orgs) > 1 {
		return nil, errPhoneAmbiguous
	}
	var cust models.Customer
	if err := config.DB.Where("phone = ?", phone).Order("id").First(&cust).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &cust, nil
}
