package middleware

import (
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// An ISP with a subdomain has its staff admin at <subdomain>.<BASE_DOMAIN>.
// That SPA is a static bundle served from a wildcard host, so the API learns
// which ISP a request is for from the browser-set Origin (or Referer) header.
//
// The header is forgeable by non-browser clients, which is fine: it is only
// ever used to *restrict* (lock a login/session to one ISP), never to grant
// access — credentials and the session's own organization_id still decide who
// you are.

const tenantCacheTTL = 30 * time.Second

// tenantCacheMax bounds the cache so a client sending random Origins can't
// grow it without limit.
const tenantCacheMax = 2000

type tenantEntry struct {
	found  bool
	id     uint
	status string
	exp    time.Time
}

var (
	tenantMu    sync.Mutex
	tenantCache = map[string]tenantEntry{}
)

// InvalidateTenantCache drops every cached subdomain lookup. Call it after
// any change to an Organization's subdomain or status.
func InvalidateTenantCache() {
	tenantMu.Lock()
	tenantCache = map[string]tenantEntry{}
	tenantMu.Unlock()
}

// LookupTenant returns the id and status of the ISP that owns subdomain, if
// any. Results (including misses) are cached briefly: this sits on every admin
// request and every CORS preflight.
func LookupTenant(subdomain string) (id uint, status string, ok bool) {
	now := time.Now()
	tenantMu.Lock()
	if e, hit := tenantCache[subdomain]; hit && now.Before(e.exp) {
		tenantMu.Unlock()
		return e.id, e.status, e.found
	}
	tenantMu.Unlock()

	var org models.Organization
	err := config.DB.Select("id", "status").Where("subdomain = ?", subdomain).First(&org).Error
	e := tenantEntry{found: err == nil, id: org.ID, status: org.Status, exp: now.Add(tenantCacheTTL)}

	tenantMu.Lock()
	if len(tenantCache) >= tenantCacheMax {
		tenantCache = map[string]tenantEntry{}
	}
	tenantCache[subdomain] = e
	tenantMu.Unlock()
	return e.id, e.status, e.found
}

// isPlatformHost reports whether host is one of the platform's own sites
// (admin., portal., captive., and anything in ALLOWED_ORIGINS — e.g. the live
// admin site at bit1.<base domain>). Those are not ISP subdomains even though
// they look like <label>.<base domain>, and must never be treated as one:
// doing so would lock every login there out as "unknown ISP portal".
func isPlatformHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i != -1 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	for _, o := range config.Config.AllowedOrigins {
		if u, err := url.Parse(strings.TrimSpace(o)); err == nil && strings.EqualFold(u.Hostname(), host) {
			return true
		}
	}
	return false
}

// IsPlatformLabel reports whether <label>.<base domain> is already one of the
// platform's own sites, so it can't be handed to an ISP.
func IsPlatformLabel(label string) bool {
	return isPlatformHost(strings.ToLower(label) + "." + config.Config.BaseDomain)
}

// requestHost returns the host the browser page that made this request was
// served from: Origin, else Referer.
func requestHost(c *fiber.Ctx) string {
	for _, h := range []string{c.Get("Origin"), c.Get("Referer")} {
		if h == "" {
			continue
		}
		if u, err := url.Parse(h); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return ""
}

// TenantFromRequest reports which ISP subdomain a request came from. sub is ""
// when the request is not from an ISP host (the shared admin host, the
// platform, localhost, non-browser clients) — callers then apply no
// restriction. When sub != "", ok says whether an ISP actually owns it.
func TenantFromRequest(c *fiber.Ctx) (sub string, orgID uint, status string, ok bool) {
	host := requestHost(c)
	if isPlatformHost(host) {
		return "", 0, "", false
	}
	sub = utils.SubdomainFromHost(host, config.Config.BaseDomain)
	if sub == "" {
		return "", 0, "", false
	}
	orgID, status, ok = LookupTenant(sub)
	return sub, orgID, status, ok
}

// IsTenantOrigin reports whether origin (e.g. "https://acme.zyranet.co.ke") is
// the admin host of an existing ISP. Used to open CORS for exactly those.
func IsTenantOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "https" && config.Config.AppEnv == "production" {
		return false
	}
	if isPlatformHost(u.Host) {
		return false // allowed via the static origin list, not as a tenant
	}
	sub := utils.SubdomainFromHost(u.Host, config.Config.BaseDomain)
	if sub == "" {
		return false
	}
	_, _, ok := LookupTenant(sub)
	return ok
}

// AdminURL is the staff-admin URL of an ISP, or "" if it has no subdomain.
func AdminURL(subdomain *string) string {
	if subdomain == nil || *subdomain == "" {
		return ""
	}
	return "https://" + strings.ToLower(*subdomain) + "." + config.Config.BaseDomain
}
