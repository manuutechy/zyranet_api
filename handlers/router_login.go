package handlers

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
)

// Router logins for the captive portal.
//
// A device gets its router username/password only from POST
// /hotspot/credentials, and only with proof that it is the device that paid:
// a one-time login ticket (handed to the session that just paid, redeemed a
// voucher, verified an OTP, ...) or the device token the portal keeps in
// localStorage. A MAC address alone is never proof — anyone can read or spoof
// one.

// routerLoginError is an error the portal can branch on (code).
func routerLoginError(c *fiber.Ctx, status int, code, msg string) error {
	return c.Status(status).JSON(fiber.Map{"success": false, "error": msg, "message": msg, "code": code})
}

// deviceTokenFrom reads the portal's device token from the body value or the
// X-Device-Token header. Never from the URL: query strings end up in access
// logs.
func deviceTokenFrom(c *fiber.Ctx, fromBody string) string {
	if t := strings.TrimSpace(fromBody); t != "" {
		return t
	}
	return strings.TrimSpace(c.Get("X-Device-Token"))
}

// strongCustomerClaims returns the customer session if it was proven by OTP
// or payment (not one recognised from a MAC address alone).
func strongCustomerClaims(c *fiber.Ctx) *middleware.Claims {
	if cl := middleware.OptionalCustomerClaims(c); cl != nil && cl.CustomerID > 0 && cl.AuthMethod != "device" {
		return cl
	}
	return nil
}

func routerSvc() *services.MikroTikService {
	if mikrotikSvcGlobal != nil {
		return mikrotikSvcGlobal
	}
	return mikrotikSvc
}

// zoneByID loads a zone from a portal-supplied id; nil if missing/unknown.
// (Never falls back to "the first zone": that could be another ISP's.)
func zoneByID(raw string) *models.Zone {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return nil
	}
	var zone models.Zone
	if err := config.DB.First(&zone, raw).Error; err != nil {
		return nil
	}
	return &zone
}

// loginZone picks the router a login should go to now: the zone the device is
// on (if it belongs to the same ISP), else the zone the login was made in.
func loginZone(requested *models.Zone, cred *models.HotspotCredential) *models.Zone {
	if requested != nil && requested.OrganizationID == cred.OrganizationID {
		return requested
	}
	var zone models.Zone
	if err := config.DB.First(&zone, cred.ZoneID).Error; err != nil {
		return nil
	}
	return &zone
}

// pushLoginAsync sends the login straight to the router (instant access when
// the router is reachable, e.g. over WireGuard). Best-effort.
func pushLoginAsync(zone *models.Zone, cred *models.HotspotCredential) {
	svc := routerSvc()
	if svc == nil || zone == nil || cred == nil {
		return
	}
	z, cr := *zone, *cred
	go func() {
		if err := svc.PushCredential(&z, &cr); err != nil {
			log.Printf("[router-login] direct push of login %d to zone %d: %v (the 1-minute sync will deliver it)", cr.ID, z.ID, err)
		}
	}()
}

// grantRouterLogin gives a proven customer's device its own login (moving the
// plan onto this MAC if needed) and returns the router_login the portal
// exchanges for the username/password. withDevice also mints a device token.
func grantRouterLogin(customerID uint, mac string, requested *models.Zone, kind string, withDevice bool) (*services.RouterLogin, *models.HotspotCredential, error) {
	var cred *models.HotspotCredential
	var err error
	if requested != nil {
		cred, err = services.MoveCredential(customerID, requested.ID, mac, 0)
		if err == nil && kind != "" && cred.Kind != kind {
			config.DB.Model(cred).Update("kind", kind)
		}
	} else {
		cred, err = services.EnsureCredential(customerID, mac, kind)
	}
	if err != nil {
		return nil, nil, err
	}
	rl, err := services.IssueRouterLogin(cred, withDevice)
	if err != nil {
		return nil, nil, err
	}
	pushLoginAsync(loginZone(requested, cred), cred)
	return rl, cred, nil
}

// HotspotCredentials exchanges proof (a one-time login ticket, or the
// device's token) for the device's router username/password.
//
// POST /hotspot/credentials { ticket?, device_token?, mac, zone_id? }
func HotspotCredentials(c *fiber.Ctx) error {
	var body struct {
		Ticket      string `json:"ticket"`
		DeviceToken string `json:"device_token"`
		Mac         string `json:"mac"`
		ZoneID      string `json:"zone_id"`
	}
	_ = c.BodyParser(&body)
	mac, ok := services.NormalizeMAC(body.Mac)
	if !ok {
		return routerLoginError(c, fiber.StatusBadRequest, "bad_mac", "Open this page from the WiFi login screen so we can see your device.")
	}
	zone := zoneByID(body.ZoneID)

	var cred *models.HotspotCredential
	var err error
	newDeviceToken := ""
	switch {
	case strings.TrimSpace(body.Ticket) != "":
		cred, err = services.ConsumeLoginTicket(body.Ticket)
		if err == nil && cred.MacAddress != mac {
			err = services.ErrInvalidProof
		}
	case deviceTokenFrom(c, body.DeviceToken) != "":
		cred, err = services.CredentialByDeviceToken(deviceTokenFrom(c, body.DeviceToken))
		if err == nil && cred.MacAddress != mac {
			// Same phone, new MAC (randomised / reset): the token is the proof.
			if zone == nil {
				zone = loginZone(nil, cred)
			}
			if zone == nil || zone.OrganizationID != cred.OrganizationID {
				return routerLoginError(c, fiber.StatusForbidden, "wrong_network", "This device's plan is for a different WiFi network.")
			}
			moved, mErr := services.MoveCredential(cred.CustomerID, zone.ID, mac, cred.ID)
			if mErr != nil {
				err = mErr
			} else {
				// The old login's tokens died with it: hand over a new one.
				if rl, iErr := services.IssueRouterLogin(moved, true); iErr == nil {
					newDeviceToken = rl.DeviceToken
				}
				cred = moved
			}
		}
	default:
		return routerLoginError(c, fiber.StatusUnauthorized, "proof_required", "Please sign back in with your phone number.")
	}
	if err != nil {
		switch {
		case errors.Is(err, services.ErrNoActivePlan):
			return routerLoginError(c, fiber.StatusPaymentRequired, "expired", "Your plan has ended. Pick a plan to continue.")
		case errors.Is(err, services.ErrWrongISP):
			return routerLoginError(c, fiber.StatusForbidden, "wrong_network", "This device's plan is for a different WiFi network.")
		}
		return routerLoginError(c, fiber.StatusUnauthorized, "proof_required", "Please sign back in with your phone number.")
	}
	if zone != nil && zone.OrganizationID != cred.OrganizationID {
		return routerLoginError(c, fiber.StatusForbidden, "wrong_network", "This device's plan is for a different WiFi network.")
	}

	// A renewal may have happened since the login lapsed: bring it current.
	if cred.Status != "active" || !cred.ExpiresAt.After(time.Now()) {
		renewed, rErr := services.EnsureCredential(cred.CustomerID, cred.MacAddress, "")
		if rErr != nil {
			return routerLoginError(c, fiber.StatusPaymentRequired, "expired", "Your plan has ended. Pick a plan to continue.")
		}
		cred = renewed
	}
	services.TouchCredentialLogin(cred.ID)

	target := loginZone(zone, cred)
	ready, retry := false, 65
	if target != nil {
		ready, retry = services.CredentialReady(cred, target)
		if !ready {
			pushLoginAsync(target, cred)
		}
	}

	planName, speed := "Internet Package", ""
	var pkg models.Package
	if config.DB.First(&pkg, cred.PackageID).Error == nil {
		planName = pkg.Name
		speed = fmt.Sprintf("%.0f Mbps", float64(pkg.SpeedDownloadKbps)/1024)
	}
	resp := fiber.Map{
		"username":    cred.Username,
		"password":    cred.Password,
		"expires_at":  cred.ExpiresAt,
		"plan_name":   planName,
		"speed":       speed,
		"ready":       ready,
		"retry_after": retry,
	}
	if newDeviceToken != "" {
		resp["device_token"] = newDeviceToken // replaces the stored one
	}
	return utils.SuccessResponse(c, resp, "Router login.")
}

// CustomerRecoverByReceipt lets a customer whose device lost its sign-in (or
// whose phone changed its MAC address) move their remaining time to this
// device with the M-Pesa code AND the phone number that paid. Both must
// match the same completed payment of this WiFi's ISP. Nothing about the
// payment or the customer is revealed on failure.
//
// POST /customer/auth/recover { phone, code, mac, ip?, zone_id }
func CustomerRecoverByReceipt(c *fiber.Ctx) error {
	var body struct {
		Phone  string `json:"phone"`
		Code   string `json:"code"`
		Mac    string `json:"mac"`
		IP     string `json:"ip"`
		ZoneID string `json:"zone_id"`
	}
	_ = c.BodyParser(&body)
	phone := normalizePhone(body.Phone)
	code := strings.ToUpper(strings.TrimSpace(body.Code))
	mac, ok := services.NormalizeMAC(body.Mac)
	zone := zoneByID(body.ZoneID)
	if !ok || zone == nil {
		return routerLoginError(c, fiber.StatusBadRequest, "bad_request", "Open this page from the WiFi login screen so we can see your device.")
	}
	if len(phone) < 9 || len(code) < 8 {
		return routerLoginError(c, fiber.StatusUnprocessableEntity, "bad_request", "Enter the phone number that paid and the M-Pesa code from the SMS.")
	}
	notMatched := func() error {
		return routerLoginError(c, fiber.StatusNotFound, "not_matched", "We couldn't match that M-Pesa code to that phone number.")
	}

	var payment models.Payment
	if err := config.DB.Where("UPPER(mpesa_receipt_number) = ? AND status = ?", code, "completed").
		Order("id DESC").First(&payment).Error; err != nil || payment.CustomerID == nil {
		return notMatched()
	}
	if normalizePhone(payment.Phone) != phone {
		return notMatched()
	}
	if org, ok := services.ZoneOrganization(payment.ZoneID); !ok || org != zone.OrganizationID {
		return notMatched()
	}

	rl, cred, err := grantRouterLogin(*payment.CustomerID, mac, zone, "", true)
	if err != nil {
		if errors.Is(err, services.ErrNoActivePlan) {
			return routerLoginError(c, fiber.StatusPaymentRequired, "expired", "The plan paid with that code has ended. Pick a plan to continue.")
		}
		return notMatched()
	}
	log.Printf("[router-login] customer %d moved to device %s by M-Pesa code (zone %d)", cred.CustomerID, mac, zone.ID)

	var customer models.Customer
	config.DB.Preload("Package").Preload("Zone").First(&customer, cred.CustomerID)
	token, _ := middleware.GenerateCustomerToken(customer.ID)
	if token != "" {
		middleware.SetAuthCookie(c, middleware.CustomerCookieName, token)
	}
	planName := ""
	if customer.Package != nil {
		planName = customer.Package.Name
	}
	return utils.SuccessResponse(c, fiber.Map{
		"token":        token,
		"customer":     buildCustomerProfile(&customer),
		"router_login": rl,
		"plan_name":    planName,
		"expires_at":   cred.ExpiresAt,
	}, "Welcome back. Your plan is now on this device.")
}

// ZoneAcceptingPaymentsPublic tells the portal whether this WiFi can take a
// payment right now (its router has checked in within 5 minutes).
//
// GET /public/zones/accepting-payments?zone_id=
func ZoneAcceptingPaymentsPublic(c *fiber.Ctx) error {
	zone := zoneByID(c.Query("zone_id"))
	if zone == nil {
		return utils.SuccessResponse(c, fiber.Map{"accepting": false, "message": "This WiFi isn't set up for payments yet."}, "")
	}
	ok, msg := services.ZoneAcceptingPayments(zone.ID)
	return utils.SuccessResponse(c, fiber.Map{"accepting": ok, "message": msg}, "")
}

// zoneOfflineResponse is the checkout refusal when the router is offline.
func zoneOfflineResponse(c *fiber.Ctx, msg string) error {
	if msg == "" {
		msg = services.ZoneOfflineMessage
	}
	return routerLoginError(c, fiber.StatusServiceUnavailable, "zone_offline", msg)
}
