package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
)

func agentsApp() *fiber.App {
	app := fiber.New()
	a := app.Group("/a", middleware.AdminAuth())
	a.Get("/agents", AgentIndex)
	a.Post("/agents", AgentStore)
	a.Get("/agents/me", AgentMe)
	a.Get("/agents/:id", AgentShow)
	a.Put("/agents/:id", AgentUpdate)
	a.Get("/agents/:id/vouchers", AgentVouchers)
	a.Get("/agents/:id/batches", AgentBatches)
	a.Post("/agents/:id/issue", AgentIssue)
	a.Post("/agents/:id/return", AgentReturn)
	a.Post("/agents/:id/handovers", AgentHandoverStore)
	a.Get("/agents/:id/handovers/:entryId/receipt", AgentHandoverReceipt)
	a.Post("/agents/:id/adjustments", AgentAdjustmentStore)
	a.Post("/agents/:id/entries/:entryId/reverse", AgentEntryReverse)
	a.Get("/agents/:id/statement", AgentStatementShow)
	a.Delete("/vouchers/:id", VoucherDestroy)
	return app
}

// agentCall makes one request and returns the status and decoded body.
func agentCall(t *testing.T, app *fiber.App, method, path, token, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]interface{}{"raw": string(raw)}
	}
	return resp.StatusCode, out
}

// setupAgentTest gives a fresh DB, one connection (SQLite has one writer at a
// time, standing in for MySQL row locks), a voucher service that can't send
// SMS, and two ISPs.
func setupAgentTest(t *testing.T) (app *fiber.App, a, b isp) {
	t.Helper()
	setupTenantTest(t)
	sqlDB, _ := config.DB.DB()
	sqlDB.SetMaxOpenConns(1)
	putSetting(t, "sms_enable_voucher", "no")
	old := voucherSvcGlobal
	voucherSvcGlobal = services.NewVoucherService(services.NewSmsService())
	t.Cleanup(func() { voucherSvcGlobal = old })
	return agentsApp(), seedISP(t, "acme", "", 2), seedISP(t, "beta", "", 1)
}

func pkgOf(t *testing.T, zoneID uint) models.Package {
	t.Helper()
	var p models.Package
	if err := config.DB.Where("zone_id = ?", zoneID).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

// newAgent creates an agent through the API: 10% commission, KES 1,000 credit.
func newAgent(t *testing.T, app *fiber.App, tok string, zoneID uint, extra string) uint {
	t.Helper()
	body := fmt.Sprintf(`{"name":"Mama Njeri Shop","phone":"0712000111","zone_ids":[%d],"commission_type":"percent","commission_value":1000,"credit_limit_cents":100000%s}`, zoneID, extra)
	st, out := agentCall(t, app, "POST", "/a/agents", tok, body)
	if st != 201 {
		t.Fatalf("create agent: %d %v", st, out)
	}
	return uint(out["data"].(map[string]interface{})["id"].(float64))
}

func agentBalance(t *testing.T, agentID uint) int64 {
	t.Helper()
	b, err := services.AgentBalance(config.DB, agentID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func entryCount(t *testing.T, agentID uint, typ string) int64 {
	t.Helper()
	var n int64
	config.DB.Model(&models.AgentLedgerEntry{}).Where("agent_id = ? AND type = ?", agentID, typ).Count(&n)
	return n
}

func TestAgentHandover_ParallelSameKeyRecordsOnce(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	id := newAgent(t, app, owner, a.ZoneID, "")

	body := `{"amount_cents":25000,"method":"cash","note":"Friday cash","idempotency_key":"handover-key-0001"}`
	var wg sync.WaitGroup
	statuses := make([]int, 12)
	entryIDs := make([]float64, 12)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, out := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), owner, body)
			statuses[i] = st
			if d, ok := out["data"].(map[string]interface{}); ok {
				entryIDs[i] = d["entry"].(map[string]interface{})["id"].(float64)
			}
		}(i)
	}
	wg.Wait()

	if n := entryCount(t, id, models.AgentEntryHandover); n != 1 {
		t.Fatalf("%d handover entries for 12 parallel requests with one key, want 1", n)
	}
	created := 0
	for i, st := range statuses {
		if st == 201 {
			created++
		} else if st != 200 {
			t.Errorf("request %d: status %d", i, st)
		}
		if entryIDs[i] != entryIDs[0] {
			t.Errorf("request %d got entry %v, want the same entry %v", i, entryIDs[i], entryIDs[0])
		}
	}
	if created != 1 {
		t.Errorf("%d requests reported 'created', want exactly 1 (the rest are replays)", created)
	}
	if got := agentBalance(t, id); got != -25000 {
		t.Errorf("balance %d, want -25000 (KES 250 float)", got)
	}

	// Same key, different amount: refused, not replayed.
	if st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), owner,
		`{"amount_cents":99999,"method":"cash","idempotency_key":"handover-key-0001"}`); st != 409 {
		t.Errorf("reused key with a different body: status %d, want 409", st)
	}
	// No key: refused.
	if st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), owner, `{"amount_cents":100,"method":"cash"}`); st != 400 {
		t.Errorf("handover without a key: status %d, want 400", st)
	}
}

func TestAgentHandover_ParallelDifferentKeysAllCount(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	id := newAgent(t, app, owner, a.ZoneID, "")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"amount_cents":1050,"method":"cash","idempotency_key":"k-parallel-%04d"}`, i)
			if st, out := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), owner, body); st != 201 {
				t.Errorf("handover %d: %d %v", i, st, out)
			}
		}(i)
	}
	wg.Wait()
	if got := agentBalance(t, id); got != -21000 {
		t.Errorf("balance %d after 20 x KES 10.50, want -21000", got)
	}
}

func TestAgentIssueSellReturn_ReversesUnsold(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	id := newAgent(t, app, owner, a.ZoneID, "")
	pkg := pkgOf(t, a.ZoneID) // KES 100

	issue := fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":5,"funding":"credit","idempotency_key":"issue-key-0001"}`, a.ZoneID, pkg.ID)
	st, out := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/issue", id), owner, issue)
	if st != 201 {
		t.Fatalf("issue: %d %v", st, out)
	}
	vouchers := out["data"].(map[string]interface{})["vouchers"].([]interface{})
	if len(vouchers) != 5 {
		t.Fatalf("%d vouchers, want 5", len(vouchers))
	}
	// 5 x KES 100 = 500 owed, less 10% commission = 450.
	if got := agentBalance(t, id); got != 45000 {
		t.Fatalf("balance after issue %d, want 45000", got)
	}
	// Double-click on issue: same batch, no second set of vouchers.
	st, out = agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/issue", id), owner, issue)
	if st != 200 || out["data"].(map[string]interface{})["replayed"] != true {
		t.Errorf("repeat issue: %d %v, want 200 replayed", st, out)
	}
	if n := count(t, &models.AgentVoucher{}); n != 5 {
		t.Errorf("%d agent vouchers after a repeated issue, want 5", n)
	}

	// A customer redeems one: it's sold, the balance doesn't move.
	sold := vouchers[0].(map[string]interface{})
	if _, err := voucherSvcGlobal.Redeem(sold["code"].(string), "254700000001"); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	var av models.AgentVoucher
	config.DB.Where("voucher_id = ?", uint(sold["id"].(float64))).First(&av)
	if av.Status != "sold" || av.SoldAt == nil {
		t.Errorf("redeemed agent voucher: %+v, want sold", av)
	}
	if got := agentBalance(t, id); got != 45000 {
		t.Errorf("balance after a sale %d, want 45000 (owed since issue)", got)
	}

	// Returning the sold one is refused, and refuses the whole return.
	soldID := uint(sold["id"].(float64))
	otherID := uint(vouchers[1].(map[string]interface{})["id"].(float64))
	st, _ = agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/return", id), owner,
		fmt.Sprintf(`{"voucher_ids":[%d,%d],"idempotency_key":"return-key-0001"}`, soldID, otherID))
	if st != 422 {
		t.Errorf("return including a sold voucher: status %d, want 422", st)
	}
	var v models.Voucher
	config.DB.First(&v, otherID)
	if v.Status != "unused" {
		t.Errorf("a refused return still changed voucher %d to %s", otherID, v.Status)
	}

	// Return the 4 unsold of the batch: -400 face, +40 commission back.
	batchID := uint(out["data"].(map[string]interface{})["batch"].(map[string]interface{})["id"].(float64))
	ret := fmt.Sprintf(`{"batch_id":%d,"note":"Shop closed","idempotency_key":"return-key-0002"}`, batchID)
	st, out = agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/return", id), owner, ret)
	if st != 201 {
		t.Fatalf("return: %d %v", st, out)
	}
	if got := agentBalance(t, id); got != 9000 {
		t.Errorf("balance after returning 4 unsold %d, want 9000 (the one sold, net of commission)", got)
	}
	var e models.AgentLedgerEntry
	config.DB.Where("agent_id = ? AND type = ?", id, models.AgentEntryReturn).First(&e)
	if e.AmountCents != -40000 || e.VoucherCount != 4 {
		t.Errorf("return entry %+v, want -40000 for 4", e)
	}
	var cr models.AgentLedgerEntry
	config.DB.Where("agent_id = ? AND type = ?", id, models.AgentEntryCommissionReversal).First(&cr)
	if cr.AmountCents != 4000 {
		t.Errorf("commission reversal %d, want 4000", cr.AmountCents)
	}
	// Repeat of the same return: replay, nothing new.
	if st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/return", id), owner, ret); st != 200 {
		t.Errorf("repeat return: status %d, want 200", st)
	}
	if n := entryCount(t, id, models.AgentEntryReturn); n != 1 {
		t.Errorf("%d return entries after a repeated return, want 1", n)
	}
	// A returned voucher is void and can't be redeemed.
	config.DB.First(&v, otherID)
	if v.Status != services.VoucherStatusVoid {
		t.Errorf("returned voucher status %s, want void", v.Status)
	}
	if _, err := voucherSvcGlobal.Redeem(v.Code, "254700000002"); err == nil {
		t.Error("a returned (void) voucher was redeemed")
	}
	// And can't be deleted from under the ledger.
	if st, _ := agentCall(t, app, "DELETE", fmt.Sprintf("/a/vouchers/%d", soldID), owner, ""); st != 409 {
		t.Errorf("deleting an agent's voucher: status %d, want 409", st)
	}

	// The list shows what's owed and what was sold this week.
	_, out = agentCall(t, app, "GET", "/a/agents", owner, "")
	sum := out["data"].([]interface{})[0].(map[string]interface{})["summary"].(map[string]interface{})
	if sum["balance_cents"].(float64) != 9000 || sum["sold_week_count"].(float64) != 1 || sum["unsold_count"].(float64) != 0 ||
		sum["cash_due_cents"].(float64) != 9000 || sum["commission_earned_cents"].(float64) != 1000 {
		t.Errorf("summary %v", sum)
	}
}

func TestAgentIssue_CreditLimitFloatAndUpfront(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	id := newAgent(t, app, owner, a.ZoneID, "") // KES 1,000 credit, 10%
	pkg := pkgOf(t, a.ZoneID)
	issue := func(qty int, funding, key, extra string) (int, map[string]interface{}) {
		return agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/issue", id), owner,
			fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":%d,"funding":%q,"idempotency_key":%q%s}`, a.ZoneID, pkg.ID, qty, funding, key, extra))
	}
	// 12 x 90 = 1,080 > 1,000 credit.
	if st, _ := issue(12, "credit", "credit-key-01", ""); st != 422 {
		t.Errorf("over the credit limit: status %d, want 422", st)
	}
	if st, _ := issue(11, "credit", "credit-key-02", ""); st != 201 { // 990
		t.Errorf("within the credit limit: status %d", st)
	}
	// No float yet.
	if st, _ := issue(1, "float", "float-key-01", ""); st != 422 {
		t.Errorf("float with none: status %d, want 422", st)
	}
	// Agent pays 1,500: clears 990, leaves 510 float; 5 x 90 = 450 fits.
	agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), owner, `{"amount_cents":150000,"method":"mpesa","reference":"SJK1ABC2DE","idempotency_key":"pay-key-0001"}`)
	if st, out := issue(5, "float", "float-key-02", ""); st != 201 {
		t.Errorf("issue from float: %d %v", st, out)
	}
	if got := agentBalance(t, id); got != -6000 {
		t.Errorf("balance %d, want -6000 (KES 60 float left)", got)
	}
	// The same M-Pesa code can't be recorded twice.
	if st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), owner, `{"amount_cents":150000,"method":"mpesa","reference":"sjk1abc2de","idempotency_key":"pay-key-0002"}`); st != 409 {
		t.Errorf("duplicate M-Pesa code: status %d, want 409", st)
	}
	// Upfront: issue and payment together, balance unchanged.
	if st, out := issue(2, "upfront", "upfront-key-1", `,"method":"cash"`); st != 201 {
		t.Errorf("upfront: %d %v", st, out)
	}
	if got := agentBalance(t, id); got != -6000 {
		t.Errorf("balance after an upfront batch %d, want unchanged -6000", got)
	}
}

func TestAgents_OrgIsolation(t *testing.T) {
	app, a, b := setupAgentTest(t)
	_, ownerA := staffUser(t, a.OrgID, "super_admin")
	_, ownerB := staffUser(t, b.OrgID, "super_admin")
	agentB := newAgent(t, app, ownerB, b.ZoneID, "")
	newAgent(t, app, ownerA, a.ZoneID, "")
	pkgB := pkgOf(t, b.ZoneID)

	for _, r := range []struct{ method, path, body string }{
		{"GET", fmt.Sprintf("/a/agents/%d", agentB), ""},
		{"GET", fmt.Sprintf("/a/agents/%d/statement", agentB), ""},
		{"PUT", fmt.Sprintf("/a/agents/%d", agentB), `{"credit_limit_cents":999999999}`},
		{"POST", fmt.Sprintf("/a/agents/%d/handovers", agentB), `{"amount_cents":100,"method":"cash","idempotency_key":"cross-org-01"}`},
		{"POST", fmt.Sprintf("/a/agents/%d/issue", agentB), fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":1,"idempotency_key":"cross-org-02"}`, b.ZoneID, pkgB.ID)},
	} {
		if st, _ := agentCall(t, app, r.method, r.path, ownerA, r.body); st != 404 {
			t.Errorf("%s %s by another ISP: status %d, want 404", r.method, r.path, st)
		}
	}
	_, out := agentCall(t, app, "GET", "/a/agents", ownerA, "")
	if list := out["data"].([]interface{}); len(list) != 1 {
		t.Errorf("ISP A sees %d agents, want only its own 1", len(list))
	}
	// Another ISP's zone can't be given to an agent.
	st, _ := agentCall(t, app, "POST", "/a/agents", ownerA, fmt.Sprintf(`{"name":"X","phone":"0712000999","zone_ids":[%d],"commission_type":"percent","commission_value":0}`, b.ZoneID))
	if st != 422 {
		t.Errorf("agent with another ISP's zone: status %d, want 422", st)
	}
	if n := count(t, &models.AgentLedgerEntry{}); n != 0 {
		t.Errorf("%d ledger entries written by cross-ISP requests", n)
	}
}

func TestAgents_FieldAgentSeesOnlyOwnLedger(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	me, meTok := staffUser(t, a.OrgID, "field_agent")
	mine := newAgent(t, app, owner, a.ZoneID, fmt.Sprintf(`,"user_id":%d`, me.ID))
	other := newAgent(t, app, owner, a.ZoneID, "")

	st, out := agentCall(t, app, "GET", "/a/agents", meTok, "")
	list := out["data"].([]interface{})
	if st != 200 || len(list) != 1 || uint(list[0].(map[string]interface{})["id"].(float64)) != mine {
		t.Errorf("field agent list: %d %v, want only their own agent", st, out)
	}
	if st, _ := agentCall(t, app, "GET", "/a/agents/me", meTok, ""); st != 200 {
		t.Errorf("/agents/me: status %d", st)
	}
	if st, _ := agentCall(t, app, "GET", fmt.Sprintf("/a/agents/%d/statement", mine), meTok, ""); st != 200 {
		t.Errorf("own statement: status %d", st)
	}
	for _, path := range []string{fmt.Sprintf("/a/agents/%d", other), fmt.Sprintf("/a/agents/%d/statement", other)} {
		if st, _ := agentCall(t, app, "GET", path, meTok, ""); st != 404 {
			t.Errorf("field agent reading %s: status %d, want 404", path, st)
		}
	}
	// Can't move money on their own ledger either.
	for _, r := range []struct{ path, body string }{
		{fmt.Sprintf("/a/agents/%d/handovers", mine), `{"amount_cents":100000,"method":"cash","idempotency_key":"self-pay-001"}`},
		{fmt.Sprintf("/a/agents/%d/adjustments", mine), `{"amount_cents":-100000,"note":"x","idempotency_key":"self-adj-001"}`},
		{fmt.Sprintf("/a/agents/%d/issue", mine), fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":1,"idempotency_key":"self-iss-001"}`, a.ZoneID, pkgOf(t, a.ZoneID).ID)},
		{"/a/agents", `{"name":"Me2","phone":"0712000222","zone_ids":[1],"commission_type":"percent","commission_value":5000}`},
	} {
		if st, _ := agentCall(t, app, "POST", r.path, meTok, r.body); st != 403 {
			t.Errorf("field agent POST %s: status %d, want 403", r.path, st)
		}
	}
	if st, _ := agentCall(t, app, "PUT", fmt.Sprintf("/a/agents/%d", mine), meTok, `{"commission_value":5000}`); st != 403 {
		t.Errorf("field agent changing own commission: status %d, want 403", st)
	}
	if n := count(t, &models.AgentLedgerEntry{}); n != 0 {
		t.Errorf("%d entries written by a field agent", n)
	}
}

func TestAgents_OnlyOwnerConfirmsHandoversAndSetsRates(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	id := newAgent(t, app, owner, a.ZoneID, "")
	for _, role := range []string{"zone_manager", "finance"} {
		u, tok := staffUser(t, a.OrgID, role)
		config.DB.Model(&models.User{}).Where("id = ?", u.ID).Update("zone_id", a.ZoneID)
		if st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), tok, `{"amount_cents":100,"method":"cash","idempotency_key":"role-pay-`+role+`"}`); st != 403 {
			t.Errorf("%s recording a handover: status %d, want 403", role, st)
		}
		if st, _ := agentCall(t, app, "PUT", fmt.Sprintf("/a/agents/%d", id), tok, `{"commission_value":9000}`); st != 403 {
			t.Errorf("%s setting commission: status %d, want 403", role, st)
		}
		if st, _ := agentCall(t, app, "GET", fmt.Sprintf("/a/agents/%d/statement", id), tok, ""); st != 200 {
			t.Errorf("%s reading the statement: status %d, want 200", role, st)
		}
	}
}

func TestAgentReverse_OnceAndOnlyHandoversOrAdjustments(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	id := newAgent(t, app, owner, a.ZoneID, "")
	_, out := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/handovers", id), owner, `{"amount_cents":5000,"method":"cash","idempotency_key":"rev-pay-0001"}`)
	hid := int(out["data"].(map[string]interface{})["entry"].(map[string]interface{})["id"].(float64))

	// Receipt
	st, out := agentCall(t, app, "GET", fmt.Sprintf("/a/agents/%d/handovers/%d/receipt", id, hid), owner, "")
	if st != 200 || out["data"].(map[string]interface{})["amount_cents"].(float64) != 5000 {
		t.Errorf("receipt: %d %v", st, out)
	}

	rev := func(key string) int {
		st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/entries/%d/reverse", id, hid), owner, `{"note":"typed wrong amount","idempotency_key":"`+key+`"}`)
		return st
	}
	if st := rev("reverse-key-01"); st != 201 {
		t.Fatalf("reverse: %d", st)
	}
	if st := rev("reverse-key-02"); st != 409 {
		t.Errorf("second reversal with a new key: status %d, want 409", st)
	}
	if got := agentBalance(t, id); got != 0 {
		t.Errorf("balance after a reversed handover %d, want 0", got)
	}
	// The original entry is still there, untouched.
	var orig models.AgentLedgerEntry
	config.DB.First(&orig, hid)
	if orig.AmountCents != -5000 {
		t.Errorf("original entry changed: %+v", orig)
	}
	// Issues are undone by returns, not reversals.
	pkg := pkgOf(t, a.ZoneID)
	_, out = agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/issue", id), owner, fmt.Sprintf(`{"zone_id":%d,"package_id":%d,"quantity":1,"idempotency_key":"rev-issue-01"}`, a.ZoneID, pkg.ID))
	issueEntry := int(out["data"].(map[string]interface{})["entries"].([]interface{})[0].(map[string]interface{})["id"].(float64))
	if st, _ := agentCall(t, app, "POST", fmt.Sprintf("/a/agents/%d/entries/%d/reverse", id, issueEntry), owner, `{"note":"x","idempotency_key":"rev-issue-02"}`); st != 422 {
		t.Errorf("reversing an issue: status %d, want 422", st)
	}
}

func TestAgentLedger_AppendOnly(t *testing.T) {
	_, a, _ := setupAgentTest(t)
	agent := models.Agent{OrganizationID: a.OrgID, Name: "A", Phone: "254700000000", CommissionType: "percent", Status: "active"}
	config.DB.Create(&agent)
	e := models.AgentLedgerEntry{OrganizationID: a.OrgID, AgentID: agent.ID, Type: models.AgentEntryAdjustment, AmountCents: 100, CreatedBy: 1}
	if err := config.DB.Create(&e).Error; err != nil {
		t.Fatal(err)
	}
	if err := config.DB.Model(&e).Update("amount_cents", 1).Error; err != models.ErrAppendOnly {
		t.Errorf("update of a ledger entry: %v, want ErrAppendOnly", err)
	}
	if err := config.DB.Delete(&e).Error; err != models.ErrAppendOnly {
		t.Errorf("delete of a ledger entry: %v, want ErrAppendOnly", err)
	}
	var got models.AgentLedgerEntry
	config.DB.First(&got, e.ID)
	if got.AmountCents != 100 {
		t.Errorf("entry changed to %d", got.AmountCents)
	}
}

func TestAgentStatement_Math(t *testing.T) {
	app, a, _ := setupAgentTest(t)
	_, owner := staffUser(t, a.OrgID, "super_admin")
	id := newAgent(t, app, owner, a.ZoneID, "")
	ownerU := models.User{}
	config.DB.Where("organization_id = ? AND role = ?", a.OrgID, "super_admin").First(&ownerU)

	// Entries on known East Africa dates (written directly; the API stamps "now").
	at := func(day string, hour int) time.Time {
		d, _ := time.ParseInLocation("2006-01-02", day, time.FixedZone("EAT", 3*3600))
		return d.Add(time.Duration(hour) * time.Hour).UTC()
	}
	add := func(typ string, cents int64, n int, when time.Time) {
		e := models.AgentLedgerEntry{OrganizationID: a.OrgID, AgentID: id, Type: typ, AmountCents: cents, VoucherCount: n, CreatedBy: ownerU.ID, CreatedAt: when}
		if err := config.DB.Create(&e).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Before the period: opening 10,000 - 1,000 - 4,000 = 5,000.
	add(models.AgentEntryIssue, 1000000, 100, at("2026-09-28", 10))
	add(models.AgentEntryCommission, -100000, 100, at("2026-09-28", 10))
	add(models.AgentEntryHandover, -400000, 0, at("2026-09-30", 23)) // 23:00 EAT 30 Sep = still September
	// In the period (1-7 Oct).
	add(models.AgentEntryIssue, 500000, 50, at("2026-10-01", 0)) // midnight EAT: first second of the period
	add(models.AgentEntryCommission, -50000, 50, at("2026-10-01", 0))
	add(models.AgentEntryReturn, -200000, 20, at("2026-10-03", 9))
	add(models.AgentEntryCommissionReversal, 20000, 20, at("2026-10-03", 9))
	add(models.AgentEntryHandover, -300000, 0, at("2026-10-05", 18))
	add(models.AgentEntryAdjustment, 1500, 0, at("2026-10-07", 23))
	// After the period.
	add(models.AgentEntryHandover, -99900, 0, at("2026-10-08", 0))

	st, out := agentCall(t, app, "GET", fmt.Sprintf("/a/agents/%d/statement?from=2026-10-01&to=2026-10-07", id), owner, "")
	if st != 200 {
		t.Fatalf("statement: %d %v", st, out)
	}
	s := out["data"].(map[string]interface{})["statement"].(map[string]interface{})
	want := map[string]float64{
		"opening_cents": 500000, "issued_cents": 500000, "issued_count": 50, "commission_cents": -50000,
		"returned_cents": -200000, "returned_count": 20, "commission_reversed_cents": 20000,
		"handover_cents": -300000, "adjustment_cents": 1500,
		// 5,000 + 5,000 - 500 - 2,000 + 200 - 3,000 + 15 = 4,715.
		"closing_cents": 471500,
	}
	for k, v := range want {
		if s[k].(float64) != v {
			t.Errorf("%s = %v, want %v", k, s[k], v)
		}
	}
	lines := s["lines"].([]interface{})
	if len(lines) != 6 {
		t.Fatalf("%d lines in the period, want 6", len(lines))
	}
	if last := lines[5].(map[string]interface{})["balance_after_cents"].(float64); last != 471500 {
		t.Errorf("running balance ends at %v, want the closing 471500", last)
	}
	// Next period opens at this one's close.
	_, out = agentCall(t, app, "GET", fmt.Sprintf("/a/agents/%d/statement?from=2026-10-08&to=2026-10-08", id), owner, "")
	s2 := out["data"].(map[string]interface{})["statement"].(map[string]interface{})
	if s2["opening_cents"].(float64) != 471500 || s2["closing_cents"].(float64) != 471500-99900 {
		t.Errorf("next period opening/closing %v/%v", s2["opening_cents"], s2["closing_cents"])
	}

	// CSV export.
	req := httptest.NewRequest("GET", fmt.Sprintf("/a/agents/%d/statement?from=2026-10-01&to=2026-10-07&format=csv", id), nil)
	req.Header.Set("Authorization", "Bearer "+owner)
	resp, _ := app.Test(req, -1)
	raw, _ := io.ReadAll(resp.Body)
	csv := string(raw)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || !strings.Contains(csv, ",Closing balance,,,,,,\"4,715.00\"") ||
		!strings.Contains(csv, "\"5,000.00\"") {
		t.Errorf("csv export:\n%s", csv)
	}
	if st, _ := agentCall(t, app, "GET", fmt.Sprintf("/a/agents/%d/statement?from=2026-10-07&to=2026-10-01", id), owner, ""); st != 422 {
		t.Errorf("reversed date range: status %d, want 422", st)
	}
}
