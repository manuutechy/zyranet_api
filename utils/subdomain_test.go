package utils

import "testing"

func TestNormalizeSubdomain(t *testing.T) {
	ok := map[string]string{"acme": "acme", "  Acme ": "acme", "wifi-kenya": "wifi-kenya", "isp2": "isp2", "": ""}
	for in, want := range ok {
		got, err := NormalizeSubdomain(in)
		if err != nil || got != want {
			t.Errorf("NormalizeSubdomain(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ab", "-acme", "acme-", "ac me", "acme.co", "acme_x", "a--b", "admin", "API", "platform", "captive", "www", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "évo"} {
		if got, err := NormalizeSubdomain(in); err == nil {
			t.Errorf("NormalizeSubdomain(%q) = %q, want an error", in, got)
		}
	}
}

func TestSubdomainFromHost(t *testing.T) {
	const base = "zyranet.co.ke"
	for in, want := range map[string]string{
		"acme.zyranet.co.ke":          "acme",
		"ACME.zyranet.co.ke":          "acme",
		"acme.zyranet.co.ke:443":      "acme",
		"acme.zyranet.co.ke.":         "acme",
		"admin.zyranet.co.ke":         "", // platform host, not an ISP
		"platform.zyranet.co.ke":      "",
		"zyranet.co.ke":               "",
		"a.b.zyranet.co.ke":           "",
		"acme.evilzyranet.co.ke":      "",
		"acme.zyranet.co.ke.evil.com": "",
		"localhost:5173":              "",
		"":                            "",
	} {
		if got := SubdomainFromHost(in, base); got != want {
			t.Errorf("SubdomainFromHost(%q) = %q, want %q", in, got, want)
		}
	}
}
