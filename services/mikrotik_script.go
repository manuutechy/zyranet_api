package services

import (
	"fmt"
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

	var vouchers []models.Voucher
	config.DB.Preload("Package").Where("zone_id = ? AND status IN ?", zoneID, []string{"unused", "active"}).Find(&vouchers)

	var customers []models.Customer
	config.DB.Preload("Package").Where("zone_id = ? AND type = ? AND status = ?", zoneID, "pppoe", "active").Find(&customers)

	var sb strings.Builder

	sb.WriteString("# ============================================================\n")
	sb.WriteString(fmt.Sprintf("# Zyra Net — Zone: %s (%s)\n", zone.Name, zone.Location))
	sb.WriteString(fmt.Sprintf("# Generated: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("# ============================================================\n\n")

	// Default configurations for Bridge, LAN Ports, IP and DHCP
	lanPorts := strings.TrimSpace(zone.LanPorts)
	if lanPorts == "" {
		lanPorts = "ether2,ether3,ether4"
	}
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

	sb.WriteString("# --- Bridge & LAN Interface Setup (Ethernet Ports 2-4 for External APs) ---\n")
	sb.WriteString(":local br \"bridge-hotspot\";\n")
	sb.WriteString(":if ([:len [/interface bridge find name=\"bridge-hotspot\"]] = 0) do={\n")
	sb.WriteString("  /interface bridge add name=bridge-hotspot comment=\"Zyra Net Hotspot Bridge\" disabled=no\n")
	sb.WriteString("}\n")
	portsList := strings.Split(lanPorts, ",")
	hasWlan := false
	for _, port := range portsList {
		port = strings.TrimSpace(port)
		if port == "wlan1" {
			hasWlan = true
		} else if port != "" {
			sb.WriteString(fmt.Sprintf(":do { /interface bridge port remove [find interface=%s] } on-error={}\n", port))
			sb.WriteString(fmt.Sprintf(":do { /interface bridge port add bridge=$br interface=%s } on-error={}\n", port))
		}
	}
	if hasWlan {
		sb.WriteString("\n# --- Wireless Hotspot Setup (AP Mode, Open) ---\n")
		sb.WriteString(":do { /interface wireless cap set enabled=no } on-error={}\n")
		sb.WriteString(":do { /interface wireless security-profiles set [find default=yes] mode=none } on-error={}\n")
		sb.WriteString(":do { /interface wireless set [find default-name=wlan1] ssid=\"Zyra Net WiFi\" mode=ap-bridge disabled=no } on-error={}\n")
		sb.WriteString(":do { /interface bridge port remove [find interface=wlan1] } on-error={}\n")
		sb.WriteString(":do { /interface bridge port add bridge=$br interface=wlan1 } on-error={}\n\n")
	} else {
		sb.WriteString("\n# --- Wireless Disabled (External APs Connected to Ethernet Ports) ---\n")
		sb.WriteString(":do { /interface wireless disable [find default-name=wlan1] } on-error={}\n")
		sb.WriteString(":do { /interface bridge port remove [find interface=wlan1] } on-error={}\n\n")
	}

	sb.WriteString("# --- IP Address & Gateway Configuration ---\n")
	sb.WriteString(":do { /ip address remove [find comment=\"Zyra Net Hotspot Gateway\"] } on-error={}\n")
	sb.WriteString(fmt.Sprintf(":do { /ip address add address=%s interface=$br comment=\"Zyra Net Hotspot Gateway\" } on-error={}\n\n", hotspotAddr))

	sb.WriteString("# --- IP Pool & DHCP Server Configuration ---\n")
	sb.WriteString(fmt.Sprintf(":if ([:len [/ip pool find name=\"hs-pool-zyranet\"]] = 0) do={ /ip pool add name=hs-pool-zyranet ranges=%s } else={ /ip pool set [find name=\"hs-pool-zyranet\"] ranges=%s }\n", ipPoolRange, ipPoolRange))
	sb.WriteString(":if ([:len [/ip dhcp-server find name=\"hs-dhcp-zyranet\"]] = 0) do={ /ip dhcp-server add name=hs-dhcp-zyranet interface=$br address-pool=hs-pool-zyranet disabled=no lease-time=1h } else={ /ip dhcp-server set [find name=\"hs-dhcp-zyranet\"] interface=$br address-pool=hs-pool-zyranet disabled=no lease-time=1h }\n")
	sb.WriteString(":do { /ip dns set allow-remote-requests=yes servers=8.8.8.8,8.8.4.4 } on-error={}\n")
	sb.WriteString(":do { /ip dhcp-server network remove [find comment=\"Zyra Net Hotspot Network\"] } on-error={}\n")
	sb.WriteString(fmt.Sprintf(":do { /ip dhcp-server network add address=%s gateway=%s dns-server=%s comment=\"Zyra Net Hotspot Network\" } on-error={}\n\n", networkCIDR, gatewayIP, gatewayIP))

	sb.WriteString("# --- Hotspot Server Setup (Overload-Protected & Instant Direct-IP Redirection) ---\n")
	sb.WriteString(fmt.Sprintf(":do { /ip dns static remove [find name=\"login.zyranet.lan\"] } on-error={}\n"))
	sb.WriteString(fmt.Sprintf(":if ([:len [/ip hotspot profile find name=\"hsp-zyranet\"]] = 0) do={ /ip hotspot profile add name=hsp-zyranet hotspot-address=%s login-by=http-pap,http-chap split-user-domain=no dns-name=\"\" } else={ /ip hotspot profile set [find name=\"hsp-zyranet\"] hotspot-address=%s login-by=http-pap,http-chap split-user-domain=no dns-name=\"\" }\n", gatewayIP, gatewayIP))
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
	sb.WriteString(":do { /ip hotspot walled-garden remove [find dst-host~\"gstatic\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot walled-garden ip remove [find dst-host~\"gstatic\"] } on-error={}\n\n")

	// WAN NAT Masquerade
	sb.WriteString("# --- Firewall NAT (Internet Access Masquerade) ---\n")
	sb.WriteString(":if ([:len [/ip firewall nat find comment=\"Zyra Net Internet Access NAT\"]] = 0) do={ /ip firewall nat add chain=srcnat action=masquerade comment=\"Zyra Net Internet Access NAT\" }\n\n")

	// Auto-deploy Cloud Redirect login.html and redirect.html directly to the router's /hotspot directory
	sb.WriteString("# --- Auto-deploy Cloud Redirect login.html & redirect.html ---\n")
	sb.WriteString(fmt.Sprintf(":do { /tool fetch url=\"https://api.zyranet.co.ke/api/v1/public/zones/login-page/%d\" dst-path=\"hotspot/login.html\" mode=https } on-error={}\n", zone.ID))
	sb.WriteString(fmt.Sprintf(":do { /tool fetch url=\"https://api.zyranet.co.ke/api/v1/public/zones/login-page/%d\" dst-path=\"hotspot/redirect.html\" mode=https } on-error={}\n\n", zone.ID))

	// Scheduled heartbeat to report router online health every 1 minute
	sb.WriteString("# --- Live Health & Status Telemetry Heartbeat (1-Min Interval) ---\n")
	sb.WriteString(":do { /system script remove [find name=\"zyranet-heartbeat\"] } on-error={}\n")
	sb.WriteString(fmt.Sprintf(":do { /system script add name=zyranet-heartbeat source=\"/tool fetch url=\\\"https://api.zyranet.co.ke/api/v1/public/zones/sync/%d\\\" dst-path=\\\"zyra-sync.rsc\\\" mode=https; :if ([:len [/file find name=\\\"zyra-sync.rsc\\\"]] > 0) do={ /import file-name=zyra-sync.rsc; /file remove [find name=\\\"zyra-sync.rsc\\\"] }; /tool fetch url=\\\"https://api.zyranet.co.ke/api/v1/public/zones/heartbeat/%d\\\" mode=https keep-result=no\" comment=\"Zyra Net Cloud Sync & Telemetry\" } on-error={}\n", zone.ID, zone.ID))
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

	// Hotspot User Profiles & Instant Auto-Connect Users
	sb.WriteString("# --- Hotspot User Profiles & Auto-Connect Users ---\n")
	for _, pkg := range packages {
		if pkg.Type != "hotspot" {
			continue
		}
		profileName := sanitizeProfileName(pkg.Name)
		pkgTag := fmt.Sprintf("pkg-%d", pkg.ID)
		rateLimit := formatRateLimitWithBurst(pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
		timeout := "0s"
		if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
			timeout = fmt.Sprintf("%dm", *pkg.TimeLimitMinutes)
		}
		sharedUsers := 500
		if pkg.DeviceLimit > 1 {
			sharedUsers = 500 * pkg.DeviceLimit
		}
		sb.WriteString(fmt.Sprintf(
			":if ([:len [/ip hotspot user profile find name=\"%s\"]] = 0) do={ /ip hotspot user profile add name=\"%s\" rate-limit=\"%s\" session-timeout=\"%s\" idle-timeout=3m keepalive-timeout=1m shared-users=%d add-mac-cookie=no } else={ /ip hotspot user profile set [find name=\"%s\"] rate-limit=\"%s\" session-timeout=\"%s\" idle-timeout=3m keepalive-timeout=1m shared-users=%d add-mac-cookie=no }\n",
			profileName, profileName, rateLimit, timeout, sharedUsers, profileName, rateLimit, timeout, sharedUsers,
		))
		sb.WriteString(fmt.Sprintf(
			":if ([:len [/ip hotspot user find name=\"%s\"]] = 0) do={ /ip hotspot user add name=\"%s\" password=\"%s\" profile=\"%s\" comment=\"Zyra Net Package Auto-Connect\" } else={ /ip hotspot user set [find name=\"%s\"] password=\"%s\" profile=\"%s\" comment=\"Zyra Net Package Auto-Connect\" }\n",
			pkgTag, pkgTag, pkgTag, profileName, pkgTag, pkgTag, profileName,
		))
	}
	sb.WriteString("\n")

	// Free Tier Instant Auto-Connect User & Profile (with 20Mbps Burst, no MAC cookie bypass)
	sb.WriteString("# --- Free Tier Auto-Connect User & Profile (20Mbps Burst) ---\n")
	sb.WriteString(":if ([:len [/ip hotspot user profile find name=\"free-tier\"]] = 0) do={ /ip hotspot user profile add name=\"free-tier\" rate-limit=\"3M/3M 20M/20M 2M/2M 15/15 8\" session-timeout=30m idle-timeout=3m keepalive-timeout=1m shared-users=200 add-mac-cookie=no } else={ /ip hotspot user profile set [find name=\"free-tier\"] rate-limit=\"3M/3M 20M/20M 2M/2M 15/15 8\" session-timeout=30m idle-timeout=3m keepalive-timeout=1m shared-users=200 add-mac-cookie=no }\n")
	sb.WriteString(":if ([:len [/ip hotspot user find name=\"free\"]] = 0) do={ /ip hotspot user add name=\"free\" password=\"free\" profile=\"free-tier\" comment=\"Zyra Net Free Tier Auto-Connect\" } else={ /ip hotspot user set [find name=\"free\"] password=\"free\" profile=\"free-tier\" comment=\"Zyra Net Free Tier Auto-Connect\" }\n\n")

	// Hotspot Users (from vouchers)
	sb.WriteString("# --- Hotspot Users (Vouchers) ---\n")
	for _, v := range vouchers {
		if v.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(v.Package.Name)
		sb.WriteString(fmt.Sprintf(
			":if ([:len [/ip hotspot user find name=\"%s\"]] = 0) do={ /ip hotspot user add name=\"%s\" password=\"%s\" profile=\"%s\" comment=\"pkg:%s\" } else={ /ip hotspot user set [find name=\"%s\"] password=\"%s\" profile=\"%s\" comment=\"pkg:%s\" }\n",
			v.Code, v.Code, v.Code, profileName, profileName, v.Code, v.Code, profileName, profileName,
		))
	}
	sb.WriteString("\n")

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
			password = "password123"
		}
		sb.WriteString(fmt.Sprintf(
			":if ([:len [/ppp secret find name=\"%s\"]] = 0) do={ /ppp secret add name=\"%s\" password=\"%s\" service=pppoe profile=\"%s\" comment=\"customer_id:%d\" } else={ /ppp secret set [find name=\"%s\"] password=\"%s\" service=pppoe profile=\"%s\" comment=\"customer_id:%d\" }\n",
			username, username, password, profileName, c.ID, username, password, profileName, c.ID,
		))
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

// GenerateSyncScript generates dynamic RouterOS commands for 1-minute heartbeat sync.
// It ensures:
// 1. Hotspot profile login-by includes mac,http-pap,http-chap
// 2. All hotspot package user profiles and pkg-<id> auto-connect users exist
// 3. Free tier user profile and free user exist
// 4. All active paid customers are bypassed in IP-Binding (instant internet) AND added to Hotspot Users
// 5. Expired customer sessions and bindings are decommissioned
func (s *MikroTikScriptService) GenerateSyncScript(zoneID uint) (string, error) {
	var zone models.Zone
	if err := config.DB.First(&zone, zoneID).Error; err != nil {
		return "# Zone not found\n", fmt.Errorf("zone not found")
	}

	now := time.Now()

	var packages []models.Package
	config.DB.Where("(zone_id = ? OR zone_id = 0) AND status = ? AND type = ?", zone.ID, "active", "hotspot").Find(&packages)

	var activeCustomers []models.Customer
	config.DB.Preload("Package").
		Where("(zone_id = ? OR zone_id = 0 OR zone_id IN (SELECT id FROM zones WHERE organization_id = ?)) AND status = ? AND expires_at IS NOT NULL AND expires_at > ?", zone.ID, zone.OrganizationID, "active", now).
		Find(&activeCustomers)

	var expiredCustomers []models.Customer
	config.DB.Where("(zone_id = ? OR zone_id IN (SELECT id FROM zones WHERE organization_id = ?)) AND ((status = 'expired') OR (expires_at IS NOT NULL AND expires_at < ? AND status = 'active'))", zone.ID, zone.OrganizationID, now).
		Find(&expiredCustomers)

	var sb strings.Builder
	sb.WriteString("# Zyra Net Dynamic RouterOS Sync & Instant Activator\n")
	sb.WriteString(fmt.Sprintf("# Zone: %s (ID: %d) | Generated: %s\n\n", zone.Name, zone.ID, now.Format("2006-01-02 15:04:05")))

	// Ensure hotspot profile authentication methods
	sb.WriteString("# --- Hotspot Authentication Methods ---\n")
	sb.WriteString(":do { /ip hotspot profile set [find name=\"hsp-zyranet\"] login-by=mac,http-pap,http-chap split-user-domain=no } on-error={}\n")
	sb.WriteString(":do { /ip hotspot profile set [find name=\"default\"] login-by=mac,http-pap,http-chap split-user-domain=no } on-error={}\n\n")

	// Ensure package profiles & auto-connect users
	sb.WriteString("# --- Hotspot Package Profiles & Auto-Connect Users ---\n")
	for _, pkg := range packages {
		profileName := sanitizeProfileName(pkg.Name)
		pkgTag := fmt.Sprintf("pkg-%d", pkg.ID)
		rateLimit := formatRateLimitWithBurst(pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
		timeout := "0s"
		if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
			timeout = fmt.Sprintf("%dm", *pkg.TimeLimitMinutes)
		}
		sharedUsers := 500
		if pkg.DeviceLimit > 1 {
			sharedUsers = 500 * pkg.DeviceLimit
		}

		sb.WriteString(fmt.Sprintf(
			":if ([:len [/ip hotspot user profile find name=\"%s\"]] = 0) do={ /ip hotspot user profile add name=\"%s\" rate-limit=\"%s\" session-timeout=\"%s\" idle-timeout=3m keepalive-timeout=1m shared-users=%d add-mac-cookie=no } else={ /ip hotspot user profile set [find name=\"%s\"] rate-limit=\"%s\" session-timeout=\"%s\" idle-timeout=3m keepalive-timeout=1m shared-users=%d add-mac-cookie=no }\n",
			profileName, profileName, rateLimit, timeout, sharedUsers, profileName, rateLimit, timeout, sharedUsers,
		))

		sb.WriteString(fmt.Sprintf(
			":if ([:len [/ip hotspot user find name=\"%s\"]] = 0) do={ /ip hotspot user add name=\"%s\" password=\"%s\" profile=\"%s\" comment=\"Zyra Net Package Auto-Connect\" } else={ /ip hotspot user set [find name=\"%s\"] password=\"%s\" profile=\"%s\" comment=\"Zyra Net Package Auto-Connect\" }\n",
			pkgTag, pkgTag, pkgTag, profileName, pkgTag, pkgTag, profileName,
		))
	}

	// Free tier user & profile
	sb.WriteString(":if ([:len [/ip hotspot user profile find name=\"free-tier\"]] = 0) do={ /ip hotspot user profile add name=\"free-tier\" rate-limit=\"3M/3M 20M/20M 2M/2M 15/15 8\" session-timeout=30m idle-timeout=3m keepalive-timeout=1m shared-users=200 add-mac-cookie=no } else={ /ip hotspot user profile set [find name=\"free-tier\"] rate-limit=\"3M/3M 20M/20M 2M/2M 15/15 8\" session-timeout=30m idle-timeout=3m keepalive-timeout=1m shared-users=200 add-mac-cookie=no }\n")
	sb.WriteString(":if ([:len [/ip hotspot user find name=\"free\"]] = 0) do={ /ip hotspot user add name=\"free\" password=\"free\" profile=\"free-tier\" comment=\"Zyra Net Free Tier Auto-Connect\" } else={ /ip hotspot user set [find name=\"free\"] password=\"free\" profile=\"free-tier\" comment=\"Zyra Net Free Tier Auto-Connect\" }\n\n")

	// Collect all active MAC bindings
	activeBinds := make(map[string]string) // mac -> profileName
	for _, cust := range activeCustomers {
		profileName := "default"
		if cust.Package != nil {
			profileName = sanitizeProfileName(cust.Package.Name)
		}
		if cust.MacAddress != nil && *cust.MacAddress != "" {
			mac := strings.ToUpper(strings.TrimSpace(*cust.MacAddress))
			mac = strings.ReplaceAll(mac, "-", ":")
			if len(mac) == 17 {
				activeBinds[mac] = profileName
			}
		}

		var devices []models.CustomerDevice
		config.DB.Where("customer_id = ?", cust.ID).Find(&devices)
		for _, dev := range devices {
			mac := strings.ToUpper(strings.TrimSpace(dev.MacAddress))
			mac = strings.ReplaceAll(mac, "-", ":")
			if len(mac) == 17 {
				activeBinds[mac] = profileName
			}
		}
	}

	// Also check recent completed payments with valid unexpired packages
	var recentPayments []models.Payment
	config.DB.Preload("Package").
		Where("(zone_id = ? OR zone_id IN (SELECT id FROM zones WHERE organization_id = ?)) AND status = 'completed' AND mac_address != '' AND created_at > ?", zone.ID, zone.OrganizationID, now.Add(-30*24*time.Hour)).
		Find(&recentPayments)

	for _, p := range recentPayments {
		if p.Package != nil {
			expiry := utils.CalculateExpiry(p.Package.BillingCycle, &p.CreatedAt, p.Package.TimeLimitMinutes)
			if expiry.After(now) {
				mac := strings.ToUpper(strings.TrimSpace(p.MacAddress))
				mac = strings.ReplaceAll(mac, "-", ":")
				if len(mac) == 17 {
					activeBinds[mac] = sanitizeProfileName(p.Package.Name)
				}
			}
		}
	}

	// Clean up any bypassed bindings so all devices must select/authenticate a plan
	sb.WriteString("# --- Revert All Devices to Captive Portal Plans (Remove Bypasses) ---\n")
	sb.WriteString(":do { /ip hotspot ip-binding remove [find comment~\"ZyraNet\"] } on-error={}\n")
	sb.WriteString(":do { /ip hotspot ip-binding remove [find type=bypassed] } on-error={}\n\n")

	// Active paid customers: authenticate through hotspot user profiles with bandwidth limits
	sb.WriteString("# --- Hotspot Plan Users (Controlled by Package Profiles) ---\n")
	for mac, profileName := range activeBinds {
		sb.WriteString(fmt.Sprintf(":do { /ip hotspot user remove [find name=\"%s\"] } on-error={}\n", mac))
		sb.WriteString(fmt.Sprintf(":do { /ip hotspot user add name=\"%s\" password=\"%s\" profile=\"%s\" comment=\"ZyraNet Paid Plan\" } on-error={}\n", mac, mac, profileName))
	}
	sb.WriteString("\n")

	// Expired customers: decommission
	sb.WriteString("# --- Expired Customer Sessions Decommissioner ---\n")
	for _, cust := range expiredCustomers {
		config.DB.Model(&cust).Update("status", "expired")
		if cust.MacAddress != nil && *cust.MacAddress != "" {
			mac := strings.ToUpper(strings.TrimSpace(*cust.MacAddress))
			mac = strings.ReplaceAll(mac, "-", ":")
			if len(mac) == 17 {
				if _, stillActive := activeBinds[mac]; !stillActive {
					sb.WriteString(fmt.Sprintf(":do { /ip hotspot ip-binding remove [find mac-address=\"%s\"] } on-error={}\n", mac))
					sb.WriteString(fmt.Sprintf(":do { /ip hotspot active remove [find mac-address=\"%s\"] } on-error={}\n", mac))
					sb.WriteString(fmt.Sprintf(":do { /ip hotspot host remove [find mac-address=\"%s\"] } on-error={}\n", mac))
					sb.WriteString(fmt.Sprintf(":do { /ip hotspot cookie remove [find mac-address=\"%s\"] } on-error={}\n", mac))
					sb.WriteString(fmt.Sprintf(":do { /ip hotspot user remove [find name=\"%s\"] } on-error={}\n", mac))
				}
			}
		}
	}

	return sb.String(), nil
}
