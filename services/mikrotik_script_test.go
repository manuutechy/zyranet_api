package services

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func TestGenerateScript_IdempotencyAndBridgePortRemoval(t *testing.T) {
	db := setupTestDB(t)

	zone := models.Zone{
		Name:           "Test Zone",
		Location:       "Test Location",
		LanPorts:       "ether2,ether3",
		HotspotAddress: "10.5.50.1/24",
	}
	if err := db.Create(&zone).Error; err != nil {
		t.Fatalf("failed to create zone: %v", err)
	}

	pkg := models.Package{
		ZoneID:            zone.ID,
		Name:              "1 Hour Pass",
		Type:              "hotspot",
		Status:            "active",
		SpeedUploadKbps:   5000,
		SpeedDownloadKbps: 10000,
	}
	db.Create(&pkg)

	svc := NewMikroTikScriptService()
	script, filename, err := svc.GenerateScript(zone.ID)
	if err != nil {
		t.Fatalf("GenerateScript failed: %v", err)
	}

	if filename == "" {
		t.Errorf("expected non-empty filename")
	}

	expectedSnippets := []string{
		`:local br "bridge-hotspot";`,
		`:if ([:len [/interface bridge find name="bridge-hotspot"]] = 0) do={`,
		`:do { /interface wireless disable [find] } on-error={}`,
		`:do { /interface wifi disable [find] } on-error={}`,
		`:do { /ip address add address=10.5.50.1/24 interface=$br comment="Zyra Net Hotspot Gateway" } on-error={}`,
		`hs-pool-zyranet`,
		`hs-dhcp-zyranet`,
		`hsp-zyranet`,
		`hs-zyranet`,
		`zyranet-heartbeat`,
		`zyranet-gc`,
		`zyranet-memprotect`,
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(script, snippet) {
			t.Errorf("script missing expected snippet:\n%s\nFull script:\n%s", snippet, script)
		}
	}
}

func TestGetStatus_HeartbeatTelemetryFallback(t *testing.T) {
	db := setupTestDB(t)

	now := time.Now()
	zone := models.Zone{
		Name:           "Telemetry Zone",
		Location:       "Nairobi",
		RouterName:     "RB951Ui-2HnD",
		RouterIP:       "10.100.0.1",
		LastSeenAt:     &now,
		LastStatus:     "online",
		ConnectionType: "api",
	}
	if err := db.Create(&zone).Error; err != nil {
		t.Fatalf("failed to create zone: %v", err)
	}

	stat := models.ZoneStat{
		ZoneID:           zone.ID,
		CPULoad:          14,
		MemoryUsedMB:     45,
		MemoryTotalMB:    128,
		ConnectedClients: 3,
		RecordedAt:       now,
	}
	db.Create(&stat)

	svc := NewMikroTikService()
	status, err := svc.GetStatus(&zone)
	if err != nil {
		t.Fatalf("GetStatus unexpected error: %v", err)
	}
	if status == nil || !status.Online {
		t.Fatalf("expected status.Online to be true from heartbeat telemetry, got: %+v", status)
	}
	if status.CPULoad != 14 {
		t.Errorf("expected CPULoad 14, got %d", status.CPULoad)
	}
	if status.ConnectedClients != 3 {
		t.Errorf("expected ConnectedClients 3, got %d", status.ConnectedClients)
	}
	if status.BoardName != "RB951Ui-2HnD" {
		t.Errorf("expected BoardName RB951Ui-2HnD, got %s", status.BoardName)
	}
}

func TestGenerateSyncScript(t *testing.T) {
	db := setupRouterAccessDB(t)

	now := time.Now()
	expiry := now.Add(24 * time.Hour)
	zone := models.Zone{
		Name:           "Sync Zone",
		Location:       "Nairobi",
		OrganizationID: 1,
	}
	db.Create(&zone)

	pkg := models.Package{
		ZoneID:            zone.ID,
		Name:              "Daily Fast",
		Type:              "hotspot",
		Status:            "active",
		SpeedUploadKbps:   5000,
		SpeedDownloadKbps: 10000,
	}
	db.Create(&pkg)

	mac := "00:11:22:33:44:55"
	cust := models.Customer{
		Name:       "John Doe",
		Phone:      "254712345678",
		ZoneID:     zone.ID,
		Type:       "hotspot",
		Status:     "active",
		PackageID:  pkg.ID,
		MacAddress: &mac,
		ExpiresAt:  &expiry,
	}
	db.Create(&cust)

	// The watchdog's reconciler gives the paid device its own login.
	ReconcileCredentials(time.Time{})

	svc := NewMikroTikScriptService()
	script, err := svc.GenerateSyncScript(zone.ID)
	if err != nil {
		t.Fatalf("GenerateSyncScript failed: %v", err)
	}

	if !strings.Contains(script, "mac-address=00:11:22:33:44:55") {
		t.Errorf("expected a login bound to the customer MAC 00:11:22:33:44:55, got:\n%s", script)
	}
	if !strings.Contains(script, "remove [find type=bypassed]") {
		t.Errorf("expected script to contain remove bypassed bindings, got:\n%s", script)
	}
	if !strings.Contains(script, "login-by=mac,http-pap,http-chap") {
		t.Errorf("expected script to contain login-by=mac,http-pap,http-chap, got:\n%s", script)
	}
}

func scriptFor(t *testing.T, z models.Zone) (full, sync string) {
	t.Helper()
	setupRouterAccessDB(t)
	z.Name, z.Location, z.RouterName, z.RouterIP = "Maseno", "L", "r", "10.200.0.2"
	if err := config.DB.Create(&z).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewMikroTikScriptService()
	full, _, err := svc.GenerateScript(z.ID)
	if err != nil {
		t.Fatal(err)
	}
	sync, _ = svc.GenerateSyncScript(z.ID)
	return full, sync
}

func TestGenerateScript_ServiceModes(t *testing.T) {
	hs := "/ip hotspot add name=hs-zyranet"
	dhcp := "hs-dhcp-zyranet"
	ppp := "/interface pppoe-server server add"
	cases := []struct {
		mode           string
		hotspot, pppoe bool
	}{{"hotspot", true, false}, {"pppoe", false, true}, {"both", true, true}, {"", true, true}}
	for _, c := range cases {
		full, sync := scriptFor(t, models.Zone{ServiceMode: c.mode})
		if got := strings.Contains(full, hs) && strings.Contains(full, dhcp); got != c.hotspot {
			t.Errorf("mode %q: hotspot configured = %v, want %v", c.mode, got, c.hotspot)
		}
		if got := strings.Contains(full, ppp); got != c.pppoe {
			t.Errorf("mode %q: PPPoE configured = %v, want %v", c.mode, got, c.pppoe)
		}
		if got := strings.Contains(sync, "Hotspot Authentication Methods"); got != c.hotspot {
			t.Errorf("mode %q: sync touches hotspot = %v, want %v", c.mode, got, c.hotspot)
		}
		// Every mode keeps the heartbeat, NAT and management lock-down.
		for _, must := range []string{"zyranet-heartbeat", "Zyra Net Internet Access NAT", "Zyra Net Remote Access"} {
			if !strings.Contains(full, must) {
				t.Errorf("mode %q: missing %q", c.mode, must)
			}
		}
	}
}

func TestGenerateScript_PortsAndWifi(t *testing.T) {
	full, sync := scriptFor(t, models.Zone{WanPort: "sfp-sfpplus1", LanPorts: "ether1,ether2,wifi1,wlan1", ServiceMode: "hotspot"})
	for _, want := range []string{
		"/interface bridge port add bridge=$br interface=ether1",
		`/interface wifi set [find default-name=wifi1] configuration.ssid="Zyra Net WiFi"`,
		`/interface wireless set [find default-name=wlan1] ssid="Zyra Net WiFi"`,
	} {
		if !strings.Contains(full, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// Management is locked to the tunnel by source address, whatever the ports.
	for _, sc := range []string{full, sync} {
		if !strings.Contains(sc, "src-address-list=zyranet-mgmt action=accept") || !strings.Contains(sc, "src-address-list=!zyranet-mgmt action=drop") {
			t.Error("management must only be reachable from the Zyra Net tunnel")
		}
	}
	if strings.Contains(full, "in-interface=ether1 ") {
		t.Error("ether1 is a customer port here and must not be treated as the uplink")
	}

	// A malicious or mistyped port list never reaches the router.
	full, _ = scriptFor(t, models.Zone{LanPorts: "ether2; /system reset"})
	if strings.Contains(full, "/system reset") || !strings.Contains(full, "interface=ether2 }") {
		t.Error("invalid port input must fall back to the safe defaults")
	}
}

// When the router can't be dialled, live traffic must say "unavailable" with
// zeros — never an estimate derived from customer counts.
func TestGetInterfaceTraffic_UnreachableIsNotInvented(t *testing.T) {
	db := setupTestDB(t)
	zone := models.Zone{Name: "NAT Zone", Location: "Maseno", RouterName: "hAP", RouterIP: "10.100.0.9", ConnectionType: "api"}
	if err := db.Create(&zone).Error; err != nil {
		t.Fatalf("create zone: %v", err)
	}
	future := time.Now().Add(time.Hour)
	for i := 0; i < 5; i++ {
		db.Create(&models.Customer{Name: "c", Phone: fmt.Sprintf("25470000%04d", i), ZoneID: zone.ID, Status: "active", ExpiresAt: &future})
	}

	tr, err := NewMikroTikService().GetInterfaceTraffic(&zone, "ether1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.Available || tr.RxBps != 0 || tr.TxBps != 0 || tr.RxMbps != 0 || tr.TxMbps != 0 {
		t.Fatalf("expected unavailable with zero traffic, got %+v", tr)
	}
	if tr.Message == "" {
		t.Fatalf("expected an explanation message, got %+v", tr)
	}
}

func TestWifiKeepAliveBlock(t *testing.T) {
	var sb strings.Builder
	z := &models.Zone{ID: 10, WanPort: "ether1", LanPorts: "ether2,wlan1", ProvisionToken: "tok123"}
	writeWifiKeepAlive(&sb, z)
	out := sb.String()
	for _, want := range []string{
		`[/interface wireless find default-name=wlan1]`,
		`/interface wireless set $id ssid="Zyra Net WiFi"`,
		`/interface bridge port add bridge=bridge-hotspot interface=wlan1`,
		`wifi-report/10?token=tok123&radio=wlan1&`,
		`mode=https keep-result=no`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WLAN") || strings.Contains(out, "TOKEN") || strings.Contains(out, "FETCHSEC") {
		t.Errorf("placeholder left:\n%s", out)
	}
	if strings.Count(out, "{") != strings.Count(out, "}") {
		t.Error("unbalanced braces")
	}
	sb.Reset()
	writeWifiKeepAlive(&sb, &models.Zone{ID: 1, WanPort: "ether1", LanPorts: "ether2,ether3", ProvisionToken: "x"})
	if sb.Len() != 0 {
		t.Error("no radio → no block")
	}
}

func TestWriteTunnel(t *testing.T) {
	ip, u, pw := "10.200.0.10", "zyranet-api", "Abcdef123456"
	z := &models.Zone{ID: 10, ProvisionToken: "tok", TunnelIP: &ip, RouterUsername: &u, RouterPassword: &pw}
	var sb strings.Builder
	writeTunnel(&sb, z)
	out := sb.String()
	for _, want := range []string{
		`/interface wireguard add name=zyra-wg`,
		`/ip address add address=10.200.0.10/24 interface=zyra-wg`,
		`endpoint-address=159.195.109.170 endpoint-port=51820 allowed-address=10.200.0.1/32`,
		`/user add name=zyranet-api group=zyranet-api password="Abcdef123456" address=10.200.0.1/32`,
		`wg-key/10?token=tok&ip=10.200.0.10&pub=`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"TUNIP", "SRVPUB", "SRVEND", "APIPW", "FETCHSEC", "TOKEN"} {
		if strings.Contains(out, bad) {
			t.Errorf("placeholder %s left", bad)
		}
	}
	if strings.Count(out, "{") != strings.Count(out, "}") {
		t.Error("unbalanced braces")
	}
	// No tunnel IP, or one outside the tunnel subnet → nothing.
	for _, tip := range []*string{nil, strp("192.168.1.5"), strp("10.200.0.1")} {
		sb.Reset()
		writeTunnel(&sb, &models.Zone{ID: 1, ProvisionToken: "x", TunnelIP: tip})
		if sb.Len() != 0 {
			t.Errorf("tunnel %v: expected nothing", tip)
		}
	}
	// Other username → tunnel only, no router user created.
	other := "admin"
	sb.Reset()
	writeTunnel(&sb, &models.Zone{ID: 10, ProvisionToken: "tok", TunnelIP: &ip, RouterUsername: &other, RouterPassword: &pw})
	if strings.Contains(sb.String(), "/user add") {
		t.Error("must only create the zyranet-api user")
	}
}
func strp(s string) *string { return &s }
