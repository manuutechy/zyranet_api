package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os/exec"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Home accounts: houses on fibre (PPPoE customers). They pay for time up
// front — by Paybill (account = customer number) or an STK prompt from the
// public renew page. Money always lands on the credit balance first, then as
// many whole periods of their plan as the credit covers are bought from it;
// the rest stays as credit. When time runs out the expiry sweep buys the next
// period from credit if it can, and only switches the home off when it can't.

// HomeMaxPeriods caps how many periods one payment (or one sweep) buys.
const HomeMaxPeriods = 366

// IsHome reports whether a customer is a home (PPPoE) account.
func IsHome(c *models.Customer) bool { return c != nil && c.Type == "pppoe" }

// toCents converts a KES amount stored as decimal(10,2) to whole cents.
func toCents(v float64) int64 { return int64(math.Round(v * 100)) }

// HomePeriodsCovered is how many whole periods at price the balance pays
// for, capped at max. Worked in cents so 90.00 / 30.00 is exactly 3.
func HomePeriodsCovered(balance, price float64, max int) int {
	p := toCents(price)
	b := toCents(balance)
	if p <= 0 || b < p {
		return 0
	}
	n := b / p
	if max > 0 && n > int64(max) {
		n = int64(max)
	}
	return int(n)
}

// HomeExtendExpiry adds n periods of the plan to the paid-until time
// (from now if it already ran out), the same way a single purchase does.
func HomeExtendExpiry(pkg *models.Package, current *time.Time, n int) time.Time {
	var t time.Time
	base := current
	for i := 0; i < n; i++ {
		t = utils.CalculateExpiry(pkg.BillingCycle, base, pkg.TimeLimitMinutes)
		base = &t
	}
	return t
}

// HomePeriod describes one period of a plan in words the portal can label:
// unit is minute|hour|day|week|month, count how many of them.
func HomePeriod(pkg *models.Package) (unit string, count int) {
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		m := *pkg.TimeLimitMinutes
		switch {
		case m%(7*24*60) == 0:
			return "week", m / (7 * 24 * 60)
		case m%(24*60) == 0:
			return "day", m / (24 * 60)
		case m%60 == 0:
			return "hour", m / 60
		default:
			return "minute", m
		}
	}
	switch pkg.BillingCycle {
	case "hourly":
		return "hour", 1
	case "daily":
		return "day", 1
	case "weekly":
		return "week", 1
	default:
		return "month", 1
	}
}

// HomeRenewResult is what RenewHomeFromCredit bought.
type HomeRenewResult struct {
	Periods   int
	Debited   float64
	ExpiresAt time.Time
	PackageID uint
}

// lockRow adds SELECT … FOR UPDATE on MySQL (SQLite, used by tests, locks
// the whole database for a write transaction anyway and has no such clause).
func lockRow(tx *gorm.DB) *gorm.DB {
	if tx.Dialector != nil && tx.Dialector.Name() == "mysql" {
		return tx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return tx
}

// RenewHomeFromCredit buys as many whole periods of a home customer's plan
// as their credit covers (up to max) and extends their paid-until time. It
// must run inside a transaction (tx): the customer row is locked, the debit
// is checked by the database (AdjustCredit) and logged, and the status goes
// back to active. Periods == 0 means nothing was bought (not a home, no plan
// price, or not enough credit) and nothing changed.
func RenewHomeFromCredit(tx *gorm.DB, customerID uint, max int, note string) (HomeRenewResult, error) {
	var out HomeRenewResult
	var c models.Customer
	if err := lockRow(tx).Select("id", "type", "package_id", "expires_at", "credit_balance").
		First(&c, customerID).Error; err != nil {
		return out, err
	}
	if !IsHome(&c) {
		return out, nil
	}
	var pkg models.Package
	if err := tx.Unscoped().First(&pkg, c.PackageID).Error; err != nil {
		return out, nil // no plan: nothing to buy
	}
	if max <= 0 || max > HomeMaxPeriods {
		max = HomeMaxPeriods
	}
	n := HomePeriodsCovered(c.CreditBalance, pkg.Price, max)
	if n == 0 {
		return out, nil
	}
	debit := float64(int64(n)*toCents(pkg.Price)) / 100
	ok, err := AdjustCredit(tx, c.ID, -debit)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, nil // balance changed under us: buy nothing
	}
	newExpiry := HomeExtendExpiry(&pkg, c.ExpiresAt, n)
	if err := tx.Model(&models.Customer{}).Where("id = ?", c.ID).Updates(map[string]interface{}{
		"expires_at": newExpiry,
		"status":     "active",
	}).Error; err != nil {
		return out, err
	}
	unit, count := HomePeriod(&pkg)
	logNote := fmt.Sprintf("%s: %d × %s (%d %s each) at KES %.2f — paid until %s EAT",
		note, n, pkg.Name, count, unit, pkg.Price, utils.Kenya(newExpiry).Format("2 Jan 2006 15:04"))
	if err := tx.Create(&models.CreditLog{CustomerID: c.ID, Amount: debit, Type: "debit", Note: &logNote}).Error; err != nil {
		return out, err
	}
	out = HomeRenewResult{Periods: n, Debited: debit, ExpiresAt: newExpiry, PackageID: pkg.ID}
	return out, nil
}

// ---------- switching a home on and off at the router ----------

// HomeRouter is the slice of MikroTikService a home switch-off needs (API
// mode). An interface so tests never dial a router.
type HomeRouter interface {
	DecommissionPPPoECustomer(zone *models.Zone, username string) error
}

// homeRadiusSync rewrites one customer's RADIUS rows (no-op outside RADIUS
// zones). A variable so tests can stand in for it.
var homeRadiusSync = func(customerID uint) error {
	err := NewRadiusService().SyncCustomer(customerID)
	if errors.Is(err, ErrRadiusNotInstalled) {
		return nil
	}
	return err
}

// homeRadiusDisconnect ends a live PPPoE session in a RADIUS zone with a
// RADIUS Disconnect-Request (RFC 5176) sent by radclient to the router —
// the same tool radius_fairuse.go uses for CoA. A variable for tests.
var homeRadiusDisconnect = radiusDisconnect

func radiusDisconnect(zone *models.Zone, username string) error {
	if config.Config.AppEnv == "local" {
		return nil // never reach a real router from a laptop
	}
	if _, err := exec.LookPath("radclient"); err != nil {
		return nil // RADIUS tools not installed: Session-Timeout still ends it at expiry
	}
	ip := strings.TrimSpace(zone.RouterIP)
	if ip == "" || zone.RadiusSecret == "" || username == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "radclient", "-r", "2", "-t", "3", ip+":3799", "disconnect", zone.RadiusSecret)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("User-Name = \"%s\"\n", strings.ReplaceAll(username, "\"", "")))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("disconnect %s at %s: %v (%s)", username, ip, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HomeUsername is the PPPoE login of a home (falls back to the account number,
// as the expiry sweep always has).
func HomeUsername(c *models.Customer) string {
	if c.PPPoEUsername != nil && strings.TrimSpace(*c.PPPoEUsername) != "" {
		return strings.TrimSpace(*c.PPPoEUsername)
	}
	return c.AccountNumber
}

// HomeSwitchOff cuts a home off at the router, in either auth mode:
//   - RADIUS: the RADIUS rows are rewritten (an unpaid/suspended customer has
//     none, so the router's next login is refused) and the live session is
//     ended with a Disconnect-Request.
//   - API: the PPP secret is disabled and the live session removed.
//
// The customer's status must already be expired/suspended in the database.
// zone must be the customer's zone (with router_ip, auth_mode, radius_secret).
func HomeSwitchOff(r HomeRouter, c *models.Customer, zone *models.Zone) error {
	if zone == nil {
		return nil
	}
	username := HomeUsername(c)
	if zone.AuthMode == "radius" {
		if err := homeRadiusSync(c.ID); err != nil {
			log.Printf("[homes] switch off %d: radius sync: %v", c.ID, err)
		}
		return homeRadiusDisconnect(zone, username)
	}
	if r == nil || strings.TrimSpace(zone.RouterIP) == "" || username == "" {
		return nil
	}
	return r.DecommissionPPPoECustomer(zone, username)
}

// HomeSwitchOn lets a paid-up home back in, in either auth mode: RADIUS
// rows are rewritten at once; in API mode the PPP secret is re-enabled
// through the persisted, retried router queue.
func (s *MpesaService) HomeSwitchOn(customerID, zoneID uint, paymentID *uint) {
	var zone models.Zone
	if err := config.DB.Select("id", "auth_mode").First(&zone, zoneID).Error; err != nil {
		return
	}
	if zone.AuthMode == "radius" {
		go func() {
			if err := homeRadiusSync(customerID); err != nil {
				log.Printf("[homes] switch on %d: radius sync: %v", customerID, err)
			}
		}()
		return
	}
	cid := customerID
	s.EnqueueRouterAction(models.PendingRouterAction{
		ZoneID: zoneID, PaymentID: paymentID, Action: RouterActionReactivate, CustomerID: &cid,
	})
}

// HomeAutoRenewAtExpiry is called by the expiry sweep for a home whose paid
// time ran out: it buys the next period(s) from credit if it can. Returns
// true when the home was renewed (so it must stay on). In RADIUS zones the
// rows get the new expiry straight away.
func HomeAutoRenewAtExpiry(customerID uint) bool {
	var res HomeRenewResult
	err := config.DB.Transaction(func(tx *gorm.DB) error {
		// Re-check under the lock that it is still an active home whose time
		// ran out — a payment may have renewed it a moment ago.
		var c models.Customer
		if err := lockRow(tx).Select("id", "status", "expires_at").First(&c, customerID).Error; err != nil {
			return err
		}
		if c.Status != "active" || c.ExpiresAt == nil || c.ExpiresAt.After(time.Now()) {
			return nil
		}
		var err error
		// One period at a time: the credit keeps paying day by day.
		res, err = RenewHomeFromCredit(tx, customerID, 1, "Auto-renewed from credit at expiry")
		return err
	})
	if err != nil {
		log.Printf("[homes] auto-renew %d: %v", customerID, err)
		return false
	}
	if res.Periods > 0 {
		if err := homeRadiusSync(customerID); err != nil {
			log.Printf("[homes] auto-renew %d: radius sync: %v", customerID, err)
		}
		return true
	}
	return false
}

// processHomeRenewal completes an STK payment made on the public renew page:
// the money goes on credit, then whole periods are bought from it. handled is
// false when the payment is not a home renewal (normal flow continues).
// Called once per payment (after ProcessPaymentSuccess claimed it).
func (s *MpesaService) processHomeRenewal(payment *models.Payment, receiptNumber string) (handled bool, err error) {
	var hr models.HomeRenewal
	if err := config.DB.Where("payment_id = ?", payment.ID).First(&hr).Error; err != nil {
		return false, nil
	}
	var c models.Customer
	if err := config.DB.First(&c, hr.CustomerID).Error; err != nil || !IsHome(&c) {
		log.Printf("[homes] renewal payment %d: customer %d not found — money NOT credited, check by hand", payment.ID, hr.CustomerID)
		return true, nil
	}
	var res HomeRenewResult
	err = config.DB.Transaction(func(tx *gorm.DB) error {
		if _, err := AdjustCredit(tx, c.ID, payment.Amount); err != nil {
			return err
		}
		note := fmt.Sprintf("M-Pesa home renewal (Receipt: %s)", receiptNumber)
		if err := tx.Create(&models.CreditLog{CustomerID: c.ID, Amount: payment.Amount, Type: "credit", Note: &note}).Error; err != nil {
			return err
		}
		if c.Status == "suspended" {
			return nil // switched off by staff: keep the money as credit
		}
		var err error
		res, err = RenewHomeFromCredit(tx, c.ID, HomeMaxPeriods, "Home renewal "+receiptNumber)
		return err
	})
	if err != nil {
		log.Printf("[homes] RENEWAL NOT CREDITED for payment %d (receipt %s, KES %.2f): %v — credit it manually", payment.ID, receiptNumber, payment.Amount, err)
		return true, err
	}
	if res.Periods > 0 {
		s.HomeSwitchOn(c.ID, c.ZoneID, &payment.ID)
	}
	log.Printf("[homes] payment %d: KES %.2f for customer %d, %d period(s) bought", payment.ID, payment.Amount, c.ID, res.Periods)
	return true, nil
}
