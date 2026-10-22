package services

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// MikroTikScriptService generates RouterOS .rsc configuration files.
type MikroTikScriptService struct{}

// NewMikroTikScriptService constructs a MikroTikScriptService.
func NewMikroTikScriptService() *MikroTikScriptService { return &MikroTikScriptService{} }

// GenerateScript produces a RouterOS script for a given zone.
func (s *MikroTikScriptService) GenerateScript(zoneID uint) (string, string, error) {
	var zone models.Zone
	if err := config.DB.First(&zone, zoneID).Error; err != nil {
		return "", "", fmt.Errorf("zone not found")
	}

	var packages []models.Package
	config.DB.Where("zone_id = ? AND status = ?", zoneID, "active").Find(&packages)

	var customers []models.Customer
	config.DB.Preload("Package").Where("zone_id = ? AND type = ? AND status = ?", zoneID, "pppoe", "active").Find(&customers)

	var sb strings.Builder

	sb.WriteString("# ============================================================\n")
	sb.WriteString(fmt.Sprintf("# Zyra Net — Zone: %s (%s)\n", zone.Name, zone.Location))
	sb.WriteString(fmt.Sprintf("# Generated: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("# ============================================================\n\n")

	// Default configurations for Bridge, LAN Ports, IP and DHCP
	// Only validated names ever reach the script; anything else falls back to
	// the defaults rather than being written into a router command.
	wanPort, lanPorts, perr := utils.NormalizeRouterPorts(zone.WanPort, zone.LanPorts)
	if perr != nil {
		wanPort, lanPorts = "ether1", "ether2,ether3,ether4"
	}
	doHotspot, doPPPoE := serviceModes(zone.ServiceMode)
	sb.WriteString(fmt.Sprintf("# Internet port: %s | Customer ports: %s | Mode: %s\n\n", wanPort, lanPorts, modeLabel(zone.ServiceMode)))
	hotspotAddr := strings.TrimSpace(zone.HotspotAddress)
	if hotspotAddr == "" {
		hotspotAddr = "10.5.50.1/24"
	}

	gatewayIP := "10.5.50.1"
	networkCIDR := "10.5.50.0/24"
	ipPoolRange := "10.5.50.10-10.5.50.254"

	parts := strings.Split(hotspotAddr, "/")
	if len(parts) > 0 {
		gatewayIP = parts[0]
	}
	ipOctets := strings.Split(gatewayIP, ".")
	if len(ipOctets) == 4 {
		networkCIDR = fmt.Sprintf("%s.%s.%s.0/%s", ipOctets[0], ipOctets[1], ipOctets[2], "24")
		if len(parts) > 1 {
			networkCIDR = fmt.Sprintf("%s.%s.%s.0/%s", ipOctets[0], ipOctets[1], ipOctets[2], parts[1])
		}
		ipPoolRange = fmt.Sprintf("%s.%s.%s.10-%s.%s.%s.254", ipOctets[0], ipOctets[1], ipOctets[2], ipOctets[0], ipOctets[1], ipOctets[2])
	}

	sb.WriteString("# --- Customer ports: joined into one bridge ---\n")
	sb.WriteString(":local br \"bridge-hotspot\";\n")
	sb.WriteString(":if ([:len [/interface bridge find name=\"bridge-hotspot\"]] = 0) do={\n")
	sb.WriteString("  /interface bridge add name=bridge-hotspot comment=\"Zyra Net Hotspot Bridge\" disabled=no\n")
	sb.WriteString("}\n")
	var radios []string
	for _, port := range strings.Split(lanPorts, ",") {
		if utils.IsWirelessPort(port) {
			radios = append(radios, port)
			continue
		}
		sb.WriteString(fmt.Sprintf(":do { /interface bridge port remove [find interface=%s] } on-error={}\n", port))
		sb.WriteString(fmt.Sprintf(":do { /interface bridge port add bridge=$br interface=%s } on-error={}\n", port))
	}
	// Keep the network name routers already broadcast: renaming it would make
	// every customer's phone forget the WiFi.
	ssid := "Zyra Net WiFi"
	if len(radios) > 0 {
		sb.WriteString("\n# --- Built-in WiFi broadcasts to customers (open network; the portal handles login) ---\n")
		for _, r := range radios {
			if strings.HasPrefix(r, "wlan") { // RouterOS 6 / legacy wireless package
				sb.WriteString(":do { /interface wireless cap set enabled=no } on-error={}\n")
				sb.WriteString(":do { /interface wireless security-profiles set [find default=yes] mode=none } on-error={}\n")
				sb.WriteString(fmt.Sprintf(":do { /interface wireless set [find default-name=%s] ssid=\"%s\" mode=ap-bridge disabled=no } on-error={}\n", r, ssid))
			} else { // RouterOS 7 wifi package
				sb.WriteString(fmt.Sprintf(":do { /interface wifi set [find default-name=%s] configuration.ssid=\"%s\" configuration.mode=ap security.authentication-types=\"\" disabled=no } on-error={}\n", r, ssid))
			}
			sb.WriteString(fmt.Sprintf(":do { /interface bridge port remove [find interface=%s] } on-error={}\n", r))
			sb.WriteString(fmt.Sprintf(":do { /interface bridge port add bridge=$br interface=%s } on-error={}\n", r))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n# --- Built-in WiFi off (customers connect through access points on the customer ports) ---\n")
		sb.WriteString(":do { /interface wireless disable [find] } on-error={}\n")
		sb.WriteString(":do { /interface wifi disable [find] } on-error={}\n\n")
	}

	sb.WriteString("# --- IP Address & Gateway Configuration ---\n")
	sb.WriteString(":do { /ip address remove [find comment=\"Zyra Net Hotspot Gateway\"] } on-error={}\n")
	sb.WriteString(fmt.Sprintf(":do { /ip address add address=%s interface=$br comment=\"Zyra Net Hotspot Gateway\" } on-error={}\n\n", hotspotAddr))

	if doHotspot {
		sb.WriteString("# --- IP Pool & DHCP Server Configuration ---\n")
		sb.WriteString(fmt.Sprintf(":if ([:len [/ip pool find name=\"hs-pool-zyranet\"]] = 0) do={ /ip pool add name=hs-pool-zyranet ranges=%s } else={ /ip pool set [find name=\"hs-pool-zyranet\"] ranges=%s }\n", ipPoolRange, ipPoolRange))
		sb.WriteString(":if ([:len [/ip dhcp-server find name=\"hs-dhcp-zyranet\"]] = 0) do={ /ip dhcp-server add name=hs-dhcp-zyranet interface=$br address-pool=hs-pool-zyranet disabled=no lease-time=1h } else={ /ip dhcp-server set [find name=\"hs-dhcp-zyranet\"] interface=$br address-pool=hs-pool-zyranet disabled=no lease-time=1h }\n")
		sb.WriteString(":do { /ip dns set allow-remote-requests=yes servers=8.8.8.8,8.8.4.4 } on-error={}\n")
		sb.WriteString(":do { /ip dhcp-server network remove [find comment=\"Zyra Net Hotspot Network\"] } on-error={}\n")
		sb.WriteString(fmt.Sprintf(":do { /ip dhcp-server network add address=%s gateway=%s dns-server=%s comment=\"Zyra Net Hotspot Network\" } on-error={}\n\n", networkCIDR, gatewayIP, gatewayIP))

		sb.WriteString("# --- Hotspot Server Setup (Overload-Protected & Instant Direct-IP Redirection) ---\n")
		sb.WriteString(fmt.Sprintf(":do { /ip dns static remove [find name=\"login.zyranet.lan\"] } on-error={}\n"))
		sb.WriteString(fmt.Sprintf(":if ([:len [/ip hotspot profile find name=\"hsp-zyranet\"]] = 0) do={ /ip hotspot profile add name=hsp-zyranet hotspot-address=%s login-by=http-pap,http-chap,mac split-user-domain=no dns-name=\"\" } else={ /ip hotspot profile set [find name=\"hsp-zyranet\"] hotspot-address=%s login-by=http-pap,http-chap,mac split-user-domain=no dns-name=\"\" }\n", gatewayIP, gatewayIP))
		sb.WriteString(":if ([:len [/ip hotspot find name=\"hs-zyranet\"]] = 0) do={ /ip hotspot add name=hs-zyranet interface=$br address-pool=hs-pool-zyranet profile=hsp-zyranet idle-timeout=3m keepalive-timeout=1m disabled=no } else={ /ip hotspot set [find name=\"hs-zyranet\"] interface=$br address-pool=hs-pool-zyranet profile=hsp-zyranet idle-timeout=3m keepalive-timeout=1m disabled=no }\n")
		sb.WriteString(":do { /ip hotspot cookie remove [find] } on-error={}\n\n")

		// Allow the cloud captive portal, API, M-Pesa endpoints, and CDN through walled garden (HTTP & HTTPS)
		// NOTE: Do NOT whitelist *gstatic.com or connectivity probes wildcard, as Android/iOS CNA detection depends on intercepting those HTTP probes!
		sb.WriteString("# --- Walled Garden: allow cloud captive portal, API, M-Pesa, and assets (HTTP & HTTPS) ---\n")
		sb.WriteString(":do { /ip hotspot walled-garden remove [find comment=\"Zyra Net Cloud\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden add dst-host=*zyranet.co.ke action=allow comment=\"Zyra Net Cloud\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden remove [find comment=\"Safaricom Daraja\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden add dst-host=*safaricom.co.ke action=allow comment=\"Safaricom Daraja\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden remove [find comment=\"Google Fonts\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden add dst-host=fonts.googleapis.com action=allow comment=\"Google Fonts\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden remove [find comment=\"Google Static Fonts\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden add dst-host=fonts.gstatic.com action=allow comment=\"Google Static Fonts\" } on-error={}\n")
		// Clean up any old wildcard gstatic rules that break Android CNA detection
		sb.WriteString(":do { /ip hotspot walled-garden remove [find dst-host=\"*gstatic.com\"] } on-error={}\n")

		// Walled Garden IP for HTTPS (port 443)
		// RouterOS /ip hotspot walled-garden ip does NOT support wildcards (e.g. *zyranet.co.ke fails DNS lookup).
		// Concrete hostnames must be used so RouterOS resolves their IPs and creates firewall accept rules.
		sb.WriteString(":do { /ip hotspot walled-garden ip remove [find dst-host=\"*zyranet.co.ke\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip remove [find dst-host=\"*safaricom.co.ke\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip remove [find comment~\"Zyra Net\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip remove [find comment~\"Safaricom\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip remove [find comment~\"Google\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip add dst-host=api.zyranet.co.ke action=accept comment=\"Zyra Net API HTTPS\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip add dst-host=captive.zyranet.co.ke action=accept comment=\"Zyra Net Captive HTTPS\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip add dst-host=zyranet.co.ke action=accept comment=\"Zyra Net Apex HTTPS\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip add dst-host=api.safaricom.co.ke action=accept comment=\"Safaricom API HTTPS\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip add dst-host=daraja.safaricom.co.ke action=accept comment=\"Safaricom Daraja HTTPS\" } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip add dst-host=fonts.googleapis.com action=accept comment=\"Google Fonts HTTPS\" } on-error={}\n")
		// Clean up any gstatic rules that break Android CNA detection
		sb.WriteString(":do { /ip hotspot walled-garden ip remove [find dst-host~\"gstatic\"] } on-error={}\n")
		sb.WriteString(":do { /ip hotspot walled-garden ip remove [find dst-host~\"gstatic\"] } on-error={}\n\n")

	}

	// WAN NAT Masquerade
	sb.WriteString("# --- Firewall NAT (Internet Access Masquerade) ---\n")
	sb.WriteString(":if ([:len [/ip firewall nat find comment=\"Zyra Net Internet Access NAT\"]] = 0) do={ /ip firewall nat add chain=srcnat action=masquerade comment=\"Zyra Net Internet Access NAT\" }\n\n")

	// Remote management: tunnel only (see writeManagementAccess)
	writeManagementAccess(&sb)

	if doHotspot {
		// Auto-deploy Cloud Redirect login.html and redirect.html directly to the router's /hotspot directory
		sb.WriteString("# --- Auto-deploy Cloud Redirect login.html & redirect.html ---\n")
		sb.WriteString(fmt.Sprintf(":do { /tool fetch url=\"https://api.zyranet.co.ke/api/v1/public/zones/login-page/%d\" dst-path=\"hotspot/login.html\" %s } on-error={}\n", zone.ID, fetchSecurity(&zone)))
		sb.WriteString(fmt.Sprintf(":do { /tool fetch url=\"https://api.zyranet.co.ke/api/v1/public/zones/redirect-page/%d\" dst-path=\"hotspot/redirect.html\" %s } on-error={}\n\n", zone.ID, fetchSecurity(&zone)))

	}

	// Scheduled heartbeat to report router online health every 1 minute.
	// The sync/heartbeat URLs carry the zone's ProvisionToken (`?token=`) —
	// those endpoints hand back live customer/voucher credentials and accept
	// router-identity updates, so they're gated server-side (zoneTokenAuthorized
	// in handlers/mikrotik_script.go) on this same token.
	sb.WriteString("# --- Live Health & Status Telemetry Heartbeat (1-Min Interval) ---\n")
	sb.WriteString(":do { /system script remove [find name=\"zyranet-heartbeat\"] } on-error={}\n")
	sb.WriteString(fmt.Sprintf(":do { /system script add name=zyranet-heartbeat source=\"%s\" comment=\"Zyra Net Cloud Sync & Telemetry\" } on-error={}\n", heartbeatSource(&zone)))
	sb.WriteString(":do { /system scheduler remove [find name=\"zyranet-heartbeat-sched\"] } on-error={}\n")
	sb.WriteString(":do { /system scheduler add name=zyranet-heartbeat-sched interval=1m on-event=zyranet-heartbeat comment=\"Zyra Net Cloud Telemetry Scheduler\" } on-error={}\n\n")

	// Automated Ghost Device Garbage Collector & Memory Protection
	sb.WriteString("# --- Automated Garbage Collection & Memory Overload Protection ---\n")
	sb.WriteString(":do { /system script remove [find name=\"zyranet-gc\"] } on-error={}\n")
	sb.WriteString(":do { /system script add name=zyranet-gc source=\"/ip hotspot host remove [find unauthorized=yes idle-time>5m]; /ip hotspot cookie remove [find expires-in<0s]\" comment=\"Zyra Net Ghost Device GC\" } on-error={}\n")
	sb.WriteString(":do { /system scheduler remove [find name=\"zyranet-gc-sched\"] } on-error={}\n")
	sb.WriteString(":do { /system scheduler add name=zyranet-gc-sched interval=5m on-event=zyranet-gc comment=\"Zyra Net GC Scheduler\" } on-error={}\n\n")
	sb.WriteString(":do { /system script remove [find name=\"zyranet-memprotect\"] } on-error={}\n")
	sb.WriteString(":do { /system script add name=zyranet-memprotect source=\":if ([/system resource get cpu-load] > 85) do={ /ip dns cache flush; /ip hotspot host remove [find unauthorized=yes] }\" comment=\"Zyra Net CPU & Memory Overload Guard\" } on-error={}\n")
	sb.WriteString(":do { /system scheduler remove [find name=\"zyranet-memprotect-sched\"] } on-error={}\n")
	sb.WriteString(":do { /system scheduler add name=zyranet-memprotect-sched interval=2m on-event=zyranet-memprotect comment=\"Zyra Net Overload Scheduler\" } on-error={}\n\n")

	if doHotspot {
		// Plan profiles only. Logins are per device (own username/password,
		// bound to the device MAC) and arrive with the 1-minute sync; shared
		// pkg-<id> / free / voucher-code users are never created any more.
		sb.WriteString("# --- Hotspot Plan Profiles ---\n")
		for i := range packages {
			if packages[i].Type != "hotspot" {
				continue
			}
			writeHotspotProfile(&sb, &packages[i])
		}
		sb.WriteString("\n")
		writeLegacyLoginCleanup(&sb)
	}

	if doPPPoE {
		// PPPoE Pool & Profiles
		sb.WriteString("# --- PPPoE Remote IP Pool & Profiles ---\n")
		sb.WriteString(":if ([:len [/ip pool find name=\"pool-pppoe-zyranet\"]] = 0) do={ /ip pool add name=pool-pppoe-zyranet ranges=10.10.0.2-10.10.3.254 } else={ /ip pool set [find name=\"pool-pppoe-zyranet\"] ranges=10.10.0.2-10.10.3.254 }\n")
		for _, pkg := range packages {
			if pkg.Type != "pppoe" {
				continue
			}
			profileName := sanitizeProfileName(pkg.Name)
			rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
			sb.WriteString(fmt.Sprintf(
				":if ([:len [/ppp profile find name=\"%s\"]] = 0) do={ /ppp profile add name=\"%s\" rate-limit=\"%s\" local-address=10.10.0.1 remote-address=pool-pppoe-zyranet dns-server=8.8.8.8,8.8.4.4 } else={ /ppp profile set [find name=\"%s\"] rate-limit=\"%s\" local-address=10.10.0.1 remote-address=pool-pppoe-zyranet dns-server=8.8.8.8,8.8.4.4 }\n",
				profileName, profileName, rateLimit, profileName, rateLimit,
			))
		}
		sb.WriteString("\n")

		// PPPoE Server Setup
		sb.WriteString("# --- PPPoE Server Setup ---\n")
		sb.WriteString(":if ([:len [/interface pppoe-server server find service-name=\"pppoe-zyranet\"]] = 0) do={ /interface pppoe-server server add service-name=pppoe-zyranet interface=$br default-profile=default authentication=pap,chap disabled=no }\n\n")

		// PPPoE Secrets
		sb.WriteString("# --- PPPoE Secrets ---\n")
		for _, c := range customers {
			if c.Package == nil {
				continue
			}
			profileName := sanitizeProfileName(c.Package.Name)
			username := strVal(c.PPPoEUsername)
			if username == "" {
				username = strings.ReplaceAll(strings.ToLower(c.Name), " ", ".")
			}
			password := strVal(c.PPPoEPassword)
			if password == "" {
				// Never invent a guessable shared password; set one on the customer first.
				sb.WriteString(fmt.Sprintf("# skipped customer %d: no PPPoE password set\n", c.ID))
				continue
			}
			username, password = rosEscape(username), rosEscape(password)
			sb.WriteString(fmt.Sprintf(
				":if ([:len [/ppp secret find name=\"%s\"]] = 0) do={ /ppp secret add name=\"%s\" password=\"%s\" service=pppoe profile=\"%s\" comment=\"customer_id:%d\" } else={ /ppp secret set [find name=\"%s\"] password=\"%s\" service=pppoe profile=\"%s\" comment=\"customer_id:%d\" }\n",
				username, username, password, profileName, c.ID, username, password, profileName, c.ID,
			))
		}

	}

	sb.WriteString("\n# --- Trigger Cloud Heartbeat Immediately ---\n")
	sb.WriteString(":do { /system script run zyranet-heartbeat } on-error={}\n")

	filename := fmt.Sprintf("zone-%s-%s.rsc",
		strings.ReplaceAll(strings.ToLower(zone.Name), " ", "-"),
		time.Now().Format("20060102150405"),
	)

	return sb.String(), filename, nil
}

// formatRateLimitWithBurst calculates steady-state rate limit with an instant 20Mbps burst for lightning-fast responsiveness
func formatRateLimitWithBurst(upKbps, downKbps int) string {
	if upKbps <= 0 || downKbps <= 0 {
		return "10M/10M 20M/20M 7M/7M 15/15 8"
	}
	burstUp := 20000
	if upKbps > burstUp {
		burstUp = upKbps * 3 / 2
	}
	burstDown := 20000
	if downKbps > burstDown {
		burstDown = downKbps * 3 / 2
	}
	threshUp := upKbps * 7 / 10
	if threshUp < 512 {
		threshUp = 512
	}
	threshDown := downKbps * 7 / 10
	if threshDown < 512 {
		threshDown = 512
	}
	return fmt.Sprintf("%dk/%dk %dk/%dk %dk/%dk 15/15 8", upKbps, downKbps, burstUp, burstDown, threshUp, threshDown)
}

// GenerateSyncScript generates the RouterOS commands the router imports every
// minute (it fetches them from /public/zones/sync/:id with its token). It:
//  1. keeps the hotspot login methods and the redirect page current;
//  2. removes the old shared logins (pkg-<id>, free, MAC=password users and
//     raw voucher-code users) that let anyone browse for free;
//  3. makes sure every paid-up device of this ISP has its own router login
//     (random username/password, bound to its MAC, limit-uptime = paid time)
//     and updates logins that changed recently;
//  4. removes logins that expired or were retired since the router last
//     synced, and ends their sessions;
//  5. keeps router management reachable only from the Zyra Net tunnel.
//
// It only reads the database: expiring customers is the watchdog's job.
func (s *MikroTikScriptService) GenerateSyncScript(zoneID uint) (string, error) {
	var zone models.Zone
	if err := config.DB.First(&zone, zoneID).Error; err != nil {
		return "# Zone not found\n", fmt.Errorf("zone not found")
	}

	now := time.Now()
	doHotspot, _ := serviceModes(zone.ServiceMode)

	var sb strings.Builder
	sb.WriteString("# Zyra Net Dynamic RouterOS Sync & Instant Activator\n")
	sb.WriteString(fmt.Sprintf("# Zone: %s (ID: %d) | Generated: %s\n\n", zone.Name, zone.ID, now.Format("2006-01-02 15:04:05")))

	if doHotspot {
		// Ensure hotspot profile authentication methods
		sb.WriteString("# --- Hotspot Authentication Methods ---\n")
		sb.WriteString(":do { /ip hotspot profile set [find name=\"hsp-zyranet\"] login-by=mac,http-pap,http-chap split-user-domain=no } on-error={}\n")
		sb.WriteString(":do { /ip hotspot profile set [find name=\"default\"] login-by=mac,http-pap,http-chap split-user-domain=no } on-error={}\n")
		// Ensure hotspot redirect.html points to destination, not captive portal
		sb.WriteString(fmt.Sprintf(":do { /tool fetch url=\"https://api.zyranet.co.ke/api/v1/public/zones/redirect-page/%d\" dst-path=\"hotspot/redirect.html\" %s } on-error={}\n\n", zone.ID, fetchSecurity(&zone)))
	}

	// Auto-upgrade heartbeat script to push real live telemetry metrics
	sb.WriteString("# --- Auto-Upgrade Live Telemetry Heartbeat ---\n")
	sb.WriteString(fmt.Sprintf(":do { /system script set [find name=\"zyranet-heartbeat\"] source=\"%s\" } on-error={}\n\n", heartbeatSource(&zone)))

	if doHotspot {
		writeLegacyLoginCleanup(&sb)

		// Logins changed since a little before the router's previous sync are
		// (re)written; older unchanged ones are only added if missing.
		windowStart := now.Add(-syncWindow)
		if zone.LastSyncAt != nil && zone.LastSyncAt.Add(-syncWindow).Before(windowStart) {
			windowStart = zone.LastSyncAt.Add(-syncWindow)
		}
		if windowStart.Before(now.Add(-maxSyncLookback)) {
			windowStart = now.Add(-maxSyncLookback)
		}

		var active []models.HotspotCredential
		config.DB.Where("organization_id = ? AND status = ? AND expires_at > ?", zone.OrganizationID, "active", now).
			Order("id").Find(&active)
		var gone []models.HotspotCredential
		config.DB.Where("organization_id = ? AND updated_at > ? AND (status <> ? OR expires_at <= ?)", zone.OrganizationID, windowStart, "active", now).
			Order("id").Find(&gone)

		// Profiles: this zone's plans plus any plan a login uses.
		pkgIDs := map[uint]bool{}
		for _, c := range active {
			pkgIDs[c.PackageID] = true
		}
		var packages []models.Package
		q := config.DB.Where("(zone_id = ? AND status = ? AND type = ?)", zone.ID, "active", "hotspot")
		if len(pkgIDs) > 0 {
			ids := make([]uint, 0, len(pkgIDs))
			for id := range pkgIDs {
				ids = append(ids, id)
			}
			q = q.Or("id IN ?", ids)
		}
		q.Order("id").Find(&packages)
		pkgByID := map[uint]*models.Package{}
		sb.WriteString("# --- Hotspot Plan Profiles (one login per device) ---\n")
		for i := range packages {
			pkgByID[packages[i].ID] = &packages[i]
			writeHotspotProfile(&sb, &packages[i])
		}
		sb.WriteString("\n")

		sb.WriteString("# --- Device Logins (own username/password, bound to the device MAC) ---\n")
		for i := range active {
			c := &active[i]
			pkg := pkgByID[c.PackageID]
			if pkg == nil {
				continue
			}
			writeCredentialUser(&sb, c, pkg, c.UpdatedAt.After(windowStart), c.ActivatedAt.After(windowStart))
		}
		sb.WriteString("\n")

		sb.WriteString("# --- Expired / Retired Device Logins ---\n")
		for i := range gone {
			writeCredentialRemoval(&sb, &gone[i])
		}
		sb.WriteString("\n")
	}

	writeManagementAccess(&sb)
	return sb.String(), nil
}

const (
	// syncWindow: changes this recent are always re-sent (covers a missed or
	// failed import between two syncs).
	syncWindow = 10 * time.Minute
	// maxSyncLookback bounds how far back removals are looked up for a router
	// that has been offline a long time. Longer than that, the login's own
	// limit-uptime has long run out on the router anyway.
	maxSyncLookback = 30 * 24 * time.Hour
)

// fetchSecurity is the /tool fetch options for calls to our API: https, and
// certificate checking when the zone has it switched on (Zone.VerifyTLS).
func fetchSecurity(zone *models.Zone) string {
	if zone.VerifyTLS {
		return "mode=https check-certificate=yes"
	}
	return "mode=https"
}

// heartbeatSource is the body of the router's zyranet-heartbeat script (already
// escaped for a double-quoted RouterOS string): fetch + import the sync
// script, then report telemetry. Both URLs carry the zone's token.
func heartbeatSource(zone *models.Zone) string {
	sec := fetchSecurity(zone)
	return fmt.Sprintf("/tool fetch url=\\\"https://api.zyranet.co.ke/api/v1/public/zones/sync/%d?token=%s\\\" dst-path=\\\"zyra-sync.rsc\\\" %s; :if ([:len [/file find name=\\\"zyra-sync.rsc\\\"]] > 0) do={ /import file-name=zyra-sync.rsc; /file remove [find name=\\\"zyra-sync.rsc\\\"] }; :local c [/system resource get cpu-load]; :local tm [/system resource get total-memory]; :local fm [/system resource get free-memory]; :local cl [:len [/ip hotspot active find]]; :local b [/system resource get board-name]; /tool fetch url=(\\\"https://api.zyranet.co.ke/api/v1/public/zones/heartbeat/%d?token=%s&cpu=\\\" . \\$c . \\\"&totalmem=\\\" . \\$tm . \\\"&freemem=\\\" . \\$fm . \\\"&clients=\\\" . \\$cl . \\\"&board=\\\" . \\$b) %s keep-result=no",
		zone.ID, zone.ProvisionToken, sec, zone.ID, zone.ProvisionToken, sec)
}

// writeLegacyLoginCleanup removes the shared router logins earlier scripts
// created, and ends sessions that used them. Idempotent: a no-op once clean.
func writeLegacyLoginCleanup(sb *strings.Builder) {
	sb.WriteString("# --- Remove old shared logins (pkg-<id>, free, MAC=password, voucher codes) ---\n")
	sb.WriteString(":do { /ip hotspot active remove [find user~\"^pkg-\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot active remove [find user=\"free\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot user remove [find comment=\"Zyra Net Package Auto-Connect\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot user remove [find name~\"^pkg-\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot user remove [find comment=\"Zyra Net Free Tier Auto-Connect\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot user remove [find name=\"free\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot user remove [find comment=\"ZyraNet Paid Plan\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot user remove [find comment~\"^pkg:\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot ip-binding remove [find comment~\"ZyraNet\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot ip-binding remove [find type=bypassed] } on-error={}\n\n")
}

// writeHotspotProfile writes a plan's user profile. shared-users=1: every
// device has its own login, so one login is one device.
func writeHotspotProfile(sb *strings.Builder, pkg *models.Package) {
	profileName := sanitizeProfileName(pkg.Name)
	rateLimit := formatRateLimitWithBurst(pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
	timeout := "0s"
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		timeout = fmt.Sprintf("%dm", *pkg.TimeLimitMinutes)
	}
	sb.WriteString(fmt.Sprintf(
		":if ([:len [/ip hotspot user profile find name=\"%s\"]] = 0) do={ /ip hotspot user profile add name=\"%s\" rate-limit=\"%s\" session-timeout=\"%s\" idle-timeout=3m keepalive-timeout=1m shared-users=1 add-mac-cookie=no } else={ /ip hotspot user profile set [find name=\"%s\"] rate-limit=\"%s\" session-timeout=\"%s\" idle-timeout=3m keepalive-timeout=1m shared-users=1 add-mac-cookie=no }\n",
		profileName, profileName, rateLimit, timeout, profileName, rateLimit, timeout,
	))
}

// writeCredentialUser makes sure a device login exists on the router. A login
// changed recently is also rewritten (new expiry/plan); one (re)activated
// recently gets its uptime counter reset so limit-uptime counts from now.
func writeCredentialUser(sb *strings.Builder, c *models.HotspotCredential, pkg *models.Package, changed, reactivated bool) {
	mac, ok := NormalizeMAC(c.MacAddress)
	if !ok || !credNameRe.MatchString(c.Username) || !credPassRe.MatchString(c.Password) {
		sb.WriteString(fmt.Sprintf("# skipped login %d: invalid stored values\n", c.ID))
		return
	}
	attrs := fmt.Sprintf("password=\"%s\" mac-address=%s profile=\"%s\" limit-uptime=%s", c.Password, mac, sanitizeProfileName(pkg.Name), credentialLimitUptime(c))
	if pkg.DataLimitMB != nil && *pkg.DataLimitMB > 0 {
		attrs += fmt.Sprintf(" limit-bytes-total=%d", int64(*pkg.DataLimitMB)*1024*1024)
	}
	attrs += fmt.Sprintf(" comment=\"%s\"", credentialComment(c))
	if changed {
		sb.WriteString(fmt.Sprintf(":if ([:len [/ip hotspot user find name=\"%s\"]] = 0) do={ /ip hotspot user add name=\"%s\" %s } else={ /ip hotspot user set [find name=\"%s\"] %s }\n", c.Username, c.Username, attrs, c.Username, attrs))
		if reactivated {
			sb.WriteString(fmt.Sprintf(":do { /ip hotspot user reset-counters [find name=\"%s\"] } on-error={}\n", c.Username))
		}
		return
	}
	sb.WriteString(fmt.Sprintf(":if ([:len [/ip hotspot user find name=\"%s\"]] = 0) do={ /ip hotspot user add name=\"%s\" %s }\n", c.Username, c.Username, attrs))
}

// writeCredentialRemoval takes an expired/retired login off the router and
// ends its session.
func writeCredentialRemoval(sb *strings.Builder, c *models.HotspotCredential) {
	if !credNameRe.MatchString(c.Username) {
		return
	}
	sb.WriteString(fmt.Sprintf(":do { /ip hotspot active remove [find user=\"%s\"] } on-error={}\n", c.Username))
	sb.WriteString(fmt.Sprintf(":do { /ip hotspot cookie remove [find user=\"%s\"] } on-error={}\n", c.Username))
	sb.WriteString(fmt.Sprintf(":do { /ip hotspot user remove [find name=\"%s\"] } on-error={}\n", c.Username))
}

var (
	credNameRe = regexp.MustCompile(`^zn[a-z0-9]{8}$`)
	credPassRe = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
)

// managementSources is who may reach WebFig/WinBox/REST on a router: the
// Zyra Net WireGuard subnet, plus anything in ROUTER_MGMT_ALLOWED
// (comma-separated IPs/CIDRs, e.g. the API server's public IP for routers
// managed without the tunnel).
func managementSources() []string {
	return addressList("10.200.0.0/24", os.Getenv("ROUTER_MGMT_ALLOWED"))
}

// apiSources is who may use the RouterOS API: the API server only — its
// tunnel address (RADIUS_SERVER_ADDR, default 10.200.0.1) plus
// ROUTER_API_ALLOWED.
func apiSources() []string {
	server := strings.TrimSpace(config.Config.RadiusServerAddr)
	if net.ParseIP(server) == nil {
		server = "10.200.0.1"
	}
	return addressList(server+"/32", os.Getenv("ROUTER_API_ALLOWED"))
}

// addressList returns base plus the valid IPs/CIDRs in extra (invalid entries
// are dropped so nothing odd reaches a router command).
func addressList(base, extra string) []string {
	out := []string{base}
	for _, e := range strings.Split(extra, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			if ip.To4() != nil {
				out = append(out, e+"/32")
			}
			continue
		}
		if ip, _, err := net.ParseCIDR(e); err == nil && ip.To4() != nil {
			out = append(out, e)
		}
	}
	return out
}

// writeManagementAccess keeps WebFig/WinBox/REST reachable only from the
// management sources and the RouterOS API (plain and TLS) only from the API
// server; everyone else — the internet and the hotspot guests — is dropped.
func writeManagementAccess(sb *strings.Builder) {
	mgmt := strings.Join(managementSources(), ",")
	api := strings.Join(apiSources(), ",")
	sb.WriteString("# --- Remote management: only over the Zyra Net tunnel (API: the Zyra Net server only) ---\n")
	sb.WriteString(":do { /ip firewall address-list remove [find list=\"zyranet-mgmt\"] } on-error={}\n")
	for _, a := range append(managementSources(), apiSources()...) {
		sb.WriteString(fmt.Sprintf(":do { /ip firewall address-list add list=zyranet-mgmt address=%s comment=\"Zyra Net management\" } on-error={}\n", a))
	}
	sb.WriteString(fmt.Sprintf(":do { /ip service set www disabled=no port=80 address=%s } on-error={}\n", mgmt))
	sb.WriteString(fmt.Sprintf(":do { /ip service set www-ssl address=%s } on-error={}\n", mgmt))
	sb.WriteString(fmt.Sprintf(":do { /ip service set winbox disabled=no port=8291 address=%s } on-error={}\n", mgmt))
	sb.WriteString(fmt.Sprintf(":do { /ip service set api disabled=no port=8728 address=%s } on-error={}\n", api))
	sb.WriteString(fmt.Sprintf(":do { /ip service set api-ssl disabled=no port=8729 address=%s } on-error={}\n", api))
	sb.WriteString(":do { /ip cloud set ddns-enabled=yes update-time=yes } on-error={}\n")
	sb.WriteString(":do { /ip firewall filter remove [find comment=\"Zyra Net Remote Access\"] } on-error={}\n")
	sb.WriteString(":do { /ip firewall filter remove [find comment=\"Zyra Net Block Hotspot Management\"] } on-error={}\n")
	sb.WriteString(":do { /ip firewall filter remove [find comment=\"Zyra Net Block Management\"] } on-error={}\n")
	sb.WriteString(":if ([:len [/ip firewall filter find]] > 0) do={ :do { /ip firewall filter add chain=input protocol=tcp dst-port=80,443,8291,8728,8729 src-address-list=zyranet-mgmt action=accept comment=\"Zyra Net Remote Access\" place-before=0 } on-error={} } else={ :do { /ip firewall filter add chain=input protocol=tcp dst-port=80,443,8291,8728,8729 src-address-list=zyranet-mgmt action=accept comment=\"Zyra Net Remote Access\" } on-error={} }\n")
	sb.WriteString(":do { /ip firewall filter add chain=input protocol=tcp dst-port=80,443,8291,8728,8729 src-address-list=!zyranet-mgmt action=drop comment=\"Zyra Net Block Management\" place-before=1 } on-error={ :do { /ip firewall filter add chain=input protocol=tcp dst-port=80,443,8291,8728,8729 src-address-list=!zyranet-mgmt action=drop comment=\"Zyra Net Block Management\" } on-error={} }\n\n")
}

// rosEscape makes a value safe inside a double-quoted RouterOS string: it
// escapes backslash, quote and $ (variable expansion) and drops line breaks,
// so a customer's name or password can never end the string or add commands.
func rosEscape(v string) string {
	v = strings.NewReplacer("\r", "", "\n", "").Replace(v)
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(v)
}

// serviceModes turns a zone's service mode into what the script configures.
// Unknown/empty means "both" — what every zone got before modes existed.
func serviceModes(mode string) (hotspot, pppoe bool) {
	switch mode {
	case "hotspot":
		return true, false
	case "pppoe":
		return false, true
	}
	return true, true
}

func modeLabel(mode string) string {
	switch mode {
	case "hotspot":
		return "WiFi hotspot"
	case "pppoe":
		return "PPPoE billing only"
	}
	return "WiFi hotspot + PPPoE"
}
