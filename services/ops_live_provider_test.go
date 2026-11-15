package services

import (
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
)

// The Home page shows who sent each prompt (Daraja / Cyzora).
func TestBuildLiveOps_RecentPaymentsCarryProvider(t *testing.T) {
	db := setupTestDB(t)
	db.AutoMigrate(&models.ZoneOutage{})
	MigrateOpsTables(db)
	now := time.Now()
	z := mkZone(t, db, 1, "Alpha", &now)
	p1 := mkPay(t, db, z.ID, "254711000001", 30, "completed", "", now.Add(-2*time.Minute))
	mkPay(t, db, z.ID, "254711000002", 30, "completed", "", now.Add(-time.Minute))
	db.Model(&models.Payment{}).Where("id = ?", p1.ID).Update("provider", "cyzora")

	live, err := BuildLiveOps(db, LiveScope{OrganizationID: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uint]string{}
	for _, p := range live.RecentPayments {
		got[p.ID] = p.Provider
	}
	if got[p1.ID] != "cyzora" || len(got) != 2 {
		t.Errorf("providers = %v, want payment %d = cyzora", got, p1.ID)
	}
	for id, p := range got {
		if id != p1.ID && p != "daraja" {
			t.Errorf("payment %d provider = %q, want daraja (an old STK row)", id, p)
		}
	}
}

func TestPaymentProvider_FromMethod(t *testing.T) {
	cases := map[[2]string]string{
		{"cyzora", "mpesa"}: "cyzora", {"daraja", "mpesa"}: "daraja", {"", "mpesa"}: "daraja",
		{"", "mpesa_c2b"}: "paybill", {"", "manual"}: "cash", {"", "credit"}: "credit",
		{"", "bank"}: "bank", {"", ""}: "other",
	}
	for in, want := range cases {
		if got := PaymentProvider(in[0], in[1]); got != want {
			t.Errorf("PaymentProvider(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
