package services

import (
	"fmt"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
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

	sb.WriteString("# --- Bridge & LAN Ports Configuration ---\n")
	sb.WriteString("/interface bridge add name=bridge-hotspot comment=\"Zyra Net Hotspot Bridge\" disabled=no\n")
	portsList := strings.Split(lanPorts, ",")
	for _, port := range portsList {
		port = strings.TrimSpace(port)
		if port != "" {
			sb.WriteString(fmt.Sprintf("/interface bridge port add bridge=bridge-hotspot interface=%s\n", port))
		}
	}
	sb.WriteString("\n")

	sb.WriteString("# --- IP Address & Gateway Configuration ---\n")
	sb.WriteString(fmt.Sprintf("/ip address add address=%s interface=bridge-hotspot comment=\"Zyra Net Hotspot Gateway\"\n\n", hotspotAddr))

	sb.WriteString("# --- IP Pool & DHCP Server Configuration ---\n")
	sb.WriteString(fmt.Sprintf("/ip pool add name=hs-pool-zyranet ranges=%s\n", ipPoolRange))
	sb.WriteString("/ip dhcp-server add name=hs-dhcp-zyranet interface=bridge-hotspot address-pool=hs-pool-zyranet disabled=no lease-time=1h\n")
	sb.WriteString(fmt.Sprintf("/ip dhcp-server network add address=%s gateway=%s dns-server=8.8.8.8,8.8.4.4 comment=\"Zyra Net Hotspot Network\"\n\n", networkCIDR, gatewayIP))

	sb.WriteString("# --- Hotspot Server Setup (Overload-Protected) ---\n")
	sb.WriteString(fmt.Sprintf("/ip hotspot profile add name=hsp-zyranet hotspot-address=%s login-by=http-chap,cookie,mac-cookie mac-cookie-timeout=1d split-user-domain=no dns-name=login.zyranet.lan\n", gatewayIP))
	sb.WriteString("/ip hotspot add name=hs-zyranet interface=bridge-hotspot address-pool=hs-pool-zyranet profile=hsp-zyranet idle-timeout=3m keepalive-timeout=1m disabled=no\n\n")

	// Allow the cloud captive portal, API, M-Pesa endpoints, and CDN through walled garden
	sb.WriteString("# --- Walled Garden: allow cloud captive portal, API, M-Pesa, and assets ---\n")
	sb.WriteString("/ip hotspot walled-garden add dst-host=*zyranet.co.ke action=allow comment=\"Zyra Net Cloud\"\n")
	sb.WriteString("/ip hotspot walled-garden add dst-host=*safaricom.co.ke action=allow comment=\"Safaricom Daraja\"\n")
	sb.WriteString("/ip hotspot walled-garden add dst-host=*googleapis.com action=allow comment=\"Google Fonts\"\n")
	sb.WriteString("/ip hotspot walled-garden add dst-host=*gstatic.com action=allow comment=\"Google Static\"\n\n")

	// WAN NAT Masquerade
	sb.WriteString("# --- Firewall NAT (Internet Access Masquerade) ---\n")
	sb.WriteString("/ip firewall nat add chain=srcnat action=masquerade comment=\"Zyra Net Internet Access NAT\"\n\n")

	// Auto-fetch login.html directly to the router's /hotspot directory so manual file upload is not needed
	sb.WriteString("# --- Auto-deploy Cloud Redirect login.html ---\n")
	sb.WriteString(fmt.Sprintf("/tool fetch url=\"https://api.zyranet.co.ke/api/v1/public/zones/login-page/%d\" dst-path=\"hotspot/login.html\" mode=https\n\n", zone.ID))

	// Scheduled heartbeat to report router online health every 1 minute
	sb.WriteString("# --- Live Health & Status Telemetry Heartbeat (1-Min Interval) ---\n")
	sb.WriteString(fmt.Sprintf("/system script add name=zyranet-heartbeat source=\"/tool fetch url=\\\"https://api.zyranet.co.ke/api/v1/public/zones/heartbeat/%d\\\" mode=https keep-result=no\" comment=\"Zyra Net Cloud Telemetry\"\n", zone.ID))
	sb.WriteString("/system scheduler add name=zyranet-heartbeat-sched interval=1m on-event=zyranet-heartbeat comment=\"Zyra Net Cloud Telemetry Scheduler\"\n\n")

	// Automated Ghost Device Garbage Collector & Memory Protection
	sb.WriteString("# --- Automated Garbage Collection & Memory Overload Protection ---\n")
	sb.WriteString("/system script add name=zyranet-gc source=\"/ip hotspot host remove [find unauthorized=yes idle-time>5m]; /ip hotspot cookie remove [find expires-in<0s]\" comment=\"Zyra Net Ghost Device GC\"\n")
	sb.WriteString("/system scheduler add name=zyranet-gc-sched interval=5m on-event=zyranet-gc comment=\"Zyra Net GC Scheduler\"\n\n")
	sb.WriteString("/system script add name=zyranet-memprotect source=\":if ([/system resource get cpu-load] > 85) do={ /ip dns cache flush; /ip hotspot host remove [find unauthorized=yes] }\" comment=\"Zyra Net CPU & Memory Overload Guard\"\n")
	sb.WriteString("/system scheduler add name=zyranet-memprotect-sched interval=2m on-event=zyranet-memprotect comment=\"Zyra Net Overload Scheduler\"\n\n")

	// Hotspot Profiles (Strict shared-users=1 to prevent tethering/reselling)
	sb.WriteString("# --- Hotspot User Profiles (Anti-Tethering & Speed Queues) ---\n")
	for _, pkg := range packages {
		if pkg.Type != "hotspot" {
			continue
		}
		profileName := sanitizeProfileName(pkg.Name)
		rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
		timeout := "0s"
		if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
			timeout = fmt.Sprintf("%dm", *pkg.TimeLimitMinutes)
		}
		sb.WriteString(fmt.Sprintf(
			"/ip hotspot user profile\nadd name=%s rate-limit=%s session-timeout=%s idle-timeout=3m keepalive-timeout=1m shared-users=1\n\n",
			profileName, rateLimit, timeout,
		))
	}

	// Hotspot Users (from vouchers)
	sb.WriteString("# --- Hotspot Users (Vouchers) ---\n")
	for _, v := range vouchers {
		if v.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(v.Package.Name)
		sb.WriteString(fmt.Sprintf(
			"/ip hotspot user\nadd name=%s password=%s profile=%s comment=\"pkg:%s\"\n",
			v.Code, v.Code, profileName, profileName,
		))
	}
	sb.WriteString("\n")

	// PPPoE Pool & Profiles
	sb.WriteString("# --- PPPoE Remote IP Pool & Profiles ---\n")
	sb.WriteString("/ip pool add name=pool-pppoe-zyranet ranges=10.10.0.2-10.10.3.254\n")
	for _, pkg := range packages {
		if pkg.Type != "pppoe" {
			continue
		}
		profileName := sanitizeProfileName(pkg.Name)
		rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
		sb.WriteString(fmt.Sprintf(
			"/ppp profile\nadd name=%s rate-limit=%s local-address=10.10.0.1 remote-address=pool-pppoe-zyranet dns-server=8.8.8.8,8.8.4.4\n\n",
			profileName, rateLimit,
		))
	}

	// PPPoE Server Setup
	sb.WriteString("# --- PPPoE Server Setup ---\n")
	sb.WriteString("/interface pppoe-server server add service-name=pppoe-zyranet interface=bridge-hotspot default-profile=default authentication=pap,chap disabled=no\n\n")

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
			"/ppp secret\nadd name=%s password=%s service=pppoe profile=%s comment=\"customer_id:%d\"\n",
			username, password, profileName, c.ID,
		))
	}

	filename := fmt.Sprintf("zone-%s-%s.rsc",
		strings.ReplaceAll(strings.ToLower(zone.Name), " ", "-"),
		time.Now().Format("20060102150405"),
	)

	return sb.String(), filename, nil
}
