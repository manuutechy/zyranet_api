package services

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The SQLite tests run one writer at a time, so they can't show the MySQL row
// locks working. These run real concurrent transactions against a throwaway
// MySQL/MariaDB database, and only when ZYRA_MYSQL_TEST_DSN points at one,
// e.g. "root@unix(/tmp/x/mysql.sock)/zyra_test?parseTime=true". The database
// is wiped. Never point it at a real database.
func setupMySQLAgentDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("ZYRA_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("ZYRA_MYSQL_TEST_DSN not set")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	all := []interface{}{&models.Organization{}, &models.Zone{}, &models.Package{}, &models.Customer{}, &models.Voucher{}, &models.Setting{},
		&models.User{}, &models.Agent{}, &models.AgentVoucherBatch{}, &models.AgentVoucher{}, &models.AgentLedgerEntry{}, &models.AgentIdempotencyKey{}}
	// One pinned connection: FOREIGN_KEY_CHECKS is per connection (users <->
	// zones reference each other, as in main.go).
	err = db.Connection(func(c *gorm.DB) error {
		c.Exec("SET FOREIGN_KEY_CHECKS = 0")
		defer c.Exec("SET FOREIGN_KEY_CHECKS = 1")
		for _, tbl := range []string{"agent_zones", "agent_idempotency_keys", "agent_ledger_entries", "agent_vouchers", "agent_voucher_batches", "agents",
			"vouchers", "customers", "packages", "zones", "settings", "users", "organizations"} {
			c.Exec("DROP TABLE IF EXISTS " + tbl)
		}
		return c.AutoMigrate(all...)
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(40)
	config.DB = db
	return db
}

// mysqlActor is set by seedMySQLAgent: the seeded ISP and its owner.
var mysqlActor AgentActor

func seedMySQLAgent(t *testing.T, creditLimit int64) (models.Agent, uint, uint) {
	t.Helper()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	no := "no"
	must(config.DB.Create(&models.Setting{Key: "sms_enable_voucher", Value: &no}).Error)
	org := models.Organization{Name: "ISP", Slug: "isp"}
	must(config.DB.Create(&org).Error)
	owner := models.User{Name: "Owner", Email: "owner@example.com", Password: "x", Role: "super_admin", Status: "active", OrganizationID: org.ID}
	must(config.DB.Create(&owner).Error)
	mysqlActor = AgentActor{OrgID: org.ID, UserID: owner.ID}
	zone := models.Zone{Name: "Z", Location: "L", RouterName: "r", RouterIP: "127.0.0.1", OrganizationID: org.ID}
	must(config.DB.Create(&zone).Error)
	pkg := models.Package{ZoneID: zone.ID, Name: "Daily", Type: "hotspot", Price: 50, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "active"}
	must(config.DB.Create(&pkg).Error)
	agent := models.Agent{OrganizationID: org.ID, Name: "Shop", Phone: "254700000000", CommissionType: "percent", CommissionValue: 1000,
		CreditLimitCents: creditLimit, Status: "active", Zones: []models.Zone{zone}}
	must(config.DB.Create(&agent).Error)
	return agent, zone.ID, pkg.ID
}

func TestMySQL_ParallelHandoversSameKeyOneEntry(t *testing.T) {
	setupMySQLAgentDB(t)
	agent, _, _ := seedMySQLAgent(t, 0)
	actor := mysqlActor
	var wg sync.WaitGroup
	var fresh, replays, fails int32
	ids := sync.Map{}
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, replay, err := RecordHandover(config.DB, actor, AgentHandoverRequest{AgentID: agent.ID, AmountCents: 12345, Method: "cash", Key: "mysql-same-key-1"})
			switch {
			case err != nil:
				atomic.AddInt32(&fails, 1)
				t.Logf("error: %v", err)
			case replay:
				atomic.AddInt32(&replays, 1)
				ids.Store(e.ID, true)
			default:
				atomic.AddInt32(&fresh, 1)
				ids.Store(e.ID, true)
			}
		}()
	}
	wg.Wait()
	var n int64
	config.DB.Model(&models.AgentLedgerEntry{}).Where("agent_id = ?", agent.ID).Count(&n)
	if n != 1 || fresh != 1 || fails != 0 {
		t.Errorf("entries=%d fresh=%d replays=%d fails=%d, want 1 entry, 1 fresh, 29 replays", n, fresh, replays, fails)
	}
	distinct := 0
	ids.Range(func(_, _ interface{}) bool { distinct++; return true })
	if distinct != 1 {
		t.Errorf("callers saw %d different entries, want 1", distinct)
	}
}

func TestMySQL_ParallelIssuesNeverPassCreditLimit(t *testing.T) {
	setupMySQLAgentDB(t)
	// KES 50 voucher, 10% commission: KES 45 each. Limit KES 450 = 10 vouchers.
	agent, zoneID, pkgID := seedMySQLAgent(t, 45000)
	actor := mysqlActor
	vs := NewVoucherService(NewSmsService())
	var wg sync.WaitGroup
	var ok int32
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := IssueToAgent(config.DB, vs, actor, AgentIssueRequest{AgentID: agent.ID, ZoneID: zoneID, PackageID: pkgID, Quantity: 1,
				Funding: "credit", Key: fmt.Sprintf("mysql-issue-%04d", i)})
			if err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}(i)
	}
	wg.Wait()
	bal, _ := AgentBalance(config.DB, agent.ID)
	if ok != 10 || bal != 45000 {
		t.Errorf("%d issues succeeded, balance %d; want exactly 10 and 45000 (the limit)", ok, bal)
	}
}

func TestMySQL_ParallelDifferentHandoversAllCount(t *testing.T) {
	setupMySQLAgentDB(t)
	agent, _, _ := seedMySQLAgent(t, 0)
	actor := mysqlActor
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := RecordHandover(config.DB, actor, AgentHandoverRequest{AgentID: agent.ID, AmountCents: 101, Method: "cash", Key: fmt.Sprintf("mysql-h-%04d", i)}); err != nil {
				t.Errorf("handover %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if bal, _ := AgentBalance(config.DB, agent.ID); bal != -4040 {
		t.Errorf("balance %d, want -4040", bal)
	}
}

func TestMySQL_RedeemAndReturnRaceOneWins(t *testing.T) {
	setupMySQLAgentDB(t)
	agent, zoneID, pkgID := seedMySQLAgent(t, 1_000_000)
	actor := mysqlActor
	vs := NewVoucherService(NewSmsService())
	const n = 20
	res, err := IssueToAgent(config.DB, vs, actor, AgentIssueRequest{AgentID: agent.ID, ZoneID: zoneID, PackageID: pkgID, Quantity: n, Funding: "credit", Key: "mysql-race-issue"})
	if err != nil {
		t.Fatal(err)
	}
	var redeemed, returned int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, v := range res.Vouchers {
		wg.Add(2)
		go func(code string, i int) {
			defer wg.Done()
			<-start
			if _, err := vs.Redeem(code, fmt.Sprintf("2547110%05d", i)); err == nil {
				atomic.AddInt32(&redeemed, 1)
			} else if !strings.Contains(err.Error(), "expired or depleted") {
				t.Errorf("redeem %s failed for a reason other than losing the race: %v", code, err)
			}
		}(v.Code, i)
		go func(i int, id uint) {
			defer wg.Done()
			<-start
			if _, _, err := ReturnAgentVouchers(config.DB, actor, AgentReturnRequest{AgentID: agent.ID, VoucherIDs: []uint{id}, Key: fmt.Sprintf("mysql-ret-%04d", i)}); err == nil {
				atomic.AddInt32(&returned, 1)
			} else if !strings.Contains(err.Error(), "has been used") && !strings.Contains(err.Error(), "already sold") {
				t.Errorf("return %d failed for a reason other than losing the race: %v", id, err)
			}
		}(i, v.ID)
	}
	close(start)
	wg.Wait()
	t.Logf("redeemed %d, returned %d", redeemed, returned)
	if redeemed+returned != n {
		t.Fatalf("redeemed %d + returned %d != %d", redeemed, returned, n)
	}
	bal, _ := AgentBalance(config.DB, agent.ID)
	if want := int64(redeemed) * 4500; bal != want {
		t.Errorf("balance %d, want %d", bal, want)
	}
}

// Many agents of one ISP busy at once (issues, handovers, returns): no
// deadlocks between agents, and every balance is exact.
func TestMySQL_ManyAgentsAtOnceNoDeadlocks(t *testing.T) {
	setupMySQLAgentDB(t)
	first, zoneID, pkgID := seedMySQLAgent(t, 10_000_000)
	actor := mysqlActor
	vs := NewVoucherService(NewSmsService())
	agents := []models.Agent{first}
	for i := 1; i < 6; i++ {
		a := models.Agent{OrganizationID: actor.OrgID, Name: fmt.Sprintf("Shop %d", i), Phone: "254700000000", CommissionType: "percent",
			CommissionValue: 1000, CreditLimitCents: 10_000_000, Status: "active", Zones: []models.Zone{{ID: zoneID}}}
		if err := config.DB.Create(&a).Error; err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, a := range agents {
		for j := 0; j < 6; j++ {
			wg.Add(1)
			go func(agentID uint, j int) {
				defer wg.Done()
				<-start
				key := fmt.Sprintf("many-%d-%d", agentID, j)
				// Each worker: issue 2 (KES 90 owed), hand over KES 50, return 1 (-45).
				res, err := IssueToAgent(config.DB, vs, actor, AgentIssueRequest{AgentID: agentID, ZoneID: zoneID, PackageID: pkgID, Quantity: 2, Funding: "credit", Key: key + "-i"})
				if err != nil {
					t.Errorf("issue: %v", err)
					return
				}
				if _, _, err := RecordHandover(config.DB, actor, AgentHandoverRequest{AgentID: agentID, AmountCents: 5000, Method: "cash", Key: key + "-h"}); err != nil {
					t.Errorf("handover: %v", err)
				}
				if _, _, err := ReturnAgentVouchers(config.DB, actor, AgentReturnRequest{AgentID: agentID, VoucherIDs: []uint{res.Vouchers[0].ID}, Key: key + "-r"}); err != nil {
					t.Errorf("return: %v", err)
				}
			}(a.ID, j)
		}
	}
	close(start)
	wg.Wait()
	for _, a := range agents {
		// 6 workers x (9000 - 5000 - 4500) = -3000.
		if bal, _ := AgentBalance(config.DB, a.ID); bal != -3000 {
			t.Errorf("agent %d balance %d, want -3000", a.ID, bal)
		}
	}
}
