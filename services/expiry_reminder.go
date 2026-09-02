package services

import (
	"fmt"
	"log"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// ExpiryReminderService runs background checks for expiring subscriber accounts
// and sends friendly SMS notifications before disconnection.
type ExpiryReminderService struct {
	smsService *SmsService
	stopChan   chan struct{}
}

// NewExpiryReminderService creates a new ExpiryReminderService instance.
func NewExpiryReminderService(sms *SmsService) *ExpiryReminderService {
	return &ExpiryReminderService{
		smsService: sms,
		stopChan:   make(chan struct{}),
	}
}

// Start runs the periodic background subscriber expiry reminder loop.
func (e *ExpiryReminderService) Start() {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		// Run an initial check after 30 seconds startup delay
		time.Sleep(30 * time.Second)
		e.ProcessExpiringSubscribers()

		for {
			select {
			case <-ticker.C:
				e.ProcessExpiringSubscribers()
			case <-e.stopChan:
				log.Println("[ExpiryReminder] Worker stopped.")
				return
			}
		}
	}()
}

// Stop signals the background worker to terminate.
func (e *ExpiryReminderService) Stop() {
	close(e.stopChan)
}

// ProcessExpiringSubscribers checks for customers expiring in 48h and 24h.
func (e *ExpiryReminderService) ProcessExpiringSubscribers() {
	if e.smsService == nil {
		return
	}

	now := time.Now()
	in48h := now.Add(48 * time.Hour)

	// 1. Find PPPoE/Hotspot subscribers expiring in the next 48 hours
	var customers []models.Customer
	err := config.DB.Preload("Package").Preload("Zone").
		Where("status = ? AND expires_at IS NOT NULL AND expires_at > ? AND expires_at <= ?", "active", now, in48h).
		Find(&customers).Error
	if err != nil {
		log.Printf("[ExpiryReminder] Error fetching expiring customers: %v", err)
		return
	}

	for _, customer := range customers {
		if customer.Phone == "" || customer.ExpiresAt == nil {
			continue
		}

		timeUntilExpiry := customer.ExpiresAt.Sub(now)
		is24hWindow := timeUntilExpiry <= 24*time.Hour
		reminderType := "expiry_reminder_48h"
		if is24hWindow {
			reminderType = "expiry_reminder_24h"
		}

		// Check if we already sent this specific reminder today to prevent duplicate SMS
		todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		var recentLogCount int64
		config.DB.Model(&models.AuditLog{}).
			Where("model = ? AND model_id = ? AND action = ? AND created_at >= ?", "Customer", customer.ID, reminderType, todayStart).
			Count(&recentLogCount)

		if recentLogCount > 0 {
			continue // Already notified today
		}

		// Prepare personalized notification
		pkgName := "Internet"
		var price float64 = 0.0
		if customer.Package != nil {
			pkgName = customer.Package.Name
			price = customer.Package.Price
		}

		formattedDate := customer.ExpiresAt.Format("02 Jan 15:04")
		var message string
		if is24hWindow {
			message = fmt.Sprintf("Hello %s, your Zyra Net %s plan (KES %.0f) expires in 24 hours on %s. Pay via Till/Paybill to avoid interruption.",
				customer.Name, pkgName, price, formattedDate)
		} else {
			message = fmt.Sprintf("Hello %s, your Zyra Net %s plan expires on %s. Renew early to enjoy continuous high-speed internet.",
				customer.Name, pkgName, formattedDate)
		}

		// Dispatch SMS via Zone SMS Gateway
		log.Printf("[ExpiryReminder] Dispatching %s to %s (%s)", reminderType, customer.Name, customer.Phone)
		go e.smsService.SendForZone(customer.ZoneID, customer.Phone, message)

		// Record audit log entry so we don't duplicate
		logMsg := fmt.Sprintf(`{"sent_to":"%s","message":"%s"}`, customer.Phone, message)
		config.DB.Create(&models.AuditLog{
			Action:    reminderType,
			Model:     "Customer",
			ModelID:   customer.ID,
			NewValues: &logMsg,
		})
	}
}
