package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// RADIUS integration.
//
// FreeRADIUS (rlm_sql) reads its per-user data from MySQL tables it owns:
// radcheck (what must match / be true for a login), radreply (what to tell the
// router). This service keeps those tables in step with who is currently paid
// up, so the router can ask "may this device in, for how long, at what speed?"
// without the API being reachable at that moment.
//
// The trick that makes expiry robust: radcheck carries an `Expiration` for
// each user. FreeRADIUS's expiration module rejects logins after it AND replies
// with Session-Timeout = the time remaining *at login*, so the router ends the
// session exactly at expiry even if the API is down.
//
// The API stays the source of truth (customers, payments). RADIUS rows are
// derived from it and rebuilt by a reconciler, so a missed update heals itself.

const radiusReconcileEvery = 30 * time.Second

// radiusInterim asks the router for a usage update every 5 minutes, so
// sessions and byte counts appear in the logs while a customer is online.
const radiusInterim = "300"

var macRe = regexp.MustCompile(`^([0-9A-F]{2}:){5}[0-9A-F]{2}$`)

// RadiusMAC normalises a MAC to the form the router sends as the RADIUS
// username (AA:BB:CC:DD:EE:FF). ok is false if it isn't a valid MAC.
func RadiusMAC(mac string) (string, bool) {
	m := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(mac), "-", ":"))
	return m, macRe.MatchString(m)
}

type radiusAttr struct{ Attribute, Op, Value string }

type radiusUser struct {
	Username           string
	Kind               string // hotspot | pppoe
	CustomerID, ZoneID uint
	Check, Reply       []radiusAttr
}

// hash fingerprints everything written for the user, so an unchanged user
// costs nothing on each reconcile.
func (u radiusUser) hash() string {
	h := sha256.New()
	fmt.Fprint(h, u.Username, "|", u.Kind, "|", u.CustomerID, "|", u.ZoneID)
	for _, group := range [][]radiusAttr{u.Check, u.Reply} {
		for _, a := range group {
			fmt.Fprint(h, "|", a.Attribute, a.Op, a.Value)
		}
		fmt.Fprint(h, "#")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// radiusExpiration formats a time the way FreeRADIUS's Expiration attribute
// expects. FreeRADIUS reads it in the server's local time; the server runs UTC.
func radiusExpiration(t time.Time) string { return t.UTC().Format("Jan 02 2006 15:04:05") }

// desiredRadiusUsers works out the RADIUS accounts a customer should have right
// now. A customer who is not active and paid up gets none — so the router
// rejects them. deviceMACs are the customer's known devices, most recent first.
func desiredRadiusUsers(c *models.Customer, pkg *models.Package, deviceMACs []string, now time.Time) []radiusUser {
	if c.Status != "active" || c.ExpiresAt == nil || !c.ExpiresAt.After(now) || pkg == nil {
		return nil
	}
	exp := radiusExpiration(*c.ExpiresAt)
	reply := []radiusAttr{
		// The router's rx/tx are the client's upload/download — same order the
		// API-driven profiles already use.
		{"Mikrotik-Rate-Limit", ":=", fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)},
		{"Acct-Interim-Interval", ":=", radiusInterim},
	}

	if c.Type == "pppoe" {
		if c.PPPoEUsername == nil || strings.TrimSpace(*c.PPPoEUsername) == "" || c.PPPoEPassword == nil || *c.PPPoEPassword == "" {
			return nil
		}
		return []radiusUser{{
			Username: strings.TrimSpace(*c.PPPoEUsername), Kind: "pppoe", CustomerID: c.ID, ZoneID: c.ZoneID,
			Check: []radiusAttr{{"Cleartext-Password", ":=", *c.PPPoEPassword}, {"Expiration", ":=", exp}},
			Reply: reply,
		}}
	}

	// Hotspot: the device's MAC is the identity (as the existing MAC whitelist
	// model already treats it). A plan's device_limit caps how many devices.
	limit := pkg.DeviceLimit
	if limit < 1 {
		limit = 1
	}
	var macs []string
	seen := map[string]bool{}
	candidates := deviceMACs
	if c.MacAddress != nil {
		candidates = append([]string{*c.MacAddress}, deviceMACs...)
	}
	for _, raw := range candidates {
		mac, ok := RadiusMAC(raw)
		if !ok || seen[mac] {
			continue
		}
		seen[mac] = true
		macs = append(macs, mac)
		if len(macs) == limit {
			break
		}
	}
	users := make([]radiusUser, 0, len(macs))
	for _, mac := range macs {
		users = append(users, radiusUser{
			Username: mac, Kind: "hotspot", CustomerID: c.ID, ZoneID: c.ZoneID,
			Check: []radiusAttr{{"Auth-Type", ":=", "Accept"}, {"Expiration", ":=", exp}},
			Reply: reply,
		})
	}
	return users
}

// RadiusService keeps FreeRADIUS's tables in step with the customer database.
type RadiusService struct {
	stop chan struct{}
}

func NewRadiusService() *RadiusService { return &RadiusService{stop: make(chan struct{})} }

var (
	radiusTablesMu    sync.Mutex
	radiusTablesOK    bool
	radiusTablesUntil time.Time
)

// RadiusTablesExist reports whether FreeRADIUS's schema is present in this
// database (it is created by installing FreeRADIUS, not by this app). Cached
// briefly because the logs endpoints call it on every request.
func RadiusTablesExist() bool {
	radiusTablesMu.Lock()
	defer radiusTablesMu.Unlock()
	if time.Now().Before(radiusTablesUntil) {
		return radiusTablesOK
	}
	m := config.DB.Migrator()
	radiusTablesOK = m.HasTable("radcheck") && m.HasTable("radreply")
	radiusTablesUntil = time.Now().Add(30 * time.Second)
	return radiusTablesOK
}

// ResetRadiusTableCache forgets the cached answer (tests, and right after the
// schema is installed).
func ResetRadiusTableCache() {
	radiusTablesMu.Lock()
	radiusTablesUntil = time.Time{}
	radiusTablesMu.Unlock()
}

var ErrRadiusNotInstalled = errors.New("the RADIUS server's database tables are not installed yet")

// writeUser replaces a user's RADIUS rows and records the mapping.
func writeUser(tx *gorm.DB, u radiusUser) error {
	if err := tx.Exec("DELETE FROM radcheck WHERE username = ?", u.Username).Error; err != nil {
		return err
	}
	if err := tx.Exec("DELETE FROM radreply WHERE username = ?", u.Username).Error; err != nil {
		return err
	}
	for _, a := range u.Check {
		if err := tx.Exec("INSERT INTO radcheck (username, attribute, op, value) VALUES (?, ?, ?, ?)", u.Username, a.Attribute, a.Op, a.Value).Error; err != nil {
			return err
		}
	}
	for _, a := range u.Reply {
		if err := tx.Exec("INSERT INTO radreply (username, attribute, op, value) VALUES (?, ?, ?, ?)", u.Username, a.Attribute, a.Op, a.Value).Error; err != nil {
			return err
		}
	}
	var acct models.RadiusAccount
	err := tx.Where("username = ?", u.Username).First(&acct).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.Create(&models.RadiusAccount{Username: u.Username, CustomerID: u.CustomerID, ZoneID: u.ZoneID, Kind: u.Kind, Active: true, AttrHash: u.hash()}).Error
	}
	if err != nil {
		return err
	}
	return tx.Model(&acct).Updates(map[string]interface{}{
		"customer_id": u.CustomerID, "zone_id": u.ZoneID, "kind": u.Kind, "active": true, "attr_hash": u.hash(),
	}).Error
}

// deactivateUser removes the credentials (so the router rejects the user) but
// keeps the username→customer mapping so their past sessions stay attributable.
func deactivateUser(tx *gorm.DB, username string) error {
	if err := tx.Exec("DELETE FROM radcheck WHERE username = ?", username).Error; err != nil {
		return err
	}
	if err := tx.Exec("DELETE FROM radreply WHERE username = ?", username).Error; err != nil {
		return err
	}
	return tx.Model(&models.RadiusAccount{}).Where("username = ?", username).
		Updates(map[string]interface{}{"active": false, "attr_hash": ""}).Error
}

// deviceMACsFor returns each customer's device MACs, most recently seen first.
func deviceMACsFor(customerIDs []uint) map[uint][]string {
	out := map[uint][]string{}
	if len(customerIDs) == 0 {
		return out
	}
	var devs []models.CustomerDevice
	config.DB.Where("customer_id IN ?", customerIDs).Order("last_seen_at DESC").Find(&devs)
	for _, d := range devs {
		out[d.CustomerID] = append(out[d.CustomerID], d.MacAddress)
	}
	return out
}

// SyncZone makes the RADIUS tables match a zone exactly: every paid-up customer
// present with current expiry/speed, everyone else absent.
func (s *RadiusService) SyncZone(zoneID uint) error {
	if !RadiusTablesExist() {
		return ErrRadiusNotInstalled
	}
	var customers []models.Customer
	if err := config.DB.Where("zone_id = ?", zoneID).Find(&customers).Error; err != nil {
		return err
	}
	pkgIDs := map[uint]bool{}
	ids := make([]uint, 0, len(customers))
	for _, c := range customers {
		ids = append(ids, c.ID)
		pkgIDs[c.PackageID] = true
	}
	pkgs := map[uint]*models.Package{}
	if len(pkgIDs) > 0 {
		var list []models.Package
		keys := make([]uint, 0, len(pkgIDs))
		for id := range pkgIDs {
			keys = append(keys, id)
		}
		config.DB.Unscoped().Where("id IN ?", keys).Find(&list)
		for i := range list {
			pkgs[list[i].ID] = &list[i]
		}
	}
	devices := deviceMACsFor(ids)

	now := time.Now()
	desired := map[string]radiusUser{}
	for i := range customers {
		c := &customers[i]
		for _, u := range desiredRadiusUsers(c, pkgs[c.PackageID], devices[c.ID], now) {
			desired[u.Username] = u
		}
	}

	var existing []models.RadiusAccount
	config.DB.Where("zone_id = ? AND active = ?", zoneID, true).Find(&existing)
	have := map[string]models.RadiusAccount{}
	for _, a := range existing {
		have[a.Username] = a
	}

	return config.DB.Transaction(func(tx *gorm.DB) error {
		names := make([]string, 0, len(desired))
		for n := range desired {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			u := desired[n]
			if cur, ok := have[n]; ok && cur.AttrHash == u.hash() {
				continue // unchanged
			}
			if err := writeUser(tx, u); err != nil {
				return err
			}
		}
		for n := range have {
			if _, keep := desired[n]; !keep {
				if err := deactivateUser(tx, n); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// SyncCustomer applies one customer's change immediately (used right after a
// payment so a reconnecting device is accepted without waiting for the next
// reconcile). A no-op for zones that aren't in RADIUS mode.
func (s *RadiusService) SyncCustomer(customerID uint) error {
	var c models.Customer
	if err := config.DB.First(&c, customerID).Error; err != nil {
		return err
	}
	var zone models.Zone
	if err := config.DB.Select("id", "auth_mode").First(&zone, c.ZoneID).Error; err != nil || zone.AuthMode != "radius" {
		return nil
	}
	if !RadiusTablesExist() {
		return ErrRadiusNotInstalled
	}
	var pkg models.Package
	var pkgPtr *models.Package
	if err := config.DB.Unscoped().First(&pkg, c.PackageID).Error; err == nil {
		pkgPtr = &pkg
	}
	desired := desiredRadiusUsers(&c, pkgPtr, deviceMACsFor([]uint{c.ID})[c.ID], time.Now())

	return config.DB.Transaction(func(tx *gorm.DB) error {
		keep := map[string]bool{}
		for _, u := range desired {
			keep[u.Username] = true
			if err := writeUser(tx, u); err != nil {
				return err
			}
		}
		var stale []models.RadiusAccount
		tx.Where("customer_id = ? AND active = ?", c.ID, true).Find(&stale)
		for _, a := range stale {
			if !keep[a.Username] {
				if err := deactivateUser(tx, a.Username); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// SyncCustomerAsync is SyncCustomer off the request path; failures are logged.
func (s *RadiusService) SyncCustomerAsync(customerID uint) {
	go func() {
		if err := s.SyncCustomer(customerID); err != nil && !errors.Is(err, ErrRadiusNotInstalled) {
			log.Printf("[radius] sync customer %d: %v", customerID, err)
		}
	}()
}

// ClearZone removes every credential of a zone (used when it leaves RADIUS
// mode), keeping the attribution mapping.
func (s *RadiusService) ClearZone(zoneID uint) error {
	if !RadiusTablesExist() {
		return nil
	}
	var accts []models.RadiusAccount
	config.DB.Where("zone_id = ? AND active = ?", zoneID, true).Find(&accts)
	return config.DB.Transaction(func(tx *gorm.DB) error {
		for _, a := range accts {
			if err := deactivateUser(tx, a.Username); err != nil {
				return err
			}
		}
		return nil
	})
}

// GenerateSecret returns a strong random shared secret for one router.
func GenerateRadiusSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// EnsureNAS registers the zone's router as a RADIUS client (name, source
// address and shared secret). FreeRADIUS loads clients from this table when it
// starts, so a *new* zone needs the RADIUS server reloaded once.
func (s *RadiusService) EnsureNAS(zone *models.Zone) error {
	if !RadiusTablesExist() {
		return ErrRadiusNotInstalled
	}
	if strings.TrimSpace(zone.RouterIP) == "" || zone.RadiusSecret == "" {
		return errors.New("the zone needs a router address and a RADIUS secret first")
	}
	short := fmt.Sprintf("zyra-zone-%d", zone.ID)
	return config.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM nas WHERE shortname = ?", short).Error; err != nil {
			return err
		}
		return tx.Exec("INSERT INTO nas (nasname, shortname, type, secret, description) VALUES (?, ?, 'other', ?, ?)",
			strings.TrimSpace(zone.RouterIP), short, zone.RadiusSecret, "Zyra Net zone "+zone.Name).Error
	})
}

// RemoveNAS drops the zone's client entry.
func (s *RadiusService) RemoveNAS(zoneID uint) {
	if RadiusTablesExist() {
		config.DB.Exec("DELETE FROM nas WHERE shortname = ?", fmt.Sprintf("zyra-zone-%d", zoneID))
	}
}

// Reconcile syncs every zone that is in RADIUS mode.
func (s *RadiusService) Reconcile() {
	var zones []models.Zone
	if err := config.DB.Select("id", "name").Where("auth_mode = ?", "radius").Find(&zones).Error; err != nil || len(zones) == 0 {
		return
	}
	for _, z := range zones {
		if err := s.SyncZone(z.ID); err != nil && !errors.Is(err, ErrRadiusNotInstalled) {
			log.Printf("[radius] reconcile zone %d (%s): %v", z.ID, z.Name, err)
		}
	}
}

// Start runs the reconciler in the background.
func (s *RadiusService) Start() {
	go func() {
		t := time.NewTicker(radiusReconcileEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.Reconcile()
			case <-s.stop:
				return
			}
		}
	}()
	log.Println("[radius] reconciler started (30s). Only zones with auth_mode=radius are touched.")
}

func (s *RadiusService) Stop() { close(s.stop) }

// ReloadServer asks FreeRADIUS to re-read its client list (RADIUS_RELOAD_CMD).
// It reports whether that happened; a failure is logged, never fatal — the
// caller then tells the operator to reload the server by hand.
func (s *RadiusService) ReloadServer() bool {
	fields := strings.Fields(config.Config.RadiusReloadCmd)
	if len(fields) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, fields[0], fields[1:]...).CombinedOutput(); err != nil {
		log.Printf("[radius] reload command failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}
