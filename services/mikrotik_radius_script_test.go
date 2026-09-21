package services

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zyranet/zyranet-api/models"
)

func TestGenerateRadiusScript(t *testing.T) {
	zone := &models.Zone{ID: 4, Name: "Maseno", RouterIP: "10.200.0.2", RadiusSecret: "0123456789abcdef0123456789abcdef"}
	s, err := GenerateRadiusScript(zone, "10.200.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`/radius add address=10.200.0.1 secret="0123456789abcdef0123456789abcdef"`,
		"service=hotspot,ppp", "src-address=10.200.0.2", "authentication-port=1812", "accounting-port=1813",
		"/ip hotspot profile set [find name=\"hsp-zyranet\"] use-radius=yes radius-accounting=yes",
		"mac-auth-mode=mac-as-username", "/ppp aaa set use-radius=yes accounting=yes", "/radius incoming set accept=yes",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script is missing %q", want)
		}
	}
	// Every command must be wrapped so one failure can't abort the import.
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "/") {
			t.Errorf("unwrapped command: %s", line)
		}
	}
	// A public (non-tunnel) router address must not be forced as the source.
	zone.RouterIP = "196.201.5.5"
	if s, _ := GenerateRadiusScript(zone, "10.200.0.1"); strings.Contains(s, "src-address") {
		t.Error("src-address should only be set for a tunnel (10.200.x) router")
	}
}

func TestGenerateRadiusScript_Validation(t *testing.T) {
	ok := &models.Zone{ID: 1, Name: "Z", RadiusSecret: "abc123"}
	if _, err := GenerateRadiusScript(&models.Zone{ID: 1, Name: "Z"}, "10.200.0.1"); err == nil {
		t.Error("no secret must be refused")
	}
	if _, err := GenerateRadiusScript(ok, "not-an-ip"); err == nil {
		t.Error("a bad server address must be refused")
	}
	// A secret that could break out of the quoted string must never reach a script.
	for _, bad := range []string{`ab"cd`, "ab cd", "ab$cd", "ab\\cd", "ab\ncd"} {
		if _, err := GenerateRadiusScript(&models.Zone{ID: 1, Name: "Z", RadiusSecret: bad}, "10.200.0.1"); err == nil {
			t.Errorf("secret %q must be refused", bad)
		}
	}
}

func TestRadiusRollbackScript(t *testing.T) {
	s := GenerateRadiusRollbackScript(&models.Zone{ID: 4, Name: "Maseno"})
	for _, want := range []string{"use-radius=no", "/ppp aaa set use-radius=no", `/radius remove [find comment="zyra-radius"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("rollback is missing %q", want)
		}
	}
}

func TestGenerateQueueTuningScript(t *testing.T) {
	zone := &models.Zone{ID: 4, Name: "Maseno", HotspotAddress: "10.5.50.1/24"}
	s, err := GenerateQueueTuningScript(zone, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	// 92% of 50 Mbps = 46000k, of 20 Mbps = 18400k; upload/download order as in RouterOS max-limit.
	for _, want := range []string{
		"target=10.5.50.0/24", "max-limit=18400k/46000k", "kind=fq-codel", "name=zyra-total",
		"parent-queue=zyra-total", "waveform.com/tools/bufferbloat", "ROLLBACK",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script is missing %q", want)
		}
	}
	// Commands (outside the commented rollback) are all wrapped.
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "/") {
			t.Errorf("unwrapped command: %s", line)
		}
	}
	if regexp.MustCompile(`(?m)^:do .*\n`).FindString(s) == "" {
		t.Error("expected wrapped commands")
	}
}

func TestQueueTuning_HotspotNetworkAndValidation(t *testing.T) {
	if got := hotspotNetwork(&models.Zone{HotspotAddress: "192.168.88.1/24"}); got != "192.168.88.0/24" {
		t.Errorf("network = %q", got)
	}
	if got := hotspotNetwork(&models.Zone{}); got != "10.5.50.0/24" {
		t.Errorf("default network = %q", got)
	}
	if got := hotspotNetwork(&models.Zone{HotspotAddress: "garbage"}); got != "10.5.50.0/24" {
		t.Errorf("bad address falls back to the default, got %q", got)
	}
	for _, bad := range [][2]int{{0, 10}, {10, 0}, {-5, 10}, {20000, 10}} {
		if _, err := GenerateQueueTuningScript(&models.Zone{}, bad[0], bad[1]); err == nil {
			t.Errorf("speeds %v must be refused", bad)
		}
	}
}

func TestRosEscape(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:            `plain`,
		`a"b`:              `a\"b`,
		`$x`:               `\$x`,
		`back\slash`:       `back\\slash`,
		"line\nbreak\r":    `linebreak`,
		`"; /system reset`: `\"; /system reset`,
	} {
		if got := rosEscape(in); got != want {
			t.Errorf("rosEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
