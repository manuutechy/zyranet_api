package middleware

import (
	"net"
	"testing"
)

func TestResolveClientIP(t *testing.T) {
	const visitor = "196.201.10.20"
	h := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name    string
		peer    string
		headers map[string]string
		want    string
	}{
		{"visitor → Cloudflare → nginx(loopback) → API", "127.0.0.1",
			map[string]string{"X-Real-IP": "172.70.1.2", "X-Forwarded-For": "172.70.1.2", "CF-Connecting-IP": visitor}, visitor},
		{"origin hit directly through nginx: CF header is spoofed", "127.0.0.1",
			map[string]string{"X-Real-IP": "41.90.5.6", "CF-Connecting-IP": "1.2.3.4"}, "41.90.5.6"},
		{"Cloudflare straight to the API", "172.70.1.2",
			map[string]string{"CF-Connecting-IP": visitor}, visitor},
		{"stranger straight to :8000 can't spoof", "41.90.5.6",
			map[string]string{"CF-Connecting-IP": "1.2.3.4", "X-Real-IP": "1.2.3.4"}, "41.90.5.6"},
		{"local tunnel that only sets CF-Connecting-IP", "127.0.0.1",
			map[string]string{"CF-Connecting-IP": visitor}, visitor},
		{"garbage headers are ignored", "127.0.0.1",
			map[string]string{"X-Real-IP": "not-an-ip", "X-Forwarded-For": "also bad"}, "127.0.0.1"},
	}
	for _, c := range cases {
		if got := ResolveClientIP(net.ParseIP(c.peer), h(c.headers)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
