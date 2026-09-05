package services

import (
	"strings"
	"testing"
	"time"

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
		`:do { /interface wireless disable [find default-name=wlan1] } on-error={}`,
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
	db := setupTestDB(t)

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
		Name:          "John Doe",
		Phone:         "254712345678",
		ZoneID:        zone.ID,
		Type:          "hotspot",
		Status:        "active",
		PackageID:     pkg.ID,
		MacAddress:    &mac,
		ExpiresAt:     &expiry,
	}
	db.Create(&cust)

	svc := NewMikroTikScriptService()
	script, err := svc.GenerateSyncScript(zone.ID)
	if err != nil {
		t.Fatalf("GenerateSyncScript failed: %v", err)
	}

	if !strings.Contains(script, "00:11:22:33:44:55") {
		t.Errorf("expected script to contain customer MAC 00:11:22:33:44:55, got:\n%s", script)
	}
	if !strings.Contains(script, "type=bypassed") {
		t.Errorf("expected script to contain type=bypassed, got:\n%s", script)
	}
	if !strings.Contains(script, "login-by=mac,http-pap,http-chap") {
		t.Errorf("expected script to contain login-by=mac,http-pap,http-chap, got:\n%s", script)
	}
}
