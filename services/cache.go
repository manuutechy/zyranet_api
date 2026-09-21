package services

import (
	"sync"
	"time"
)

// Short-lived caches for values read on every payment/portal request but
// changed rarely (Daraja settings, per-ISP Daraja config). Staleness is bounded
// by cacheTTL and cut to zero in-process by InvalidateMpesaCaches, which every
// write path calls. In a multi-instance deployment another instance sees a
// change within cacheTTL.
const cacheTTL = 30 * time.Second

// cacheMaxEntries bounds the per-zone cache so it can't grow without limit.
const cacheMaxEntries = 5000

var (
	cacheMu       sync.Mutex
	settingsCache map[string]string
	settingsExp   time.Time
	credsCache    = map[uint]cachedCreds{}
)

type cachedCreds struct {
	creds mpesaCreds
	exp   time.Time
}

// InvalidateMpesaCaches drops the cached Daraja settings and per-zone creds.
// Call it after saving platform Daraja settings or an ISP's own Daraja config.
func InvalidateMpesaCaches() {
	cacheMu.Lock()
	settingsCache = nil
	credsCache = map[uint]cachedCreds{}
	cacheMu.Unlock()
}

func cachedSettings() (map[string]string, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if settingsCache != nil && time.Now().Before(settingsExp) {
		out := make(map[string]string, len(settingsCache))
		for k, v := range settingsCache {
			out[k] = v
		}
		return out, true
	}
	return nil, false
}

func storeSettings(m map[string]string) {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	cacheMu.Lock()
	settingsCache, settingsExp = cp, time.Now().Add(cacheTTL)
	cacheMu.Unlock()
}

func cachedCredsFor(zoneID uint) (mpesaCreds, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if e, ok := credsCache[zoneID]; ok && time.Now().Before(e.exp) {
		return e.creds, true
	}
	return mpesaCreds{}, false
}

func storeCreds(zoneID uint, c mpesaCreds) {
	cacheMu.Lock()
	if len(credsCache) >= cacheMaxEntries {
		credsCache = map[uint]cachedCreds{}
	}
	credsCache[zoneID] = cachedCreds{creds: c, exp: time.Now().Add(cacheTTL)}
	cacheMu.Unlock()
}
