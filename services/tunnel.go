package services

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Automatic router tunnel. Every provisioned router gets, with no hand work:
//
//  1. a tunnel address (10.200.0.x) and a tunnel-only API login, chosen on
//     its first sync (EnsureZoneTunnel);
//  2. the WireGuard interface and the server as its peer, from the sync
//     script (writeTunnel) — the router dials out, so NAT and a changing
//     public IP don't matter;
//  3. its public key added to the hub the moment it reports it
//     (RegisterTunnelPeer → the peers file a root service applies), and the
//     API pointed at the tunnel address from then on.
//
// The key is only accepted from a request carrying the zone's own provision
// token, and only ever for the address the server chose for that zone.

const (
	tunnelFirstHost = 10 // 10.200.0.2–9 stay free for hand-set routers
	tunnelLastHost  = 254
	tunnelAPIUser   = "zyranet-api"
	tunnelAckWait   = 4 * time.Second
)

// The API never touches WireGuard itself (it runs without privileges). It
// writes the full list of router keys — one "publickey address" per line — to
// WG_PEER_FILE; a root service watching that file validates every line,
// applies it to the hub and writes the file's SHA-256 to WG_PEER_FILE.applied.
// tunnelPush waits for that receipt, so "added to the hub" is only recorded
// when it really was.
var (
	tunnelMu sync.Mutex
	// tunnelPush hands the list to the hub; swapped in tests.
	tunnelPush = pushTunnelPeers
	// tunnelHubHere: this server runs the hub (the peers file is configured).
	tunnelHubHere = func() bool { return tunnelPeerFile() != "" }
)

func tunnelPeerFile() string { return strings.TrimSpace(os.Getenv("WG_PEER_FILE")) }

func pushTunnelPeers(lines []string) error {
	path := tunnelPeerFile()
	if path == "" {
		return fmt.Errorf("WG_PEER_FILE is not set")
	}
	body := strings.Join(lines, "\n")
	if body != "" {
		body += "\n"
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	applied := func() bool {
		b, err := os.ReadFile(path + ".applied")
		return err == nil && strings.TrimSpace(string(b)) == want
	}
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body && applied() {
		return nil // the hub already has exactly this
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		return err
	}
	deadline := time.Now().Add(tunnelAckWait)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if applied() {
			return nil
		}
	}
	return fmt.Errorf("the hub did not confirm the new peer list within %s", tunnelAckWait)
}

// tunnelPeerLines is every router key the hub should hold, from the database.
func tunnelPeerLines(db *gorm.DB) []string {
	var zones []models.Zone
	db.Where("tunnel_pub_key IS NOT NULL AND tunnel_pub_key <> '' AND tunnel_ip IS NOT NULL").Order("id").Find(&zones)
	var lines []string
	seenIP := map[string]bool{}
	for i := range zones {
		pub, ip := strVal(zones[i].TunnelPubKey), strVal(zones[i].TunnelIP)
		if !wgKeyRe.MatchString(pub) || !tunnelIPRe.MatchString(ip) || seenIP[ip] {
			continue
		}
		seenIP[ip] = true
		lines = append(lines, pub+" "+ip)
	}
	return lines
}

// EnsureZoneTunnel gives a provisioned zone its tunnel address and API login
// if it has none. Returns true when the zone was changed (it is updated in
// place, so the sync script generated next already carries the tunnel).
func EnsureZoneTunnel(db *gorm.DB, zone *models.Zone) bool {
	if zone.ProvisionToken == "" {
		return false
	}
	upd := map[string]interface{}{}
	if zone.TunnelIP == nil || !tunnelIPRe.MatchString(*zone.TunnelIP) {
		tunnelMu.Lock()
		defer tunnelMu.Unlock()
		var taken []string
		db.Model(&models.Zone{}).Where("tunnel_ip IS NOT NULL AND tunnel_ip <> ''").Pluck("tunnel_ip", &taken)
		used := map[string]bool{}
		for _, t := range taken {
			used[t] = true
		}
		ip := ""
		for h := tunnelFirstHost; h <= tunnelLastHost; h++ {
			if c := fmt.Sprintf("10.200.0.%d", h); !used[c] {
				ip = c
				break
			}
		}
		if ip == "" {
			log.Printf("[tunnel] zone %d: no free tunnel address left", zone.ID)
			return false
		}
		zone.TunnelIP = &ip
		upd["tunnel_ip"] = ip
	}
	// A login the server made for itself, usable only from the hub's tunnel
	// address. An owner-entered router login is left alone.
	if strVal(zone.RouterUsername) == "" || (strVal(zone.RouterUsername) == tunnelAPIUser && !credPassRe.MatchString(strVal(zone.RouterPassword))) {
		pw, err := randomFrom("ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789", 28)
		if err != nil {
			return len(upd) > 0 && db.Model(&models.Zone{}).Where("id = ?", zone.ID).Updates(upd).Error == nil
		}
		user := tunnelAPIUser
		zone.RouterUsername, zone.RouterPassword = &user, &pw
		upd["router_username"], upd["router_password"] = user, pw
	}
	if len(upd) == 0 {
		return false
	}
	if err := db.Model(&models.Zone{}).Where("id = ?", zone.ID).Updates(upd).Error; err != nil {
		log.Printf("[tunnel] zone %d: could not save tunnel settings: %v", zone.ID, err)
		return false
	}
	log.Printf("[tunnel] zone %d (%s): tunnel address %s assigned", zone.ID, zone.Name, strVal(zone.TunnelIP))
	return true
}

// RegisterTunnelPeer puts the router's reported key on the hub (once per key)
// and points the API at the tunnel address. The key is saved first, so if
// the hub doesn't confirm, the router's next report (a minute later) retries.
func RegisterTunnelPeer(db *gorm.DB, zone *models.Zone, pub string) error {
	if zone.TunnelIP == nil || !tunnelIPRe.MatchString(*zone.TunnelIP) || !wgKeyRe.MatchString(pub) {
		return fmt.Errorf("zone has no tunnel address or the key is malformed")
	}
	if strVal(zone.TunnelPubKey) == pub && zone.TunnelPeerAt != nil {
		return nil // already on the hub
	}
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if err := db.Model(&models.Zone{}).Where("id = ?", zone.ID).
		Updates(map[string]interface{}{"tunnel_pub_key": pub, "tunnel_peer_at": nil}).Error; err != nil {
		return err
	}
	zone.TunnelPubKey, zone.TunnelPeerAt = &pub, nil
	if !tunnelHubHere() {
		return nil // not this server's job (dev box): the key is remembered, nothing else
	}
	if err := tunnelPush(tunnelPeerLines(db)); err != nil {
		log.Printf("[tunnel] zone %d (%s): hub not updated: %v", zone.ID, zone.Name, err)
		return err
	}
	now := time.Now()
	upd := map[string]interface{}{"tunnel_peer_at": now}
	if !isWireGuardIP(zone.RouterIP) {
		upd["router_ip"] = *zone.TunnelIP // the API dials RouterIP; the tunnel is the reachable one
	}
	if err := db.Model(&models.Zone{}).Where("id = ?", zone.ID).Updates(upd).Error; err != nil {
		return err
	}
	zone.TunnelPeerAt = &now
	log.Printf("[tunnel] zone %d (%s): peer %s… is on the hub at %s", zone.ID, zone.Name, pub[:8], *zone.TunnelIP)
	return nil
}

// ReconcileTunnelPeers hands the hub the full list again (startup). Idempotent.
func ReconcileTunnelPeers(db *gorm.DB) {
	if !tunnelHubHere() {
		return
	}
	lines := tunnelPeerLines(db)
	if len(lines) == 0 {
		return
	}
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if err := tunnelPush(lines); err != nil {
		log.Printf("[tunnel] could not restore %d router peer(s) on the hub: %v", len(lines), err)
		return
	}
	log.Printf("[tunnel] %d router peer(s) confirmed on the hub", len(lines))
}
