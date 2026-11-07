package services

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// Fair use for RADIUS zones.
//
// A plan with a data cap (data_limit_mb) is counted per "plan window": one
// window per purchased period, counted back from the customer's expiry. A
// customer who bought three days in a row gets the cap once per day, not
// three days' worth up front.
//
// Usage comes from FreeRADIUS accounting (radacct), which the router updates
// every 5 minutes (Acct-Interim-Interval), so the cap is enforced to within
// about 5 minutes of traffic.
//
//   - Plan WITH fair_use_kbps: past the cap the customer is slowed to that
//     speed (no disconnect). The live session is slowed with a RADIUS CoA
//     message; any new login gets the slow speed straight away.
//   - Plan WITHOUT fair_use_kbps: the router is told the bytes left at login
//     (Mikrotik-Total-Limit) and ends the session exactly at the cap; past it
//     the customer has no RADIUS account, so they are refused until they buy.

// usageUnknown means "no accounting data available" (radacct missing): the
// full cap is applied per session, which is the old behaviour.
const usageUnknown int64 = -1

// stepBack returns the start of the plan period that ends at t.
func stepBack(t time.Time, pkg *models.Package) time.Time {
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		return t.Add(-time.Duration(*pkg.TimeLimitMinutes) * time.Minute)
	}
	switch pkg.BillingCycle {
	case "hourly":
		return t.Add(-time.Hour)
	case "daily":
		return t.Add(-24 * time.Hour)
	case "weekly":
		return t.Add(-7 * 24 * time.Hour)
	default: // monthly, as utils.CalculateExpiry treats anything else
		return t.AddDate(0, -1, 0)
	}
}

// usageWindowStart is the start of the plan window that contains now: step
// back one period at a time from the expiry until we pass now.
func usageWindowStart(expires time.Time, pkg *models.Package, now time.Time) time.Time {
	start := expires
	for i := 0; i < 1000 && start.After(now); i++ {
		start = stepBack(start, pkg)
	}
	return start
}

func capBytes(pkg *models.Package) int64 {
	if pkg == nil || pkg.DataLimitMB == nil || *pkg.DataLimitMB <= 0 {
		return 0
	}
	return int64(*pkg.DataLimitMB) * 1024 * 1024
}

func fairUseKbps(pkg *models.Package) int {
	if pkg == nil || pkg.FairUseKbps == nil || *pkg.FairUseKbps <= 0 {
		return 0
	}
	return *pkg.FairUseKbps
}

// radacctExists is cached alongside the other RADIUS table check.
func radacctExists() bool {
	return RadiusTablesExist() && config.DB.Migrator().HasTable("radacct")
}

// capUsage returns bytes used in the current plan window for every capped,
// paid-up customer in the list. Customers without a cap are left out (they
// don't need it). Missing accounting ⇒ every capped customer gets usageUnknown.
func capUsage(customers []models.Customer, pkgs map[uint]*models.Package, now time.Time) map[uint]int64 {
	out := map[uint]int64{}
	starts := map[uint]time.Time{}
	var ids []uint
	var earliest time.Time
	for i := range customers {
		c := &customers[i]
		pkg := pkgs[c.PackageID]
		if capBytes(pkg) == 0 || c.ExpiresAt == nil || !c.ExpiresAt.After(now) {
			continue
		}
		s := usageWindowStart(*c.ExpiresAt, pkg, now)
		starts[c.ID] = s
		ids = append(ids, c.ID)
		if earliest.IsZero() || s.Before(earliest) {
			earliest = s
		}
	}
	if len(ids) == 0 {
		return out
	}
	if !radacctExists() {
		for _, id := range ids {
			out[id] = usageUnknown
		}
		return out
	}
	var rows []struct {
		CustomerID uint
		Start      time.Time
		Bytes      int64
	}
	// Every username the customer ever had (MACs change), sessions that began
	// inside the window. Input+output octets incl. the 4 GB wrap counters.
	err := config.DB.Raw(`SELECT ra.customer_id AS customer_id, r.acctstarttime AS start,
		  COALESCE(r.acctinputoctets,0) + COALESCE(r.acctoutputoctets,0) AS bytes
		FROM radacct r JOIN radius_accounts ra ON ra.username = r.username
		WHERE ra.customer_id IN ? AND r.acctstarttime >= ?`, ids, earliest.UTC()).Scan(&rows).Error
	if err != nil {
		log.Printf("[radius] fair use: reading usage: %v", err)
		for _, id := range ids {
			out[id] = usageUnknown
		}
		return out
	}
	for _, id := range ids {
		out[id] = 0
	}
	for _, r := range rows {
		if s, ok := starts[r.CustomerID]; ok && !r.Start.Before(s) {
			out[r.CustomerID] += r.Bytes
		}
	}
	return out
}

// radiusReply builds what the router is told for a paid-up customer, given
// what they have used. ok=false means "refuse": hard cap reached.
func radiusReply(pkg *models.Package, used int64) (reply []radiusAttr, fairUse, ok bool) {
	rate := formatRateLimitWithBurst(pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
	cap := capBytes(pkg)
	slow := fairUseKbps(pkg)
	interim := radiusAttr{"Acct-Interim-Interval", ":=", radiusInterim}

	if cap == 0 { // no cap: full speed with burst
		return []radiusAttr{{"Mikrotik-Rate-Limit", ":=", rate}, interim}, false, true
	}
	if slow > 0 {
		if used >= cap {
			return []radiusAttr{{"Mikrotik-Rate-Limit", ":=", fmt.Sprintf("%dk/%dk", slow, slow)}, interim}, true, true
		}
		return []radiusAttr{{"Mikrotik-Rate-Limit", ":=", rate}, interim}, false, true
	}
	// Hard cap: tell the router how many bytes are left this window.
	left := cap
	if used > 0 {
		left = cap - used
	}
	if left <= 0 {
		return nil, false, false
	}
	return []radiusAttr{
		{"Mikrotik-Rate-Limit", ":=", rate},
		{"Mikrotik-Total-Limit", ":=", fmt.Sprintf("%d", left&0xFFFFFFFF)},
		{"Mikrotik-Total-Limit-Gigawords", ":=", fmt.Sprintf("%d", left>>32)},
		interim,
	}, false, true
}

// slowLiveSession sends a RADIUS CoA to the zone's router so a customer who
// just crossed their cap is slowed without being disconnected. Best effort:
// if it fails, their next login still gets the slow speed.
func slowLiveSession(zoneID uint, username, rate string) {
	if _, err := exec.LookPath("radclient"); err != nil {
		return
	}
	var zone models.Zone
	if err := config.DB.Select("id", "router_ip", "radius_secret").First(&zone, zoneID).Error; err != nil {
		return
	}
	ip := strings.TrimSpace(zone.RouterIP)
	if ip == "" || zone.RadiusSecret == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "radclient", "-r", "2", "-t", "3", ip+":3799", "coa", zone.RadiusSecret)
	cmd.Stdin = strings.NewReader(fmt.Sprintf("User-Name = \"%s\"\nMikrotik-Rate-Limit = \"%s\"\n", username, rate))
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[radius] fair use: CoA to %s for %s failed: %v (%s)", ip, username, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("[radius] fair use: %s slowed to %s (zone %d)", username, rate, zoneID)
}
