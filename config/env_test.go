package config

import (
	"net"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestTrustedProxyRangesAreValid(t *testing.T) {
	all := trustedProxies()
	for _, r := range all {
		if _, _, err := net.ParseCIDR(r); err != nil && net.ParseIP(r) == nil {
			t.Errorf("invalid trusted proxy entry %q", r)
		}
	}
	for _, want := range []string{"173.245.48.0/20", "2606:4700::/32", "127.0.0.0/8"} {
		found := false
		for _, r := range all {
			found = found || r == want
		}
		if !found {
			t.Errorf("trusted proxies missing %q", want)
		}
	}
}

func TestTrustedProxiesAddedFromEnv(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", " 203.0.113.0/24 , 198.51.100.7 ")
	all := trustedProxies()
	if all[len(all)-2] != "203.0.113.0/24" || all[len(all)-1] != "198.51.100.7" {
		t.Errorf("TRUSTED_PROXIES not appended: %v", all[len(all)-3:])
	}
}

// clientIP builds an app the way main.go does and returns what c.IP() sees.
// fiber's in-memory test connection reports its peer as 0.0.0.0.
func clientIP(t *testing.T, trusted []string, header string) string {
	t.Helper()
	app := fiber.New(fiber.Config{
		ProxyHeader: "CF-Connecting-IP", EnableTrustedProxyCheck: true,
		TrustedProxies: trusted, EnableIPValidation: true,
	})
	app.Get("/", func(c *fiber.Ctx) error { return c.SendString(c.IP()) })
	req := httptest.NewRequest("GET", "/", nil)
	if header != "" {
		req.Header.Set("CF-Connecting-IP", header)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}

func TestClientIP_HonoursCloudflareHeaderOnlyFromTrustedPeers(t *testing.T) {
	const real = "196.201.10.20"
	if got := clientIP(t, []string{"0.0.0.0/32"}, real); got != real {
		t.Errorf("trusted peer: c.IP() = %q, want the CF-Connecting-IP value %q", got, real)
	}
	// Someone reaching the server directly (peer not trusted) can't spoof it.
	if got := clientIP(t, cloudflareRanges, "1.2.3.4"); got == "1.2.3.4" {
		t.Errorf("untrusted peer's CF-Connecting-IP was honoured: %q", got)
	}
	// Garbage in the header is ignored rather than becoming a rate-limit key.
	if got := clientIP(t, []string{"0.0.0.0/32"}, "not-an-ip"); got == "not-an-ip" {
		t.Errorf("invalid header value accepted: %q", got)
	}
}
