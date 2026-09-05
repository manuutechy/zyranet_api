package services

import (
	"strings"
	"testing"

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
		`:if ([:len [/interface bridge find name="bridge-hotspot"]] = 0) do={ /interface bridge add name=bridge-hotspot comment="Zyra Net Hotspot Bridge" disabled=no }`,
		`:do { /interface bridge port remove [find interface="ether2"] } on-error={}`,
		`:do { /interface bridge port add bridge=bridge-hotspot interface="ether2" } on-error={}`,
		`:do { /interface bridge port remove [find interface="ether3"] } on-error={}`,
		`:do { /interface bridge port add bridge=bridge-hotspot interface="ether3" } on-error={}`,
		`:do { /ip address remove [find comment="Zyra Net Hotspot Gateway"] } on-error={}`,
		`:do { /ip address add address=10.5.50.1/24 interface=bridge-hotspot comment="Zyra Net Hotspot Gateway" } on-error={}`,
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
