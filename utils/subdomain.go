package utils

import (
	"errors"
	"regexp"
	"strings"
)

var subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// reservedSubdomains can never be handed to an ISP: they are the platform's
// own hosts or names an ISP could use to impersonate the platform or mail.
var reservedSubdomains = map[string]bool{
	"admin": true, "api": true, "app": true, "portal": true, "captive": true,
	"platform": true, "www": true, "mail": true, "smtp": true, "imap": true,
	"pop": true, "ftp": true, "ns1": true, "ns2": true, "dashboard": true,
	"static": true, "assets": true, "cdn": true, "support": true, "help": true,
	"status": true, "docs": true, "blog": true, "billing": true, "login": true,
	"auth": true, "staging": true, "dev": true, "test": true, "demo": true,
	"root": true, "zyra": true, "zyranet": true,
}

const (
	subdomainMinLen = 3
	subdomainMaxLen = 40
)

// NormalizeSubdomain lower-cases and validates an ISP subdomain label. An
// empty input is valid and means "no subdomain" (returns "", nil).
func NormalizeSubdomain(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", nil
	}
	if len(s) < subdomainMinLen || len(s) > subdomainMaxLen {
		return "", errors.New("subdomain must be 3-40 characters")
	}
	if !subdomainRe.MatchString(s) {
		return "", errors.New("subdomain may only contain letters, numbers and hyphens, and cannot start or end with a hyphen")
	}
	if strings.Contains(s, "--") {
		return "", errors.New("subdomain cannot contain consecutive hyphens")
	}
	if reservedSubdomains[s] {
		return "", errors.New("that subdomain is reserved")
	}
	return s, nil
}

// SubdomainFromHost extracts an ISP subdomain from a host such as
// "acme.zyranet.co.ke" or "acme.zyranet.co.ke:443". It returns "" for the base
// domain itself, for nested hosts ("a.b.zyranet.co.ke"), for other domains and
// for reserved/invalid labels — i.e. anything that is not an ISP host.
func SubdomainFromHost(host, baseDomain string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i != -1 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.TrimSuffix(host, ".")
	baseDomain = strings.ToLower(strings.Trim(strings.TrimSpace(baseDomain), "."))
	if baseDomain == "" || !strings.HasSuffix(host, "."+baseDomain) {
		return ""
	}
	label := strings.TrimSuffix(host, "."+baseDomain)
	if strings.Contains(label, ".") {
		return ""
	}
	sub, err := NormalizeSubdomain(label)
	if err != nil || sub != label {
		return ""
	}
	return sub
}
