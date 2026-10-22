package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Per-device router logins.
//
// Every device that is paid up gets its own hotspot user on the ISP's routers:
// a random username and password (crypto/rand), bound to the device's MAC
// (the router's mac-address field), with limit-uptime capped at what was paid
// for. The portal only learns the password after proving it is that device —
// see IssueRouterLogin / ConsumeLoginTicket / CredentialByDeviceToken.

const (
	credUserAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no look-alikes
	credPassAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"

	loginTicketTTL   = 5 * time.Minute
	deviceTokenTTL   = 365 * 24 * time.Hour
	maxDeviceTokens  = 5
	credStatusActive = "active"
)

var (
	ErrNoActivePlan  = errors.New("no active plan")
	ErrBadMAC        = errors.New("invalid device MAC address")
	ErrInvalidProof  = errors.New("invalid or expired sign-in proof")
	ErrWrongISP      = errors.New("this device belongs to another network")
	ErrPlanExpired   = errors.New("plan expired")
	errCredCollision = errors.New("credential username collision")
)

// MigrateRouterAccess creates the tables the per-device login system uses.
// Additive only (new tables); run at startup.
func MigrateRouterAccess(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.HotspotCredential{},
		&models.HotspotAccessToken{},
		&models.ZoneOutage{},
		&models.FreeTrialClaim{},
	)
}

// NormalizeMAC returns AA:BB:CC:DD:EE:FF, or ok=false for anything that isn't
// a MAC (so a bad value never reaches a router command).
func NormalizeMAC(mac string) (string, bool) { return RadiusMAC(mac) }

func randomFrom(alphabet string, n int) (string, error) {
	var sb strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		sb.WriteByte(alphabet[v.Int64()])
	}
	return sb.String(), nil
}

// NewCredentialSecrets returns a fresh router username ("zn" + 8 random
// characters) and a 14-character random password.
func NewCredentialSecrets() (username, password string, err error) {
	u, err := randomFrom(credUserAlphabet, 8)
	if err != nil {
		return "", "", err
	}
	p, err := randomFrom(credPassAlphabet, 14)
	if err != nil {
		return "", "", err
	}
	return "zn" + u, p, nil
}

func newOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashAccessToken is what is stored for a device token / login ticket.
func HashAccessToken(t string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(t)))
	return hex.EncodeToString(sum[:])
}

// ZoneOrganization returns the organization that owns a zone.
func ZoneOrganization(zoneID uint) (uint, bool) {
	var zone models.Zone
	if err := config.DB.Select("id", "organization_id").First(&zone, zoneID).Error; err != nil || zone.OrganizationID == 0 {
		return 0, false
	}
	return zone.OrganizationID, true
}

// RouterLogin is handed to the browser that proved it is the device: a
// one-time ticket to fetch the router login now, and a device token to keep.
type RouterLogin struct {
	LoginTicket string    `json:"login_ticket"`
	DeviceToken string    `json:"device_token,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// IssueRouterLogin mints a login ticket (and, if withDevice, a device token)
// for the credential.
func IssueRouterLogin(cred *models.HotspotCredential, withDevice bool) (*RouterLogin, error) {
	now := time.Now()
	ticket, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	rows := []models.HotspotAccessToken{{
		CredentialID: cred.ID, Kind: "ticket", TokenHash: HashAccessToken(ticket), ExpiresAt: now.Add(loginTicketTTL),
	}}
	rl := &RouterLogin{LoginTicket: ticket, ExpiresAt: cred.ExpiresAt}
	if withDevice {
		dev, err := newOpaqueToken()
		if err != nil {
			return nil, err
		}
		rows = append(rows, models.HotspotAccessToken{
			CredentialID: cred.ID, Kind: "device", TokenHash: HashAccessToken(dev), ExpiresAt: now.Add(deviceTokenTTL),
		})
		rl.DeviceToken = dev
	}
	err = config.DB.Transaction(func(tx *gorm.DB) error {
		// Tidy: used/expired tickets, and old device tokens beyond the cap.
		tx.Where("credential_id = ? AND kind = ? AND (used_at IS NOT NULL OR expires_at < ?)", cred.ID, "ticket", now).
			Delete(&models.HotspotAccessToken{})
		if err := tx.Create(&rows).Error; err != nil {
			return err
		}
		if withDevice {
			var keep []uint
			tx.Model(&models.HotspotAccessToken{}).
				Where("credential_id = ? AND kind = ?", cred.ID, "device").
				Order("id DESC").Limit(maxDeviceTokens).Pluck("id", &keep)
			if len(keep) > 0 {
				tx.Where("credential_id = ? AND kind = ? AND id NOT IN ?", cred.ID, "device", keep).
					Delete(&models.HotspotAccessToken{})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rl, nil
}

// ConsumeLoginTicket redeems a one-time ticket. It succeeds once.
func ConsumeLoginTicket(ticket string) (*models.HotspotCredential, error) {
	if strings.TrimSpace(ticket) == "" {
		return nil, ErrInvalidProof
	}
	now := time.Now()
	var tok models.HotspotAccessToken
	if err := config.DB.Where("token_hash = ? AND kind = ?", HashAccessToken(ticket), "ticket").First(&tok).Error; err != nil {
		return nil, ErrInvalidProof
	}
	res := config.DB.Model(&models.HotspotAccessToken{}).
		Where("id = ? AND used_at IS NULL AND expires_at > ?", tok.ID, now).
		Update("used_at", now)
	if res.Error != nil || res.RowsAffected != 1 {
		return nil, ErrInvalidProof
	}
	var cred models.HotspotCredential
	if err := config.DB.First(&cred, tok.CredentialID).Error; err != nil {
		return nil, ErrInvalidProof
	}
	return &cred, nil
}

// CredentialByDeviceToken resolves a device token to its credential (any
// status: an expired plan still identifies the device and its customer).
func CredentialByDeviceToken(token string) (*models.HotspotCredential, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrInvalidProof
	}
	var tok models.HotspotAccessToken
	if err := config.DB.Where("token_hash = ? AND kind = ? AND expires_at > ?", HashAccessToken(token), "device", time.Now()).
		First(&tok).Error; err != nil {
		return nil, ErrInvalidProof
	}
	var cred models.HotspotCredential
	if err := config.DB.First(&cred, tok.CredentialID).Error; err != nil {
		return nil, ErrInvalidProof
	}
	return &cred, nil
}

// CustomerHoldsMAC reports whether the customer has a login for this MAC
// (any status) inside the organization.
func CustomerHoldsMAC(orgID, customerID uint, mac string) bool {
	m, ok := NormalizeMAC(mac)
	if !ok {
		return false
	}
	var n int64
	config.DB.Model(&models.HotspotCredential{}).
		Where("organization_id = ? AND customer_id = ? AND mac_address = ?", orgID, customerID, m).Count(&n)
	return n > 0
}

// customerActive reports whether the customer is paid up right now.
func customerActive(c *models.Customer, now time.Time) bool {
	return c.Status == "active" && c.ExpiresAt != nil && c.ExpiresAt.After(now)
}

func deviceLimit(pkg *models.Package) int {
	if pkg == nil || pkg.DeviceLimit < 1 {
		return 1
	}
	return pkg.DeviceLimit
}

// EnsureCredential gives the customer's device its own active router login
// matching the customer's current plan (creating or re-activating it), and
// enforces the plan's device limit by retiring the least recently used other
// devices. kind is paid | voucher | trial.
func EnsureCredential(customerID uint, mac, kind string) (*models.HotspotCredential, error) {
	var out *models.HotspotCredential
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		c, err := ensureCredentialTx(tx, customerID, mac, kind, 0)
		out = c
		return err
	})
	return out, err
}

// MoveCredential moves a proven customer's remaining time onto newMAC (a
// phone that randomised its MAC, or a new phone). fromCredentialID, when the
// proof was a device token, is that device's old login and is retired even
// if the plan allows several devices. All in one transaction, scoped to the
// organization that owns zoneID.
func MoveCredential(customerID, zoneID uint, newMAC string, fromCredentialID uint) (*models.HotspotCredential, error) {
	orgID, ok := ZoneOrganization(zoneID)
	if !ok {
		return nil, ErrWrongISP
	}
	var out *models.HotspotCredential
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		var cust models.Customer
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cust, customerID).Error; err != nil {
			return ErrNoActivePlan
		}
		if custOrg, ok := zoneOrgTx(tx, cust.ZoneID); !ok || custOrg != orgID {
			return ErrWrongISP
		}
		c, err := ensureCredentialTx(tx, customerID, newMAC, "", fromCredentialID)
		if err != nil {
			return err
		}
		out = c
		return nil
	})
	return out, err
}

func zoneOrgTx(tx *gorm.DB, zoneID uint) (uint, bool) {
	var zone models.Zone
	if err := tx.Select("id", "organization_id").First(&zone, zoneID).Error; err != nil || zone.OrganizationID == 0 {
		return 0, false
	}
	return zone.OrganizationID, true
}

func ensureCredentialTx(tx *gorm.DB, customerID uint, mac, kind string, retireCredID uint) (*models.HotspotCredential, error) {
	now := time.Now()
	m, ok := NormalizeMAC(mac)
	if !ok {
		return nil, ErrBadMAC
	}
	var cust models.Customer
	if err := tx.Preload("Package").First(&cust, customerID).Error; err != nil {
		return nil, ErrNoActivePlan
	}
	if !customerActive(&cust, now) || cust.Package == nil {
		return nil, ErrNoActivePlan
	}
	orgID, ok := zoneOrgTx(tx, cust.ZoneID)
	if !ok {
		return nil, ErrWrongISP
	}

	var cred models.HotspotCredential
	err := tx.Where("organization_id = ? AND customer_id = ? AND mac_address = ?", orgID, cust.ID, m).First(&cred).Error
	switch {
	case err == nil:
		updates := map[string]interface{}{
			"package_id": cust.PackageID,
			"expires_at": *cust.ExpiresAt,
			"zone_id":    cust.ZoneID,
		}
		if kind != "" {
			updates["kind"] = kind
		}
		if cred.Status != credStatusActive {
			updates["status"] = credStatusActive
			updates["activated_at"] = now
			updates["revoked_at"] = nil
		}
		if err := tx.Model(&cred).Updates(updates).Error; err != nil {
			return nil, err
		}
		tx.First(&cred, cred.ID)
	case errors.Is(err, gorm.ErrRecordNotFound):
		if kind == "" {
			kind = "paid"
		}
		var created bool
		for attempt := 0; attempt < 3 && !created; attempt++ {
			user, pass, err := NewCredentialSecrets()
			if err != nil {
				return nil, err
			}
			cred = models.HotspotCredential{
				OrganizationID: orgID, ZoneID: cust.ZoneID, CustomerID: cust.ID, PackageID: cust.PackageID,
				MacAddress: m, Username: user, Password: pass, Kind: kind, Status: credStatusActive,
				ActivatedAt: now, ExpiresAt: *cust.ExpiresAt,
			}
			if err := tx.Create(&cred).Error; err != nil {
				// Username collision (vanishingly rare) — or a concurrent
				// request already created this device's row: use that one.
				var existing models.HotspotCredential
				if tx.Where("organization_id = ? AND customer_id = ? AND mac_address = ?", orgID, cust.ID, m).First(&existing).Error == nil {
					cred = existing
					created = true
					break
				}
				continue
			}
			created = true
		}
		if !created {
			return nil, errCredCollision
		}
	default:
		return nil, err
	}

	// The device that proved itself with an old token is the same phone with
	// a new MAC: retire its old login.
	if retireCredID != 0 && retireCredID != cred.ID {
		retireCredentials(tx, "id = ? AND customer_id = ? AND organization_id = ?", retireCredID, cust.ID, orgID)
	}

	// Device limit: keep the newest (limit-1) other active devices.
	limit := deviceLimit(cust.Package)
	var others []models.HotspotCredential
	tx.Where("organization_id = ? AND customer_id = ? AND status = ? AND id <> ?", orgID, cust.ID, credStatusActive, cred.ID).
		Find(&others)
	if len(others) > limit-1 {
		sortCredsByRecentUse(others)
		var retire []uint
		for _, o := range others[limit-1:] {
			retire = append(retire, o.ID)
		}
		retireCredentials(tx, "id IN ?", retire)
	}
	return &cred, nil
}

// sortCredsByRecentUse orders most recently used first.
func sortCredsByRecentUse(cs []models.HotspotCredential) {
	last := func(c models.HotspotCredential) time.Time {
		if c.LastLoginAt != nil && c.LastLoginAt.After(c.ActivatedAt) {
			return *c.LastLoginAt
		}
		return c.ActivatedAt
	}
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && last(cs[j]).After(last(cs[j-1])); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

// retireCredentials revokes logins (the router drops them on its next sync)
// and kills their device tokens.
func retireCredentials(tx *gorm.DB, where string, args ...interface{}) {
	now := time.Now()
	var ids []uint
	tx.Model(&models.HotspotCredential{}).Where(where, args...).Where("status = ?", credStatusActive).Pluck("id", &ids)
	if len(ids) == 0 {
		return
	}
	tx.Model(&models.HotspotCredential{}).Where("id IN ?", ids).
		Updates(map[string]interface{}{"status": "revoked", "revoked_at": now})
	tx.Where("credential_id IN ?", ids).Delete(&models.HotspotAccessToken{})
}

// ActiveCredentialsForMAC returns the active logins bound to a MAC inside an
// organization.
func ActiveCredentialsForMAC(orgID uint, mac string) []models.HotspotCredential {
	m, ok := NormalizeMAC(mac)
	if !ok {
		return nil
	}
	var creds []models.HotspotCredential
	config.DB.Where("organization_id = ? AND mac_address = ? AND status = ? AND expires_at > ?", orgID, m, credStatusActive, time.Now()).
		Find(&creds)
	return creds
}

// TouchCredentialLogin records that the device fetched its login (used to
// pick which device to retire when the device limit is hit).
func TouchCredentialLogin(credID uint) {
	now := time.Now()
	config.DB.Model(&models.HotspotCredential{}).Where("id = ?", credID).UpdateColumn("last_login_at", now)
}

// ---------------------------------------------------------------------------
// Reconciler: keeps logins in step with customers whatever activated them
// (STK, Paybill/C2B, manual payment, credit, voucher, outage compensation).
// Only rows that changed since the last run are looked at.

// ReconcileCredentials processes customers/devices changed since `since`, and
// expires logins whose time is up. Returns how many customers were looked at.
func ReconcileCredentials(since time.Time) int {
	now := time.Now()

	// Time-based expiry: one statement, no per-row work.
	config.DB.Model(&models.HotspotCredential{}).
		Where("status = ? AND expires_at <= ?", credStatusActive, now).
		Updates(map[string]interface{}{"status": "expired"})

	var ids []uint
	config.DB.Model(&models.Customer{}).Where("updated_at > ?", since).Pluck("id", &ids)
	var devIDs []uint
	config.DB.Model(&models.CustomerDevice{}).Where("updated_at > ?", since).Pluck("customer_id", &devIDs)
	seen := map[uint]bool{}
	for _, id := range append(ids, devIDs...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		reconcileCustomer(id, now)
	}
	return len(seen)
}

// ReconcileCustomer brings one customer's logins in line now.
func ReconcileCustomer(customerID uint) { reconcileCustomer(customerID, time.Now()) }

func reconcileCustomer(customerID uint, now time.Time) {
	var cust models.Customer
	if err := config.DB.Preload("Package").First(&cust, customerID).Error; err != nil {
		return
	}
	if !customerActive(&cust, now) || cust.Type == "pppoe" {
		config.DB.Model(&models.HotspotCredential{}).
			Where("customer_id = ? AND status = ?", cust.ID, credStatusActive).
			Updates(map[string]interface{}{"status": "expired"})
		return
	}
	// Paid up: existing active logins follow the plan's expiry/package...
	config.DB.Model(&models.HotspotCredential{}).
		Where("customer_id = ? AND status = ? AND (expires_at <> ? OR package_id <> ?)", cust.ID, credStatusActive, *cust.ExpiresAt, cust.PackageID).
		Updates(map[string]interface{}{"expires_at": *cust.ExpiresAt, "package_id": cust.PackageID})
	// ...and if no device has a login yet (Paybill / manual / admin
	// activation), the device on the account gets one. Only then: the
	// account's MAC field can be set by unproven requests, so it must never
	// push out a device that already proved itself (device limit).
	var holding int64
	config.DB.Model(&models.HotspotCredential{}).Where("customer_id = ? AND status = ?", cust.ID, credStatusActive).Count(&holding)
	if holding == 0 && cust.MacAddress != nil && *cust.MacAddress != "" {
		if _, err := EnsureCredential(cust.ID, *cust.MacAddress, ""); err != nil && !errors.Is(err, ErrBadMAC) {
			log.Printf("[credentials] customer %d: %v", cust.ID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Router readiness: has the router got this login yet?

var credPushed sync.Map // "credID:zoneID" -> time.Time of a successful direct push

// MarkCredentialPushed records a successful direct API push to a router.
func MarkCredentialPushed(credID, zoneID uint) {
	credPushed.Store(fmt.Sprintf("%d:%d", credID, zoneID), time.Now())
}

// CredentialReady says whether the zone's router already has this login, and
// if not, roughly how many seconds until its next sync brings it.
func CredentialReady(cred *models.HotspotCredential, zone *models.Zone) (bool, int) {
	if v, ok := credPushed.Load(fmt.Sprintf("%d:%d", cred.ID, zone.ID)); ok {
		if t, ok := v.(time.Time); ok && !t.Before(cred.UpdatedAt) {
			return true, 0
		}
	}
	if zone.LastSyncAt != nil && zone.LastSyncAt.After(cred.UpdatedAt.Add(time.Second)) {
		return true, 0
	}
	wait := 65
	if zone.LastSyncAt != nil {
		wait = 65 - int(time.Since(*zone.LastSyncAt).Seconds())
	}
	if wait < 5 {
		wait = 5
	}
	if wait > 70 {
		wait = 70
	}
	return false, wait
}
