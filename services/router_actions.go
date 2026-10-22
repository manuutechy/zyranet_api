package services

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// RouterPusher is the slice of MikroTikService a paid-up customer depends on.
// It's an interface so tests can stand in for a router.
type RouterPusher interface {
	WhitelistMAC(zone *models.Zone, mac string, pkg *models.Package) error
	PushHotspotUsers(zone *models.Zone, vouchers []models.Voucher) (int, error)
	ReactivateCustomer(zone *models.Zone, customer *models.Customer) error
}

const (
	RouterActionWhitelistMAC = "whitelist_mac"
	RouterActionPushVoucher  = "push_voucher"
	RouterActionReactivate   = "reactivate_customer"
)

// routerActionBackoff is the wait before each retry: the first attempt runs at
// once, then 30s, 1m, 2m, 5m, 10m, 20m and 30m later (~70 minutes in all —
// long enough to ride out a router reboot or a flaky link). After the last one
// the action is marked failed and logged for staff.
var routerActionBackoff = []time.Duration{
	30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
	10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
}

// router returns what router actions are sent through.
func (s *MpesaService) router() RouterPusher {
	if s.Router != nil {
		return s.Router
	}
	if s.MikroTik == nil {
		return nil
	}
	return s.MikroTik
}

// EnqueueRouterAction persists a router change owed to a paying customer and
// tries it straight away in the background. If that attempt fails (router
// offline, NAT, timeout) RunDueRouterActions retries it with backoff — and
// because it's in the database, an API restart doesn't lose it.
func (s *MpesaService) EnqueueRouterAction(a models.PendingRouterAction) {
	if a.ZoneID == 0 {
		return
	}
	a.Status = "pending"
	a.NextAttemptAt = time.Now()
	if err := config.DB.Create(&a).Error; err != nil {
		log.Printf("[router-queue] could not save %s for zone %d: %v", a.Action, a.ZoneID, err)
		return
	}
	go s.runRouterAction(a.ID)
}

// RunDueRouterActions runs every queued router action whose retry time has
// come. Called by the money job every few seconds.
func (s *MpesaService) RunDueRouterActions() {
	// A "running" row older than 5 minutes belongs to a process that died
	// mid-attempt: put it back in the queue.
	config.DB.Model(&models.PendingRouterAction{}).
		Where("status = ? AND updated_at < ?", "running", time.Now().Add(-5*time.Minute)).
		Update("status", "pending")

	var ids []uint
	config.DB.Model(&models.PendingRouterAction{}).
		Where("status = ? AND next_attempt_at <= ?", "pending", time.Now()).
		Order("id ASC").Limit(50).Pluck("id", &ids)
	for _, id := range ids {
		s.runRouterAction(id)
	}
}

// runRouterAction makes one attempt at a queued action. It first claims the
// row (pending → running) so the immediate attempt and the background job
// can never both work on the same action.
func (s *MpesaService) runRouterAction(id uint) {
	claim := config.DB.Model(&models.PendingRouterAction{}).
		Where("id = ? AND status = ? AND next_attempt_at <= ?", id, "pending", time.Now()).
		Updates(map[string]interface{}{"status": "running", "updated_at": time.Now()})
	if claim.Error != nil || claim.RowsAffected == 0 {
		return
	}
	var a models.PendingRouterAction
	if err := config.DB.First(&a, id).Error; err != nil {
		return
	}

	err := s.executeRouterAction(&a)
	attempts := a.Attempts + 1
	if err == nil {
		config.DB.Model(&models.PendingRouterAction{}).Where("id = ?", id).
			Updates(map[string]interface{}{"status": "done", "attempts": attempts, "last_error": ""})
		log.Printf("[router-queue] %s for zone %d done (attempt %d)", a.Action, a.ZoneID, attempts)
		return
	}

	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	updates := map[string]interface{}{"attempts": attempts, "last_error": msg}
	if attempts > len(routerActionBackoff) {
		updates["status"] = "failed"
		log.Printf("[router-queue] GAVE UP on %s for zone %d (payment %v) after %d attempts: %v — customer paid but the router was never updated; check the router", a.Action, a.ZoneID, uintOrNil(a.PaymentID), attempts, err)
	} else {
		updates["status"] = "pending"
		updates["next_attempt_at"] = time.Now().Add(routerActionBackoff[attempts-1])
		log.Printf("[router-queue] %s for zone %d failed (attempt %d), retrying in %s: %v", a.Action, a.ZoneID, attempts, routerActionBackoff[attempts-1], err)
	}
	config.DB.Model(&models.PendingRouterAction{}).Where("id = ?", id).Updates(updates)
}

func (s *MpesaService) executeRouterAction(a *models.PendingRouterAction) error {
	r := s.router()
	if r == nil {
		return fmt.Errorf("no router client configured")
	}
	var zone models.Zone
	if err := config.DB.First(&zone, a.ZoneID).Error; err != nil {
		return fmt.Errorf("zone %d not found: %w", a.ZoneID, err)
	}
	switch a.Action {
	case RouterActionWhitelistMAC:
		if a.PackageID == nil {
			return fmt.Errorf("no package to whitelist with")
		}
		var pkg models.Package
		if err := config.DB.First(&pkg, *a.PackageID).Error; err != nil {
			return fmt.Errorf("package %d not found: %w", *a.PackageID, err)
		}
		mac := strings.ToUpper(strings.ReplaceAll(a.MacAddress, "-", ":"))
		return r.WhitelistMAC(&zone, mac, &pkg)
	case RouterActionPushVoucher:
		if a.VoucherID == nil {
			return fmt.Errorf("no voucher to push")
		}
		var v models.Voucher
		if err := config.DB.First(&v, *a.VoucherID).Error; err != nil {
			return fmt.Errorf("voucher %d not found: %w", *a.VoucherID, err)
		}
		_, err := r.PushHotspotUsers(&zone, []models.Voucher{v})
		return err
	case RouterActionReactivate:
		if a.CustomerID == nil {
			return fmt.Errorf("no customer to reactivate")
		}
		var c models.Customer
		if err := config.DB.Preload("Package").First(&c, *a.CustomerID).Error; err != nil {
			return fmt.Errorf("customer %d not found: %w", *a.CustomerID, err)
		}
		return r.ReactivateCustomer(&zone, &c)
	}
	return fmt.Errorf("unknown router action %q", a.Action)
}

func uintOrNil(p *uint) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
