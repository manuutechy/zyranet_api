package middleware

import (
	"net"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
)

// Production traffic reaches the API as visitor → Cloudflare → nginx (on the
// same host, 127.0.0.1) → :8000. Fiber's built-in ProxyHeader handling trusts
// CF-Connecting-IP whenever the direct peer is trusted — and nginx on loopback
// is — so anyone who skips Cloudflare and talks to nginx directly could set
// CF-Connecting-IP to any address and pick their own rate-limit bucket.
//
// RealIP walks the chain properly instead:
//
//  1. If the peer is a local proxy (loopback/private: nginx, cloudflared), the
//     previous hop is what that proxy saw: X-Real-IP (nginx sets it to
//     $remote_addr, overwriting anything the client sent), else the last
//     X-Forwarded-For entry.
//  2. If that hop is Cloudflare, the visitor is in CF-Connecting-IP.
//  3. Otherwise the hop itself is the visitor.
//
// It then writes the answer back into the ProxyHeader so c.IP() — used by the
// rate limiters and every log line — returns the real visitor everywhere.

var (
	ipNetsOnce sync.Once
	cfNets     []*net.IPNet
	localNets  []*net.IPNet
)

func parseNets(list []string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range list {
		if !strings.Contains(s, "/") {
			if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
				s += "/32"
			} else if ip != nil {
				s += "/128"
			}
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func inNets(ip net.IP, nets []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func loadNets() {
	ipNetsOnce.Do(func() {
		cfNets = parseNets(config.CloudflareProxyRanges())
		localNets = parseNets(config.LocalProxyRanges())
	})
}

// headerIP parses a header value that may hold "a, b, c"; last=true takes the
// right-most entry (the one appended by the nearest proxy).
func headerIP(v string, last bool) net.IP {
	parts := strings.Split(v, ",")
	if last {
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
				return ip
			}
		}
		return nil
	}
	return net.ParseIP(strings.TrimSpace(parts[0]))
}

// ResolveClientIP is the chain walk above, given the TCP peer and a header
// getter. Exported for tests.
func ResolveClientIP(peer net.IP, header func(string) string) string {
	loadNets()
	hop := peer
	if inNets(peer, localNets) {
		if ip := headerIP(header("X-Real-IP"), false); ip != nil {
			hop = ip
		} else if ip := headerIP(header("X-Forwarded-For"), true); ip != nil {
			hop = ip
		} else if ip := headerIP(header("CF-Connecting-IP"), false); ip != nil {
			// A local tunnel (cloudflared) that only sets CF-Connecting-IP.
			return ip.String()
		}
	}
	if inNets(hop, cfNets) {
		if ip := headerIP(header("CF-Connecting-IP"), false); ip != nil {
			return ip.String()
		}
	}
	if hop == nil {
		return ""
	}
	return hop.String()
}

// RealIP is global middleware: it resolves the visitor's address and pins it
// into the ProxyHeader, so c.IP() is right for every handler and limiter.
func RealIP() fiber.Handler {
	return func(c *fiber.Ctx) error {
		hdr := config.Config.ProxyHeader
		if hdr == "" {
			return c.Next()
		}
		ip := ResolveClientIP(c.Context().RemoteIP(), func(k string) string { return c.Get(k) })
		if ip != "" {
			c.Request().Header.Set(hdr, ip)
		}
		return c.Next()
	}
}
