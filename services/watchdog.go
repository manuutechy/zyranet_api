package services

import (
	"log"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// WatchdogService runs background maintenance tasks to keep the network healthy.
type WatchdogService struct {
	mikrotikSvc *MikroTikService
	stopChan    chan struct{}
}

// NewWatchdogService initializes the background watchdog service.
func NewWatchdogService(mikrotikSvc *MikroTikService) *WatchdogService {
	return &WatchdogService{
		mikrotikSvc: mikrotikSvc,
		stopChan:    make(chan struct{}),
	}
}

// Start launches the background watchdog loop.
func (w *WatchdogService) Start() {
	ticker := time.NewTicker(60 * time.Second)
	log.Println("[watchdog] Automated Overload & Session Watchdog started (60s interval).")

	go func() {
		for {
			select {
			case <-ticker.C:
				w.runOverloadProtectionSweep()
				w.runRouterHealthSweep()
			case <-w.stopChan:
				ticker.Stop()
				log.Println("[watchdog] Automated Watchdog stopped.")
				return
			}
		}
	}()
}

// Stop terminates the watchdog loop gracefully.
func (w *WatchdogService) Stop() {
	close(w.stopChan)
}

// runOverloadProtectionSweep finds expired customer accounts and kicks their sessions.
func (w *WatchdogService) runOverloadProtectionSweep() {
	now := time.Now()

	// 1. Find all active customers whose expiry time has passed
	var expiredCustomers []models.Customer
	if err := config.DB.Preload("Zone").
		Where("status = ? AND expires_at IS NOT NULL AND expires_at < ?", "active", now).
		Find(&expiredCustomers).Error; err != nil {
		return
	}

	if len(expiredCustomers) == 0 {
		return
	}

	log.Printf("[watchdog] Found %d expired customer accounts to decommission.", len(expiredCustomers))

	for _, cust := range expiredCustomers {
		// Update customer status to expired
		config.DB.Model(&models.Customer{}).Where("id = ?", cust.ID).Update("status", "expired")

		// Close any open sessions
		config.DB.Model(&models.Session{}).
			Where("customer_id = ? AND ended_at IS NULL", cust.ID).
			Updates(map[string]interface{}{
				"ended_at": &now,
			})

		// If customer has a MAC and Zone has router IP configured, kick the session from MikroTik
		if cust.MacAddress != nil && *cust.MacAddress != "" && cust.Zone != nil && cust.Zone.RouterIP != "" {
			mac := *cust.MacAddress
			zoneCopy := *cust.Zone
			go func(z models.Zone, m string) {
				_ = w.mikrotikSvc.DisconnectClient(&z, m)
			}(zoneCopy, mac)
		}
	}
}

// runRouterHealthSweep checks for heartbeat staleness and maintains ZoneAlerts.
func (w *WatchdogService) runRouterHealthSweep() {
	now := time.Now()
	staleThreshold := now.Add(-5 * time.Minute)

	var zones []models.Zone
	if err := config.DB.Where("status = ?", "active").Find(&zones).Error; err != nil {
		return
	}

	for _, zone := range zones {
		// Router is stale / offline
		if zone.LastSeenAt == nil || zone.LastSeenAt.Before(staleThreshold) {
			if zone.LastStatus != "offline" {
				config.DB.Model(&models.Zone{}).Where("id = ?", zone.ID).Update("last_status", "offline")

				// Create offline alert if none open
				var count int64
				config.DB.Model(&models.ZoneAlert{}).
					Where("zone_id = ? AND type = ? AND resolved_at IS NULL", zone.ID, "offline").
					Count(&count)
				if count == 0 {
					alert := models.ZoneAlert{
						ZoneID:    zone.ID,
						Type:      "offline",
						Message:   "Router heartbeat stopped. The device may be disconnected from power or ISP uplink.",
						CreatedAt: now,
					}
					config.DB.Create(&alert)
					log.Printf("[watchdog] Zone #%d (%s) marked OFFLINE - Alert created.", zone.ID, zone.Name)
				}
			}
		} else {
			// Router is online
			if zone.LastStatus != "online" {
				config.DB.Model(&models.Zone{}).Where("id = ?", zone.ID).Update("last_status", "online")

				// Auto-resolve any open offline alerts
				config.DB.Model(&models.ZoneAlert{}).
					Where("zone_id = ? AND type = ? AND resolved_at IS NULL", zone.ID, "offline").
					Update("resolved_at", &now)
				log.Printf("[watchdog] Zone #%d (%s) marked ONLINE - Alerts auto-resolved.", zone.ID, zone.Name)
			}
		}
	}
}
