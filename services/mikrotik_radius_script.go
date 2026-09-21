package services

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/models"
)

// These generate RouterOS scripts that are DOWNLOADED and reviewed by a person,
// never pushed automatically. They change how a live router authenticates and
// shapes traffic, so they are deliberately separate from the provisioning
// script and written so a failing line can't abort the rest: each command is
// wrapped in :do { … } on-error={ … }.
//
// They follow RouterOS 7 syntax from MikroTik's documentation and have not been
// run against hardware from here — apply to ONE router first and use the
// rollback script if anything looks wrong.

const radiusProfileName = "hsp-zyranet" // the hotspot profile the provisioning script creates

// GenerateRadiusScript switches a zone's router to RADIUS authentication and
// accounting. Existing local hotspot users keep working (RouterOS checks its
// own user list before asking RADIUS), so the switch can be made and reversed
// without disconnecting anyone.
func GenerateRadiusScript(zone *models.Zone, serverAddr string) (string, error) {
	if zone.RadiusSecret == "" {
		return "", errors.New("this zone has no RADIUS secret yet — enable RADIUS for it first")
	}
	if strings.ContainsAny(zone.RadiusSecret, "\"\\$ \n") {
		return "", errors.New("the RADIUS secret contains characters that are unsafe in a script")
	}
	if net.ParseIP(strings.TrimSpace(serverAddr)) == nil {
		return "", fmt.Errorf("RADIUS server address %q is not a valid IP", serverAddr)
	}
	serverAddr = strings.TrimSpace(serverAddr)

	// Send from the tunnel address so the server sees the source IP it has on file.
	src := ""
	if ip := strings.TrimSpace(zone.RouterIP); strings.HasPrefix(ip, "10.200.") && net.ParseIP(ip) != nil {
		src = " src-address=" + ip
	}

	var b strings.Builder
	w := func(format string, a ...interface{}) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# ============================================================")
	w("# Zyra Net — switch zone %q (#%d) to RADIUS", zone.Name, zone.ID)
	w("# Generated %s. APPLY ON ONE ROUTER FIRST.", time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	w("#")
	w("# What this does: the router asks the RADIUS server whether a device may")
	w("# connect (and for how long, at what speed) and reports usage back.")
	w("# Local hotspot users are still checked first, so nobody is disconnected.")
	w("# To undo: run the rollback script for this zone.")
	w("# ============================================================")
	w(":log info \"zyra: applying RADIUS configuration\"")
	w("")
	w("# 1. The RADIUS server (reached over the WireGuard tunnel)")
	w(":do { /radius remove [find comment=\"zyra-radius\"] } on-error={}")
	w(":do { /radius add address=%s secret=\"%s\" service=hotspot,ppp authentication-port=1812 accounting-port=1813 timeout=3s%s comment=\"zyra-radius\" } on-error={ :log error \"zyra: could not add the RADIUS server\" }", serverAddr, zone.RadiusSecret, src)
	w("")
	w("# 2. Accept disconnect requests from the server")
	w(":do { /radius incoming set accept=yes port=3799 } on-error={ :log warning \"zyra: could not enable RADIUS incoming\" }")
	w("")
	w("# 3. Hotspot: authenticate by MAC address via RADIUS, and send usage records")
	w(":do { /ip hotspot profile set [find name=\"%s\"] use-radius=yes radius-accounting=yes radius-interim-update=5m login-by=mac,http-pap,http-chap mac-auth-mode=mac-as-username } on-error={ :log error \"zyra: could not update the hotspot profile\" }", radiusProfileName)
	w("")
	w("# 4. PPPoE: authenticate and account via RADIUS")
	w(":do { /ppp aaa set use-radius=yes accounting=yes interim-update=5m } on-error={ :log warning \"zyra: could not update PPP AAA\" }")
	w("")
	w(":log info \"zyra: RADIUS configuration applied\"")
	return b.String(), nil
}

// GenerateRadiusRollbackScript puts a zone's router back to the API-driven
// model. Local hotspot users, which were never removed, keep working.
func GenerateRadiusRollbackScript(zone *models.Zone) string {
	var b strings.Builder
	w := func(format string, a ...interface{}) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# Zyra Net — ROLLBACK RADIUS for zone %q (#%d)", zone.Name, zone.ID)
	w("# Returns the router to API-driven authentication.")
	w(":log info \"zyra: rolling back RADIUS\"")
	w(":do { /ip hotspot profile set [find name=\"%s\"] use-radius=no radius-accounting=no } on-error={}", radiusProfileName)
	w(":do { /ppp aaa set use-radius=no accounting=no } on-error={}")
	w(":do { /radius remove [find comment=\"zyra-radius\"] } on-error={}")
	w(":log info \"zyra: RADIUS rolled back\"")
	return b.String()
}

// hotspotNetwork returns the zone's hotspot subnet ("10.5.50.0/24") from its
// gateway address, falling back to the provisioning script's default.
func hotspotNetwork(zone *models.Zone) string {
	addr := strings.TrimSpace(zone.HotspotAddress)
	if addr == "" {
		addr = "10.5.50.1/24"
	}
	if _, n, err := net.ParseCIDR(addr); err == nil {
		return n.String()
	}
	return "10.5.50.0/24"
}

// tunedKbps is the share of the real uplink the router will hand out: a little
// under 100% so the queue builds on the router (where it can be managed fairly)
// rather than in the ISP's equipment (where it can't).
func tunedKbps(mbps int) int { return mbps * 1000 * 92 / 100 }

// GenerateQueueTuningScript builds the fair-queue configuration for a zone's
// hotspot: a total cap just under the real uplink, every customer's queue
// hanging under it, and a fair, low-latency queue algorithm on each.
func GenerateQueueTuningScript(zone *models.Zone, downMbps, upMbps int) (string, error) {
	if downMbps < 1 || downMbps > 10000 || upMbps < 1 || upMbps > 10000 {
		return "", errors.New("enter your real download and upload speed in Mbps (1–10000)")
	}
	down, up := tunedKbps(downMbps), tunedKbps(upMbps)
	network := hotspotNetwork(zone)

	var b strings.Builder
	w := func(format string, a ...interface{}) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# ============================================================")
	w("# Zyra Net — fair-queue tuning for zone %q (#%d)", zone.Name, zone.ID)
	w("# Generated %s. APPLY ON ONE ROUTER FIRST, in a quiet hour.", time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	w("#")
	w("# Your real uplink: %d Mbps down / %d Mbps up.", downMbps, upMbps)
	w("# The router will hand out 92%% of it (%dk / %dk) so that when the line is", down, up)
	w("# full, the waiting happens HERE, where every customer is treated fairly,")
	w("# instead of inside your ISP's equipment, where one heavy user delays everyone.")
	w("#")
	w("# Test before/after at https://www.waveform.com/tools/bufferbloat while")
	w("# someone is downloading. To undo, run the rollback commands at the bottom.")
	w("# ============================================================")
	w(":log info \"zyra: applying fair-queue tuning\"")
	w("")
	w("# 1. A fair, low-latency queue algorithm (falls back to SFQ on old RouterOS)")
	w(":do { /queue type remove [find name=\"zyra-fq\"] } on-error={}")
	w(":do { /queue type add name=zyra-fq kind=fq-codel fq-codel-limit=1024 fq-codel-flows=1024 fq-codel-target=5ms fq-codel-interval=100ms } on-error={ :do { /queue type add name=zyra-fq kind=sfq sfq-perturb=5 } on-error={ :log error \"zyra: could not create a queue type\" } }")
	w("")
	w("# 2. One total cap for the whole hotspot, just under the real uplink")
	w(":do { /queue simple remove [find name=\"zyra-total\"] } on-error={}")
	w(":do { /queue simple add name=zyra-total target=%s max-limit=%dk/%dk queue=zyra-fq/zyra-fq comment=\"zyra-fair-queue\" } on-error={ :log error \"zyra: could not create the total queue\" }", network, up, down)
	w("")
	w("# 3. Every customer's own queue hangs beneath the total, ahead of it")
	w(":do { /ip hotspot user profile set [find] parent-queue=zyra-total insert-queue-before=first } on-error={ :log warning \"zyra: could not attach hotspot user queues\" }")
	w("")
	w("# 4. Use the same fair algorithm for each customer's queue")
	w(":do { /queue type set [find name=\"hotspot-default\"] kind=fq-codel } on-error={ :log warning \"zyra: could not change the per-user queue type (fine — steps 2-3 still help)\" }")
	w("")
	w(":log info \"zyra: fair-queue tuning applied\"")
	w("")
	w("# ------------------------------------------------------------")
	w("# ROLLBACK (paste to undo)")
	w("# ------------------------------------------------------------")
	w("# :do { /queue simple remove [find name=\"zyra-total\"] } on-error={}")
	w("# :do { /ip hotspot user profile set [find] parent-queue=none } on-error={}")
	w("# :do { /queue type set [find name=\"hotspot-default\"] kind=sfq } on-error={}")
	w("# :do { /queue type remove [find name=\"zyra-fq\"] } on-error={}")
	return b.String(), nil
}
