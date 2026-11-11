package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// RoleAreaManager is a staff role that manages every zone in one or more
// areas (user_areas). It is the zone manager role widened from one zone to a
// set of zones, and is implemented exactly that way so every existing
// zone-manager rule applies without touching each handler:
//
//   - AdminAuth loads the user and, for an area manager, presents them to the
//     rest of the app as Role "zone_manager" with no single ZoneID and
//     AreaManager = true. Route gates such as RequireRoles("super_admin",
//     "zone_manager") therefore let them in, and they get no more rights than
//     a zone manager has.
//   - OrgZoneIDs / OrgZoneIDsWithDeleted — the zone set almost every
//     customer, package, payment, voucher, alert and report query is
//     constrained to — return only the zones of their areas.
//   - The few handlers that pin a zone manager by users.zone_id (or query
//     zones by organization_id directly) call ZoneInScope / ScopeZones.
//
// /auth/me and the users list still read the stored role, so the admin shows
// "Area manager".
const RoleAreaManager = "area_manager"

func applyAreaManager(claims *Claims) {
	if claims.Role != RoleAreaManager {
		return
	}
	claims.AreaManager = true
	claims.Role = "zone_manager"
	claims.ZoneID = nil
}

// areaZonesSubquery is the ids of the areas assigned to userID.
func userAreaIDs(userID uint) *gorm.DB {
	return config.DB.Model(&models.UserArea{}).Select("area_id").Where("user_id = ?", userID)
}

// limitToAreaZones narrows a query on the zones table to the caller's areas
// when the caller is an area manager; otherwise it returns q unchanged.
func limitToAreaZones(claims *Claims, q *gorm.DB) *gorm.DB {
	if claims == nil || !claims.AreaManager {
		return q
	}
	return q.Where("area_id IN (?)", userAreaIDs(claims.UserID))
}

// IsAreaManager reports whether the caller is an area manager.
func IsAreaManager(c *fiber.Ctx) bool {
	claims := GetClaims(c)
	return claims != nil && claims.AreaManager
}

// AreaZoneIDs is every zone (live only) of the caller's areas. Only
// meaningful for area managers.
func AreaZoneIDs(c *fiber.Ctx) []uint {
	claims := GetClaims(c)
	if claims == nil {
		return nil
	}
	ids := []uint{}
	limitToAreaZones(claims, config.DB.Model(&models.Zone{}).Where("organization_id = ?", claims.OrganizationID)).
		Pluck("id", &ids)
	return ids
}

// ScopeZones narrows a query to the zones the caller may see: col IN (their
// areas' zones) for an area manager, col = users.zone_id for a zone manager
// with a zone, unchanged for everyone else (the caller still constrains to
// the organization). col is the zone-id column, e.g. "id" on zones or
// "zone_id" on customers.
func ScopeZones(c *fiber.Ctx, q *gorm.DB, col string) *gorm.DB {
	claims := GetClaims(c)
	if claims == nil {
		return q
	}
	if claims.AreaManager {
		return q.Where(col+" IN (?)", limitToAreaZones(claims,
			config.DB.Unscoped().Model(&models.Zone{}).Select("id").Where("organization_id = ?", claims.OrganizationID)))
	}
	if claims.Role == "zone_manager" && claims.ZoneID != nil {
		return q.Where(col+" = ?", *claims.ZoneID)
	}
	return q
}

// ZoneInScope reports whether a zone-limited caller (zone or area manager)
// may act on zoneID. For every other role it returns true: they are limited
// by organization, which the caller checks separately.
func ZoneInScope(c *fiber.Ctx, zoneID uint) bool {
	claims := GetClaims(c)
	if claims == nil {
		return false
	}
	if claims.AreaManager {
		var n int64
		limitToAreaZones(claims, config.DB.Model(&models.Zone{}).
			Where("id = ? AND organization_id = ?", zoneID, claims.OrganizationID)).Count(&n)
		return n > 0
	}
	if claims.Role == "zone_manager" && claims.ZoneID != nil {
		return *claims.ZoneID == zoneID
	}
	return true
}
