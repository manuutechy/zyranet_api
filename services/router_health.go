package services

import (
	"encoding/json"
	"log"
	"net"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

const (
	// RouterOfflineAfter: a router that hasn't checked in for this long is
	// treated as offline — we stop taking money for it.
	RouterOfflineAfter = 5 * time.Minute
	// outageCompensateAfter: a gap longer than this between check-ins is an
	// outage, and customers get the lost time back.
	outageCompensateAfter = 10 * time.Minute
	// maxOutageCompensation caps the time given back for one outage (a router
	// unplugged for a month is a closed site, not an outage).
	maxOutageCompensation = 7 * 24 * time.Hour

	// ZoneOfflineMessage is what the customer sees when checkout is refused.
	ZoneOfflineMessage = "This WiFi is offline right now, you have not been charged."
)

// ZoneAcceptingPayments says whether a zone may take a payment right now. It
// is false when the zone's router hasn't checked in for over 5 minutes, so
// nobody pays for WiFi that can't connect them. The reason is customer-facing.
func ZoneAcceptingPayments(zoneID uint) (bool, string) {
	if config.Config.AppEnv == "local" {
		return true, ""
	}
	var zone models.Zone
	if err := config.DB.Select("id", "status", "last_seen_at").First(&zone, zoneID).Error; err != nil {
		return false, "This WiFi isn't set up for payments yet."
	}
	if zone.Status != "" && zone.Status != "active" {
		return false, ZoneOfflineMessage
	}
	if zone.LastSeenAt == nil || time.Since(*zone.LastSeenAt) > RouterOfflineAfter {
		return false, ZoneOfflineMessage
	}
	return true, ""
}

// isWireGuardIP: routers managed over the Zyra Net WireGuard tunnel.
func isWireGuardIP(ip string) bool { return strings.HasPrefix(strings.TrimSpace(ip), "10.200.") }

// usablePublicIP is true for an address worth recording as a router's
// location (not empty, loopback, private or link-local).
func usablePublicIP(ip string) bool {
	p := net.ParseIP(strings.TrimSpace(ip))
	return p != nil && !p.IsLoopback() && !p.IsPrivate() && !p.IsLinkLocalUnicast() && !p.IsUnspecified()
}

// RouterCheckIn is an authenticated request from a zone's router.
type RouterCheckIn struct {
	SourceIP  string // proxy-aware client IP (fiber c.IP())
	Board     string
	Heartbeat bool // heartbeat (vs sync fetch): updates router IP / board
}

// RecordRouterCheckIn notes that the zone's router is alive: last_seen,
// online status, router/public IP (heartbeats only; changes are audited),
// and — exactly once per outage — gives customers back time lost to an
// outage longer than 10 minutes.
func RecordRouterCheckIn(zone *models.Zone, in RouterCheckIn) {
	now := time.Now()
	prev := zone.LastSeenAt

	updates := map[string]interface{}{
		"last_seen_at": now,
		"last_status":  "online",
	}
	var ipChanges []map[string]string
	if in.Heartbeat && usablePublicIP(in.SourceIP) {
		ip := strings.TrimSpace(in.SourceIP)
		if zone.PublicIP != ip {
			updates["public_ip"] = ip
			if zone.PublicIP != "" {
				ipChanges = append(ipChanges, map[string]string{"field": "public_ip", "old": zone.PublicIP, "new": ip})
			}
		}
		// The API dials RouterIP. A WireGuard address is the reachable one, so
		// it is kept; otherwise the router lives at its heartbeat source.
		if !isWireGuardIP(zone.RouterIP) && zone.RouterIP != ip {
			updates["router_ip"] = ip
			ipChanges = append(ipChanges, map[string]string{"field": "router_ip", "old": zone.RouterIP, "new": ip})
		}
	}
	if b := strings.TrimSpace(in.Board); in.Heartbeat && b != "" && len(b) <= 64 &&
		(zone.RouterName == "" || zone.RouterName == "Default Router" || zone.RouterName == "RB951") {
		updates["router_name"] = b
	}

	// Conditional on the last_seen we read, so two concurrent check-ins can't
	// both compensate the same outage.
	q := config.DB.Model(&models.Zone{}).Where("id = ?", zone.ID)
	if prev == nil {
		q = q.Where("last_seen_at IS NULL")
	} else {
		q = q.Where("last_seen_at = ?", *prev)
	}
	res := q.Updates(updates)
	if res.Error != nil || res.RowsAffected == 0 {
		// Someone else recorded a check-in in between; just keep it fresh.
		config.DB.Model(&models.Zone{}).Where("id = ?", zone.ID).
			Updates(map[string]interface{}{"last_seen_at": now, "last_status": "online"})
		return
	}

	for _, ch := range ipChanges {
		old, _ := json.Marshal(map[string]string{ch["field"]: ch["old"]})
		nw, _ := json.Marshal(map[string]string{ch["field"]: ch["new"]})
		o, n := string(old), string(nw)
		config.DB.Create(&models.AuditLog{Action: "router_ip_changed", Model: "zone", ModelID: zone.ID, OldValues: &o, NewValues: &n})
		log.Printf("[router] zone %d %s changed %s -> %s", zone.ID, ch["field"], ch["old"], ch["new"])
	}

	if prev != nil && now.Sub(*prev) > outageCompensateAfter {
		// Close the offline alert the watchdog opened.
		config.DB.Model(&models.ZoneAlert{}).
			Where("zone_id = ? AND type = ? AND resolved_at IS NULL", zone.ID, "offline").
			Update("resolved_at", &now)
		CompensateOutage(zone, *prev, now)
	}
}

// CompensateOutage gives every paying customer of the zone who had time
// running when the outage started the time they lost (capped at 7 days),
// and records it in zone_outages.
func CompensateOutage(zone *models.Zone, start, end time.Time) *models.ZoneOutage {
	lost := end.Sub(start)
	if lost <= 0 {
		return nil
	}
	credit := lost
	if credit > maxOutageCompensation {
		credit = maxOutageCompensation
	}
	cutoff := start.Add(credit) // time lost is counted only up to the cap

	var customers []models.Customer
	config.DB.Preload("Package").
		Where("zone_id = ? AND status IN ? AND expires_at IS NOT NULL AND expires_at > ?", zone.ID, []string{"active", "expired"}, start).
		Find(&customers)

	count := 0
	for _, c := range customers {
		if c.Package == nil || c.Package.IsFreeTier || c.Package.Price <= 0 {
			continue // free trials aren't compensated
		}
		// Time this customer actually lost: from the outage start until their
		// plan would have ended (or the outage ended, whichever is first).
		lostEnd := *c.ExpiresAt
		if lostEnd.After(cutoff) {
			lostEnd = cutoff
		}
		give := lostEnd.Sub(start)
		if give <= 0 {
			continue
		}
		newExpiry := c.ExpiresAt.Add(give)
		if !c.ExpiresAt.After(end) {
			// Their plan ran out during the outage: restart the clock now.
			newExpiry = end.Add(give)
		}
		res := config.DB.Model(&models.Customer{}).
			Where("id = ? AND expires_at = ?", c.ID, *c.ExpiresAt). // don't clobber a concurrent renewal
			Updates(map[string]interface{}{"expires_at": newExpiry, "status": "active"})
		if res.RowsAffected == 1 {
			count++
			ReconcileCustomer(c.ID)
		}
	}

	outage := models.ZoneOutage{
		OrganizationID:       zone.OrganizationID,
		ZoneID:               zone.ID,
		StartedAt:            start,
		EndedAt:              end,
		DurationSeconds:      int64(lost.Seconds()),
		CompensatedSeconds:   int64(credit.Seconds()),
		CustomersCompensated: count,
	}
	config.DB.Create(&outage)
	log.Printf("[router] zone %d back after %s offline: %d customers given up to %s back", zone.ID, lost.Round(time.Second), count, credit.Round(time.Second))
	return &outage
}
