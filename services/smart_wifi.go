package services

import (
	"fmt"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// Smart WiFi: three router behaviours delivered by the 1-minute sync.
//
//  1. Stay logged in. Every plan profile adds a MAC cookie, so a paid phone
//     that drops off the WiFi (asleep, walked away, keepalive) is let straight
//     back in by the router: no portal, no tap. The cookie dies with the
//     login (writeCredentialRemoval removes cookies), so it never outlives
//     the plan.
//  2. Plan used up → portal. When a login's time or data runs out on the
//     router (on-logout) or the server retires it (expiry, writeCredential
//     Removal), the device is taken off the built-in WiFi for a moment. The
//     phone rejoins, its "sign in to network" check hits the hotspot and the
//     portal opens on the plans.
//  3. Smart speed. Traffic is sorted into three lanes on the customer bridge
//     (download) and the internet port (upload): games, calls, DNS and ping
//     first; page loads and messages next; big transfers (video, downloads,
//     updates) last. Lanes only matter when the line is full; each customer
//     is still held to their own plan speed by their hotspot queue.

// zoneRadios says which WiFi packages the zone's built-in radios use, from
// its saved ports (same split as the setup script): wlanN = legacy wireless,
// anything else wireless = RouterOS 7 wifi. Only commands for a package the
// router has are ever written: an unknown menu is a syntax error that would
// stop the whole sync import.
func zoneRadios(zone *models.Zone) (legacy, wifi bool) {
	_, lanPorts, err := utils.NormalizeRouterPorts(zone.WanPort, zone.LanPorts)
	if err != nil {
		return false, false
	}
	for _, p := range strings.Split(lanPorts, ",") {
		if !utils.IsWirelessPort(p) {
			continue
		}
		if strings.HasPrefix(p, "wlan") {
			legacy = true
		} else {
			wifi = true
		}
	}
	return legacy, wifi
}

// kickCommands drops a device (MAC held in the RouterOS expression macExpr)
// off the hotspot and the built-in WiFi so its phone rejoins and shows the
// portal.
func kickCommands(zone *models.Zone, macExpr string) []string {
	cmds := []string{fmt.Sprintf(":do { /ip hotspot host remove [find mac-address=%s] } on-error={}", macExpr)}
	legacy, wifi := zoneRadios(zone)
	if legacy {
		cmds = append(cmds, fmt.Sprintf(":do { /interface wireless registration-table remove [find mac-address=%s] } on-error={}", macExpr))
	}
	if wifi {
		cmds = append(cmds, fmt.Sprintf(":do { /interface wifi registration-table remove [find mac-address=%s] } on-error={}", macExpr))
	}
	return cmds
}

// writeDeviceKick is the server-side kick for a login the server retired.
// Skipped on the router when the device still has a login there (it renewed
// or moved to a new plan in the same minute).
func writeDeviceKick(sb *strings.Builder, zone *models.Zone, mac string) {
	if zone == nil {
		return
	}
	sb.WriteString(fmt.Sprintf(":if ([:len [/ip hotspot user find mac-address=%s]] = 0) do={ %s }\n", mac, strings.Join(kickCommands(zone, mac), "; ")))
}

// endedSince: the login ended after the router's previous sync (or in the
// last 2 minutes if it never synced), so the kick goes out exactly once and
// a device sitting on the portal isn't dropped again every minute.
func endedSince(c *models.HotspotCredential, lastSync *time.Time, now time.Time) bool {
	ended := c.UpdatedAt
	if c.Status == "active" && !c.ExpiresAt.After(now) {
		ended = c.ExpiresAt
	}
	from := now.Add(-2 * time.Minute)
	if lastSync != nil {
		from = *lastSync
	}
	return ended.After(from) && !ended.After(now)
}

// hotspotLogoutScript is a plan profile's on-logout: if the login is used up
// (gone, uptime limit reached or data limit reached) the device is kicked so
// the portal opens. Any other logout (phone asleep, keepalive, moved away)
// leaves it alone: the MAC cookie lets it back in silently.
func hotspotLogoutScript(zone *models.Zone) string {
	kick := strings.Join(kickCommands(zone, "$m"), "; ")
	return `:do { :local m $"mac-address"; ` +
		`:if ([:len $m] = 0) do={ :do { :set m [/ip hotspot host get [find address=$address] mac-address] } on-error={} }; ` +
		`:local done false; :local id [/ip hotspot user find name=$user]; ` +
		`:if ([:len $id] = 0) do={ :set done true } else={ ` +
		// Each check guarded on its own: a plan without a data limit has no
		// limit-bytes-total, and that error must not skip the time check.
		`:do { :local lu [/ip hotspot user get $id limit-uptime]; :if ($lu > 0s && [/ip hotspot user get $id uptime] >= $lu) do={ :set done true } } on-error={}; ` +
		`:do { :local lb [/ip hotspot user get $id limit-bytes-total]; :if ($lb > 0 && ([/ip hotspot user get $id bytes-in] + [/ip hotspot user get $id bytes-out]) >= $lb) do={ :set done true } } on-error={} }; ` +
		`:if ($done && [:len $m] > 0) do={ ` + kick + ` } } on-error={}`
}

// writeProfileExtras adds the MAC cookie and the used-up kick to a plan
// profile. Kept on its own guarded line: a router that rejects any of it
// still gets every login below.
func writeProfileExtras(sb *strings.Builder, zone *models.Zone, profileName string) {
	sb.WriteString(fmt.Sprintf(":do { /ip hotspot user profile set [find name=\"%s\"] add-mac-cookie=yes mac-cookie-timeout=3d on-logout=\"%s\" } on-error={}\n",
		profileName, rosEscape(hotspotLogoutScript(zone))))
}

const smartSpeedVersion = "v1"

// smartSpeedLanes: name, mark, HTB priority, guaranteed share of the line (%).
var smartSpeedLanes = []struct {
	name, mark string
	prio, pct  int
}{
	{"rt", "zq-rt", 1, 20},       // games, calls, DNS, ping
	{"quick", "zq-quick", 3, 40}, // page loads, messages, first 2 MB of anything
	{"heavy", "zq-heavy", 7, 10}, // big transfers: video, downloads, updates
}

// SmartSpeedState is what the admin shows: on/off and why.
func SmartSpeedState(zone *models.Zone) (active bool, reason string) {
	switch {
	case !zone.SmartSpeed:
		return false, "Switched off"
	case zone.UplinkDownMbps < 1 || zone.UplinkUpMbps < 1:
		return false, "Needs the line speed (download and upload)"
	default:
		return true, "On"
	}
}

// writeSmartSpeed puts the traffic lanes on the router, or takes them off
// when switched off. The setup is tagged with a signature and rebuilt only
// when it changes, so the router isn't touched every minute.
func writeSmartSpeed(sb *strings.Builder, zone *models.Zone) {
	sb.WriteString("# --- Smart speed: games and calls first, big downloads last ---\n")
	clear := `:do { /queue tree remove [find comment~"^zyra-smart"] } on-error={}
:do { /ip firewall mangle remove [find comment~"^zyra-smart"] } on-error={}
`
	if on, _ := SmartSpeedState(zone); !on {
		sb.WriteString(`:if ([:len [/queue tree find comment~"^zyra-smart"]] > 0 || [:len [/ip firewall mangle find comment~"^zyra-smart"]] > 0) do={` + "\n" + clear + "}\n\n")
		return
	}
	wanPort, _, err := utils.NormalizeRouterPorts(zone.WanPort, zone.LanPorts)
	if err != nil {
		wanPort = "ether1"
	}
	down, up := tunedKbps(zone.UplinkDownMbps), tunedKbps(zone.UplinkUpMbps)
	sig := fmt.Sprintf("zyra-smart %s d=%d u=%d wan=%s", smartSpeedVersion, down, up, wanPort)

	var b strings.Builder
	w := func(format string, a ...interface{}) { fmt.Fprintf(&b, format+"\n", a...) }
	w(`:if ([:len [/queue tree find comment=%q]] = 0) do={`, sig)
	b.WriteString(clear)
	// The internet port by its default name (it may have been renamed).
	w(`:local wan %q`, wanPort)
	w(`:do { :local i [/interface find default-name=%q]; :if ([:len $i] > 0) do={ :set wan [/interface get [:pick $i 0] name] } } on-error={}`, wanPort)
	w(`:if ([:len [/queue type find name="zq-sfq"]] = 0) do={ /queue type add name=zq-sfq kind=sfq }`)
	// Mangle (forward only = customer traffic through the router). First match
	// wins (passthrough=no). The heavy lane is anything past 2 MB on one
	// connection that isn't real-time.
	m := `/ip firewall mangle add chain=forward action=mark-packet passthrough=no comment="zyra-smart"`
	w(`%s protocol=icmp new-packet-mark=zq-rt`, m)
	w(`%s protocol=udp port=53 new-packet-mark=zq-rt`, m)
	w(`%s protocol=tcp port=53 new-packet-mark=zq-rt`, m)
	// Calls: STUN/TURN (WhatsApp, Meet, Teams) and Zoom.
	w(`%s protocol=udp port=3478-3481,8801-8810 new-packet-mark=zq-rt`, m)
	// Games and voice: small UDP packets that aren't web (QUIC is 443).
	w(`%s protocol=udp src-port=!443 dst-port=!443 packet-size=0-600 new-packet-mark=zq-rt`, m)
	w(`%s connection-bytes=0-2000000 new-packet-mark=zq-quick`, m)
	w(`%s new-packet-mark=zq-heavy`, m)
	// Lanes: download on the customer bridge, upload on the internet port.
	// The signature goes on the LAST queue: if any line fails, it's missing
	// and the next sync clears and retries instead of keeping half a setup.
	dirs := []struct {
		name, parent string
		kbps         int
	}{{"down", "bridge-hotspot", down}, {"up", "$wan", up}}
	for di, dir := range dirs {
		root := "zq-" + dir.name
		w(`/queue tree add name=%s parent=%s max-limit=%dk comment="zyra-smart"`, root, dir.parent, dir.kbps)
		for li, l := range smartSpeedLanes {
			tag := "zyra-smart"
			if di == len(dirs)-1 && li == len(smartSpeedLanes)-1 {
				tag = sig
			}
			w(`/queue tree add name=%s-%s parent=%s packet-mark=%s priority=%d limit-at=%dk max-limit=%dk queue=zq-sfq comment=%q`,
				root, l.name, root, l.mark, l.prio, dir.kbps*l.pct/100, dir.kbps, tag)
		}
	}
	w(`:log info "zyra: smart speed applied (%s)"`, sig)
	b.WriteString("}\n\n")
	// One guard so a router that refuses any line keeps syncing logins.
	sb.WriteString(":do {\n" + b.String() + "} on-error={ :log warning \"zyra: smart speed could not be applied\" }\n\n")
}
