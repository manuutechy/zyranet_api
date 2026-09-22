package services

import (
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

func TestWorthReminding(t *testing.T) {
	mins := func(n int) *int { return &n }
	for _, tc := range []struct {
		pkg  *models.Package
		want bool
	}{
		{nil, false},
		{&models.Package{TimeLimitMinutes: mins(15)}, false},
		{&models.Package{TimeLimitMinutes: mins(24 * 60)}, false},
		{&models.Package{TimeLimitMinutes: mins(3 * 24 * 60)}, true},
		{&models.Package{BillingCycle: "hourly"}, false},
		{&models.Package{BillingCycle: "daily"}, false},
		{&models.Package{BillingCycle: "weekly"}, true},
		{&models.Package{BillingCycle: "monthly"}, true},
	} {
		if got := worthReminding(tc.pkg); got != tc.want {
			t.Errorf("worthReminding(%+v) = %v, want %v", tc.pkg, got, tc.want)
		}
	}
}

func TestReminderTimeAndBrand(t *testing.T) {
	utc := time.Date(2026, 9, 22, 21, 30, 0, 0, time.UTC)
	if got := utils.Kenya(utc).Format("02 Jan 15:04"); got != "23 Sep 00:30" {
		t.Errorf("EAT time = %q, want 23 Sep 00:30", got)
	}
	z := &models.Zone{Organization: &models.Organization{Name: "Acme", CaptivePortalCompanyName: "Acme WiFi"}}
	if ispName(z) != "Acme WiFi" || ispName(&models.Zone{Organization: &models.Organization{Name: "Acme"}}) != "Acme" || ispName(nil) != "internet" {
		t.Error("ispName wrong")
	}
}
