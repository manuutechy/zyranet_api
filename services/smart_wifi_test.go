package services

import (
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func balanced(t *testing.T, label, s string) {
	t.Helper()
	if strings.Count(s, "{") != strings.Count(s, "}") || strings.Count(s, "[") != strings.Count(s, "]") {
		t.Errorf("%s: unbalanced braces/brackets:\n%s", label, s)
	}
}

func TestZoneRadios(t *testing.T) {
	cases := []struct {
		lan          string
		legacy, wifi bool
	}{
		{"ether2,wlan1", true, false},
		{"ether2,wifi1", false, true},
		{"ether2,ether3", false, false},
	}
	for _, c := range cases {
		l, w := zoneRadios(&models.Zone{WanPort: "ether1", LanPorts: c.lan})
		if l != c.legacy || w != c.wifi {
			t.Errorf("%s: got legacy=%v wifi=%v", c.lan, l, w)
		}
	}
}

// Only the WiFi package the router has is ever named: an unknown menu would
// be a syntax error that stops the whole sync.
func TestKickCommands_OnlyKnownRadioMenus(t *testing.T) {
	legacy := strings.Join(kickCommands(&models.Zone{WanPort: "ether1", LanPorts: "ether2,wlan1"}, "AA:BB:CC:DD:EE:FF"), "\n")
	if !strings.Contains(legacy, "/interface wireless registration-table remove [find mac-address=AA:BB:CC:DD:EE:FF]") || strings.Contains(legacy, "/interface wifi ") {
		t.Errorf("legacy radio:\n%s", legacy)
	}
	wired := strings.Join(kickCommands(&models.Zone{WanPort: "ether1", LanPorts: "ether2"}, "$m"), "\n")
	if strings.Contains(wired, "registration-table") || !strings.Contains(wired, "/ip hotspot host remove [find mac-address=$m]") {
		t.Errorf("wired only:\n%s", wired)
	}
}

func TestProfileExtras_EscapedLogoutScript(t *testing.T) {
	var sb strings.Builder
	writeProfileExtras(&sb, &models.Zone{WanPort: "ether1", LanPorts: "ether2,wlan1"}, "Daily_Fast")
	out := sb.String()
	for _, want := range []string{
		`[find name="Daily_Fast"] add-mac-cookie=yes mac-cookie-timeout=3d on-logout="`,
		`:local m \$\"mac-address\"`, // $ escaped: expanded at logout, not at sync
		`/interface wireless registration-table remove [find mac-address=\$m]`,
		`:do { :local lu [/ip hotspot user get \$id limit-uptime]`,
		`:do { :local lb [/ip hotspot user get \$id limit-bytes-total]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// No unescaped $ inside the quoted on-logout value.
	val := out[strings.Index(out, `on-logout="`)+len(`on-logout="`):]
	for i := 0; i < len(val); i++ {
		if val[i] == '$' && (i == 0 || val[i-1] != '\\') {
			t.Fatalf("unescaped $ at %d in on-logout: %s", i, val)
		}
	}
	balanced(t, "extras", out)
	if !strings.HasPrefix(out, ":do {") || !strings.HasSuffix(strings.TrimSpace(out), "on-error={}") {
		t.Error("extras must be on their own guarded line")
	}
}

func TestEndedSince_KickOnce(t *testing.T) {
	now := time.Now()
	last := now.Add(-time.Minute)
	expired := models.HotspotCredential{Status: "active", ExpiresAt: now.Add(-30 * time.Second), UpdatedAt: now.Add(-time.Hour)}
	if !endedSince(&expired, &last, now) {
		t.Error("expired since last sync → kick")
	}
	old := models.HotspotCredential{Status: "active", ExpiresAt: now.Add(-3 * time.Minute), UpdatedAt: now.Add(-time.Hour)}
	if endedSince(&old, &last, now) {
		t.Error("ended before the last sync → already kicked, not again")
	}
	retired := models.HotspotCredential{Status: "revoked", ExpiresAt: now.Add(time.Hour), UpdatedAt: now.Add(-10 * time.Second)}
	if !endedSince(&retired, &last, now) {
		t.Error("retired since last sync → kick")
	}
	if !endedSince(&expired, nil, now) || endedSince(&old, nil, now) {
		t.Error("never synced → only the last 2 minutes")
	}
}

func TestSmartSpeed_OffWithoutLineSpeed(t *testing.T) {
	var sb strings.Builder
	writeSmartSpeed(&sb, &models.Zone{SmartSpeed: true, WanPort: "ether1", LanPorts: "ether2"})
	out := sb.String()
	if strings.Contains(out, "queue tree add") || !strings.Contains(out, `/queue tree remove [find comment~"^zyra-smart"]`) {
		t.Errorf("no line speed → only the clean-up:\n%s", out)
	}
	balanced(t, "off", out)
	if on, why := SmartSpeedState(&models.Zone{SmartSpeed: true}); on || !strings.Contains(why, "line speed") {
		t.Errorf("state: %v %q", on, why)
	}
}

func TestSmartSpeed_Lanes(t *testing.T) {
	var sb strings.Builder
	writeSmartSpeed(&sb, &models.Zone{SmartSpeed: true, UplinkDownMbps: 20, UplinkUpMbps: 10, WanPort: "ether1", LanPorts: "ether2,wlan1"})
	out := sb.String()
	for _, want := range []string{
		`:if ([:len [/queue tree find comment="zyra-smart v1 d=18400 u=9200 wan=ether1"]] = 0) do={`,
		`protocol=udp port=3478-3481,8801-8810 new-packet-mark=zq-rt`,
		`protocol=udp src-port=!443 dst-port=!443 packet-size=0-600 new-packet-mark=zq-rt`,
		`connection-bytes=0-2000000 new-packet-mark=zq-quick`,
		`/queue tree add name=zq-down parent=bridge-hotspot max-limit=18400k`,
		`/queue tree add name=zq-up parent=$wan max-limit=9200k`,
		`name=zq-down-rt parent=zq-down packet-mark=zq-rt priority=1 limit-at=3680k max-limit=18400k`,
		`name=zq-up-heavy parent=zq-up packet-mark=zq-heavy priority=7 limit-at=920k max-limit=9200k queue=zq-sfq comment="zyra-smart v1 d=18400 u=9200 wan=ether1"`,
		`on-error={ :log warning "zyra: smart speed could not be applied" }`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Catch-all last, after every specific lane.
	if strings.Index(out, "comment=\"zyra-smart\" new-packet-mark=zq-heavy") < strings.Index(out, "zq-quick") {
		t.Error("heavy catch-all must come after the quick lane")
	}
	balanced(t, "on", out)
	sb.Reset()
	writeSmartSpeed(&sb, &models.Zone{SmartSpeed: false, UplinkDownMbps: 20, UplinkUpMbps: 10})
	if strings.Contains(sb.String(), "queue tree add") {
		t.Error("switched off → no lanes")
	}
}

// End to end through the sync: cookie, kick once on expiry, lanes.
func TestSyncScript_SmartWifi(t *testing.T) {
	db := setupRouterAccessDB(t)
	now := time.Now()
	last := now.Add(-time.Minute)
	zone := models.Zone{Name: "Oxygen", Location: "L", OrganizationID: 1, WanPort: "ether1", LanPorts: "ether2,wlan1",
		UplinkDownMbps: 20, UplinkUpMbps: 10, LastSyncAt: &last}
	db.Create(&zone)
	var z models.Zone
	db.First(&z, zone.ID)
	if !z.SmartSpeed {
		t.Fatal("smart speed should default on")
	}
	pkg := models.Package{ZoneID: zone.ID, Name: "1 Hour", Type: "hotspot", Status: "active", SpeedUploadKbps: 1024, SpeedDownloadKbps: 2048}
	db.Create(&pkg)
	ended := models.HotspotCredential{OrganizationID: 1, CustomerID: 1, ZoneID: zone.ID, PackageID: pkg.ID, Username: "znabcdefgh", Password: "Secret123",
		MacAddress: "AA:BB:CC:DD:EE:01", Status: "active", ExpiresAt: now.Add(-20 * time.Second)}
	if err := config.DB.Create(&ended).Error; err != nil {
		t.Fatal(err)
	}
	script, err := NewMikroTikScriptService().GenerateSyncScript(zone.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`login-by=mac,mac-cookie,http-pap,http-chap`,
		`[find name="1_Hour"] add-mac-cookie=yes`,
		`:if ([:len [/ip hotspot user find mac-address=AA:BB:CC:DD:EE:01]] = 0) do={ :do { /ip hotspot host remove [find mac-address=AA:BB:CC:DD:EE:01] } on-error={}; :do { /interface wireless registration-table remove [find mac-address=AA:BB:CC:DD:EE:01] } on-error={} }`,
		`zyra-smart v1 d=18400 u=9200 wan=ether1`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in sync:\n%s", want, script)
		}
	}
	if strings.Contains(script, "add-mac-cookie=no") {
		t.Error("profile line must not switch the cookie back off every minute")
	}
	// Next minute: already kicked, the login stays in the clean-up list but no second kick.
	later := now.Add(30 * time.Second)
	db.Model(&models.Zone{}).Where("id = ?", zone.ID).UpdateColumn("last_sync_at", later)
	script2, _ := NewMikroTikScriptService().GenerateSyncScript(zone.ID)
	if strings.Contains(script2, "registration-table remove [find mac-address=AA:BB:CC:DD:EE:01]") {
		t.Error("device kicked again on the next sync")
	}
	if !strings.Contains(script2, `/ip hotspot user remove [find name="znabcdefgh"]`) {
		t.Error("ended login should still be cleaned up")
	}
}
