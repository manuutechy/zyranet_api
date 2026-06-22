package services

import (
	"fmt"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// MikroTikScriptService generates RouterOS .rsc configuration files.
type MikroTikScriptService struct{}

// NewMikroTikScriptService constructs a MikroTikScriptService.
func NewMikroTikScriptService() *MikroTikScriptService { return &MikroTikScriptService{} }

// GenerateScript produces a RouterOS script for a given zone.
func (s *MikroTikScriptService) GenerateScript(zoneID uint) (string, string, error) {
	var zone models.Zone
	if err := config.DB.First(&zone, zoneID).Error; err != nil {
		return "", "", fmt.Errorf("zone not found")
	}

	var packages []models.Package
	config.DB.Where("zone_id = ? AND status = ?", zoneID, "active").Find(&packages)

	var vouchers []models.Voucher
	config.DB.Preload("Package").Where("zone_id = ? AND status IN ?", zoneID, []string{"unused", "active"}).Find(&vouchers)

	var customers []models.Customer
	config.DB.Preload("Package").Where("zone_id = ? AND type = ? AND status = ?", zoneID, "pppoe", "active").Find(&customers)

	var sb strings.Builder

	sb.WriteString("# ============================================================\n")
	sb.WriteString(fmt.Sprintf("# Zyra Net — Zone: %s (%s)\n", zone.Name, zone.Location))
	sb.WriteString(fmt.Sprintf("# Generated: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("# ============================================================\n\n")

	// Hotspot Profiles
	sb.WriteString("# --- Hotspot User Profiles ---\n")
	for _, pkg := range packages {
		if pkg.Type != "hotspot" {
			continue
		}
		profileName := sanitizeProfileName(pkg.Name)
		rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
		timeout := "0s"
		if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
			timeout = fmt.Sprintf("%dm", *pkg.TimeLimitMinutes)
		}
		sb.WriteString(fmt.Sprintf(
			"/ip hotspot user profile\nadd name=%s rate-limit=%s session-timeout=%s idle-timeout=5m keepalive-timeout=2m\n\n",
			profileName, rateLimit, timeout,
		))
	}

	// Hotspot Users (from vouchers)
	sb.WriteString("# --- Hotspot Users (Vouchers) ---\n")
	for _, v := range vouchers {
		if v.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(v.Package.Name)
		sb.WriteString(fmt.Sprintf(
			"/ip hotspot user\nadd name=%s password=%s profile=%s comment=\"pkg:%s\"\n",
			v.Code, v.Code, profileName, profileName,
		))
	}
	sb.WriteString("\n")

	// PPPoE Profiles
	sb.WriteString("# --- PPPoE Profiles ---\n")
	for _, pkg := range packages {
		if pkg.Type != "pppoe" {
			continue
		}
		profileName := sanitizeProfileName(pkg.Name)
		rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
		sb.WriteString(fmt.Sprintf(
			"/ppp profile\nadd name=%s rate-limit=%s local-address=10.0.0.1 dns-server=8.8.8.8,8.8.4.4\n\n",
			profileName, rateLimit,
		))
	}

	// PPPoE Secrets
	sb.WriteString("# --- PPPoE Secrets ---\n")
	for _, c := range customers {
		if c.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(c.Package.Name)
		username := strVal(c.PPPoEUsername)
		if username == "" {
			username = strings.ReplaceAll(strings.ToLower(c.Name), " ", ".")
		}
		password := strVal(c.PPPoEPassword)
		if password == "" {
			password = "password123"
		}
		sb.WriteString(fmt.Sprintf(
			"/ppp secret\nadd name=%s password=%s service=pppoe profile=%s comment=\"customer_id:%d\"\n",
			username, password, profileName, c.ID,
		))
	}

	filename := fmt.Sprintf("zone-%s-%s.rsc",
		strings.ReplaceAll(strings.ToLower(zone.Name), " ", "-"),
		time.Now().Format("20060102150405"),
	)

	return sb.String(), filename, nil
}
