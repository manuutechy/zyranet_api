package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// RequireRoles returns a middleware that only allows users with one of the given roles.
func RequireRoles(roles ...string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		role, ok := c.Locals("role").(string)
		if !ok || role == "" {
			return utils.ErrorResponse(c, "Unauthorized.", "Role information missing.", fiber.StatusForbidden)
		}
		for _, r := range roles {
			if r == role {
				return c.Next()
			}
		}
		return utils.ErrorResponse(c, "Forbidden.", "You do not have permission to perform this action.", fiber.StatusForbidden)
	}
}

// IsSuperAdmin is a convenience middleware for super_admin only routes.
func IsSuperAdmin() fiber.Handler {
	return RequireRoles("super_admin")
}

// IsFinanceOrAdmin allows super_admin and finance roles.
func IsFinanceOrAdmin() fiber.Handler {
	return RequireRoles("super_admin", "finance")
}

// IsManagerOrAdmin allows super_admin and zone_manager.
func IsManagerOrAdmin() fiber.Handler {
	return RequireRoles("super_admin", "zone_manager")
}

// CanManagePayments allows super_admin, zone_manager, and finance.
func CanManagePayments() fiber.Handler {
	return RequireRoles("super_admin", "zone_manager", "finance")
}

// CanManageVouchers allows super_admin, zone_manager, and field_agent.
func CanManageVouchers() fiber.Handler {
	return RequireRoles("super_admin", "zone_manager", "field_agent")
}

// OrgZoneIDs returns every Zone ID belonging to the caller's Organization.
// Every zone-scoping check in the handlers (zones, customers, vouchers,
// reports, alerts) should additionally constrain against this set so that
// no admin — including a super_admin — can see or act on another
// Organization's zones. zone_manager's existing single-zone restriction is
// applied on top of this, not instead of it.
func OrgZoneIDs(c *fiber.Ctx) ([]uint, error) {
	claims := GetClaims(c)
	if claims == nil {
		return nil, fiber.ErrUnauthorized
	}
	var ids []uint
	if err := config.DB.Model(&models.Zone{}).
		Where("organization_id = ?", claims.OrganizationID).
		Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
