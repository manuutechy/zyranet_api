package services

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// VoucherService handles voucher generation and redemption.
type VoucherService struct {
	SMS *SmsService
}

// NewVoucherService constructs a VoucherService.
func NewVoucherService(sms *SmsService) *VoucherService {
	return &VoucherService{SMS: sms}
}

// Generate creates a new unique voucher with an 8-char uppercase code.
func (s *VoucherService) Generate(zoneID, packageID uint, vType string, usageLimit int) (*models.Voucher, error) {
	if vType == "single_use" {
		usageLimit = 1
	}

	code, err := s.uniqueCode()
	if err != nil {
		return nil, fmt.Errorf("failed to generate voucher code: %w", err)
	}

	voucher := &models.Voucher{
		Code:       code,
		ZoneID:     zoneID,
		PackageID:  packageID,
		Type:       vType,
		UsageLimit: &usageLimit,
		UsageCount: 0,
		Status:     "unused",
	}

	if err := config.DB.Create(voucher).Error; err != nil {
		return nil, fmt.Errorf("failed to save voucher: %w", err)
	}
	return voucher, nil
}

// Redeem processes a voucher code for a given phone number.
func (s *VoucherService) Redeem(code, phone string) (map[string]interface{}, error) {
	code = strings.ToUpper(strings.TrimSpace(code))

	var voucher models.Voucher
	if err := config.DB.Where("code = ?", code).First(&voucher).Error; err != nil {
		return nil, fmt.Errorf("voucher code not found")
	}

	if voucher.Status == "expired" || voucher.Status == "depleted" {
		return nil, fmt.Errorf("voucher code is already expired or depleted")
	}

	if voucher.ExpiresAt != nil && time.Now().UTC().After(*voucher.ExpiresAt) {
		config.DB.Model(&voucher).Update("status", "expired")
		return nil, fmt.Errorf("voucher code has expired")
	}

	var pkg models.Package
	if err := config.DB.First(&pkg, voucher.PackageID).Error; err != nil || pkg.Status != "active" {
		return nil, fmt.Errorf("associated internet package is inactive or unavailable")
	}

	// Find or create customer
	var customer models.Customer
	err := config.DB.Where("phone = ? AND zone_id = ?", phone, voucher.ZoneID).First(&customer).Error
	if err != nil {
		// Create new guest customer
		expiresAt := calculateVoucherExpiry(&pkg)
		name := "Guest " + phone[len(phone)-4:]
		customer = models.Customer{
			Name:      name,
			Phone:     phone,
			ZoneID:    voucher.ZoneID,
			PackageID: voucher.PackageID,
			Type:      "hotspot",
			Status:    "active",
			ExpiresAt: expiresAt,
		}
		if err := config.DB.Create(&customer).Error; err != nil {
			return nil, fmt.Errorf("failed to create customer: %w", err)
		}
	} else {
		expiresAt := calculateVoucherExpiry(&pkg)
		config.DB.Model(&customer).Updates(map[string]interface{}{
			"package_id": voucher.PackageID,
			"status":     "active",
			"expires_at": expiresAt,
		})
		customer.ExpiresAt = expiresAt
	}

	// Update voucher usage
	newCount := voucher.UsageCount + 1
	newStatus := "active"
	limit := 1
	if voucher.UsageLimit != nil {
		limit = *voucher.UsageLimit
	}
	if newCount >= limit {
		newStatus = "depleted"
	}

	config.DB.Model(&voucher).Updates(map[string]interface{}{
		"usage_count": newCount,
		"status":      newStatus,
		"used_by":     customer.ID,
		"expires_at":  customer.ExpiresAt,
	})

	// Send SMS
	template := s.SMS.GetSetting("sms_template_voucher", "Hi {name}, payment of KES {price} received. Your voucher code is {code}. Enjoy browsing!")
	msg := utils.RenderTemplate(template, map[string]string{
		"name":  customer.Name,
		"price": fmt.Sprintf("%.0f", pkg.Price),
		"code":  voucher.Code,
	})
	if s.SMS.GetSetting("sms_enable_voucher", "yes") != "no" {
		go s.SMS.Send(phone, msg) //nolint:errcheck
	}

	return map[string]interface{}{
		"voucher":  voucher,
		"customer": customer,
		"package":  pkg,
	}, nil
}

// uniqueCode generates a unique 8-character uppercase alphanumeric code.
func (s *VoucherService) uniqueCode() (string, error) {
	for i := 0; i < 20; i++ {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		code := strings.ToUpper(hex.EncodeToString(b))[:8]
		var count int64
		config.DB.Model(&models.Voucher{}).Where("code = ?", code).Count(&count)
		if count == 0 {
			return code, nil
		}
	}
	return "", fmt.Errorf("could not generate unique code after 20 attempts")
}

// calculateVoucherExpiry returns an expiry time based on package settings.
func calculateVoucherExpiry(pkg *models.Package) *time.Time {
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		t := time.Now().UTC().Add(time.Duration(*pkg.TimeLimitMinutes) * time.Minute)
		return &t
	}
	t := utils.CalculateExpiry(pkg.BillingCycle, nil)
	return &t
}
