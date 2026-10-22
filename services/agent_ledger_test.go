package services

import (
	"fmt"
	"sync"
	"testing"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func TestCommissionFor(t *testing.T) {
	for _, c := range []struct {
		typ         string
		value, face int64
		want        int64
	}{
		{"percent", 1000, 10000, 1000}, // 10% of KES 100
		{"percent", 750, 3000, 225},    // 7.5% of KES 30
		{"percent", 333, 1000, 33},     // 3.33% of KES 10 = 33.3c -> 33c
		{"percent", 250, 1100, 28},     // 2.5% of KES 11 = 27.5c -> rounds half up to 28c
		{"percent", 0, 5000, 0},
		{"percent", 10000, 5000, 5000}, // 100%
		{"fixed", 500, 5000, 500},      // KES 5 a voucher
		{"fixed", 9000, 5000, 5000},    // never more than the voucher
		{"fixed", -5, 5000, 0},
	} {
		if got := CommissionFor(c.typ, c.value, c.face); got != c.want {
			t.Errorf("CommissionFor(%s, %d, %d) = %d, want %d", c.typ, c.value, c.face, got, c.want)
		}
	}
}

func TestFormatCentsAndCentsFromKES(t *testing.T) {
	for c, want := range map[int64]string{0: "0.00", 5: "0.05", 123456: "1,234.56", -100000050: "-1,000,000.50"} {
		if got := FormatCents(c); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", c, got, want)
		}
	}
	if CentsFromKES(29.99) != 2999 || CentsFromKES(0.1+0.2) != 30 {
		t.Error("CentsFromKES rounding")
	}
}

// A customer redeeming a voucher at the same moment the agent returns it:
// exactly one of them wins, and the ledger matches whichever did.
func TestAgentVoucher_RedeemAndReturnRaceOneWins(t *testing.T) {
	db := setupTestDB(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	no := "no"
	config.DB.Create(&models.Setting{Key: "sms_enable_voucher", Value: &no})
	zoneID, pkgID := seedZone(t) // KES 30
	config.DB.Model(&models.Zone{}).Where("id = ?", zoneID).Update("organization_id", 7)
	agent := models.Agent{OrganizationID: 7, Name: "Shop", Phone: "254700000000", CommissionType: "fixed", CommissionValue: 300,
		CreditLimitCents: 1_000_000, Status: "active", Zones: []models.Zone{{ID: zoneID}}}
	if err := config.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	actor := AgentActor{OrgID: 7, UserID: 1}
	vs := NewVoucherService(NewSmsService())

	const n = 10
	res, err := IssueToAgent(config.DB, vs, actor, AgentIssueRequest{AgentID: agent.ID, ZoneID: zoneID, PackageID: pkgID, Quantity: n, Funding: "credit", Key: "race-issue-01"})
	if err != nil {
		t.Fatal(err)
	}
	var redeemed, returned int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, v := range res.Vouchers {
		wg.Add(2)
		go func(code string) {
			defer wg.Done()
			if _, err := vs.Redeem(code, "254711000111"); err == nil {
				mu.Lock()
				redeemed++
				mu.Unlock()
			}
		}(v.Code)
		go func(i int, id uint) {
			defer wg.Done()
			if _, _, err := ReturnAgentVouchers(config.DB, actor, AgentReturnRequest{AgentID: agent.ID, VoucherIDs: []uint{id}, Key: fmt.Sprintf("race-ret-%04d", i)}); err == nil {
				mu.Lock()
				returned++
				mu.Unlock()
			}
		}(i, v.ID)
	}
	wg.Wait()
	if redeemed+returned != n {
		t.Fatalf("redeemed %d + returned %d != %d: a voucher was both sold and returned, or neither", redeemed, returned, n)
	}
	// Each sold voucher is owed at KES 30 - KES 3 = 27; returned ones net to 0.
	bal, _ := AgentBalance(config.DB, agent.ID)
	if want := int64(redeemed) * 2700; bal != want {
		t.Errorf("balance %d, want %d for %d sold", bal, want, redeemed)
	}
	var sold, ret int64
	config.DB.Model(&models.AgentVoucher{}).Where("status = ?", "sold").Count(&sold)
	config.DB.Model(&models.AgentVoucher{}).Where("status = ?", "returned").Count(&ret)
	if int(sold) != redeemed || int(ret) != returned {
		t.Errorf("agent vouchers sold/returned %d/%d, want %d/%d", sold, ret, redeemed, returned)
	}
}
