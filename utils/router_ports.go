package utils

import (
	"fmt"
	"regexp"
	"strings"
)

// Port names go straight into RouterOS scripts, so only real interface names
// are accepted: ether1, sfp1, sfp-sfpplus1, combo1, wlan1, wifi1 …
var routerPortRe = regexp.MustCompile(`^(ether|sfp|sfp-sfpplus|sfp28-|qsfpplus|combo|wlan|wifi)[0-9]{1,2}(-[0-9])?$`)

// ValidRouterPort reports whether name is an allowed interface name.
func ValidRouterPort(name string) bool { return routerPortRe.MatchString(name) }

// IsWirelessPort reports whether a port is the router's own WiFi radio.
func IsWirelessPort(name string) bool {
	return strings.HasPrefix(name, "wlan") || strings.HasPrefix(name, "wifi")
}

// NormalizeRouterPorts validates the internet (WAN) port and the customer
// ports, returning the customer ports as a clean comma-separated list.
func NormalizeRouterPorts(wan, lan string) (string, string, error) {
	wan = strings.ToLower(strings.TrimSpace(wan))
	if wan == "" {
		wan = "ether1"
	}
	if !ValidRouterPort(wan) || IsWirelessPort(wan) {
		return "", "", fmt.Errorf("%q is not a valid internet port (e.g. ether1)", wan)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(lan, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		if !ValidRouterPort(p) {
			return "", "", fmt.Errorf("%q is not a valid port name (e.g. ether2, wlan1)", p)
		}
		if p == wan {
			return "", "", fmt.Errorf("%s can't be both the internet port and a customer port", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return "", "", fmt.Errorf("choose at least one port for customers")
	}
	return wan, strings.Join(out, ","), nil
}
