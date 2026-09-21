package utils

import "testing"

func TestNormalizeRouterPorts(t *testing.T) {
	wan, lan, err := NormalizeRouterPorts(" ETHER1 ", "ether2, ether3,,ether2, wlan1")
	if err != nil || wan != "ether1" || lan != "ether2,ether3,wlan1" {
		t.Errorf("got %q %q %v", wan, lan, err)
	}
	if wan, lan, err := NormalizeRouterPorts("", "wifi1,wifi2"); err != nil || wan != "ether1" || lan != "wifi1,wifi2" {
		t.Errorf("default wan / RouterOS 7 wifi: %q %q %v", wan, lan, err)
	}
	if _, _, err := NormalizeRouterPorts("sfp-sfpplus1", "ether1,ether2"); err != nil {
		t.Errorf("sfp uplink with ether1 as a customer port should be fine: %v", err)
	}
	for _, bad := range [][2]string{
		{"ether1", "ether1,ether2"},         // same port both ways
		{"ether1", ""},                      // no customer ports
		{"wlan1", "ether2"},                 // WiFi can't be the uplink
		{"ether1", "ether2; /system reset"}, // injection
		{"ether1", `ether2"`},
		{"eth0", "ether2"},
		{"ether1", "bridge1"},
	} {
		if _, _, err := NormalizeRouterPorts(bad[0], bad[1]); err == nil {
			t.Errorf("wan=%q lan=%q should be rejected", bad[0], bad[1])
		}
	}
}
