package handlers

import (
	"fmt"
	"testing"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// PUT /customers/:id used to accept any column, so any staff role could set
// credit_balance (mint money) or move a customer to another ISP's zone.
func TestCustomerUpdate_CannotMintCreditOrCrossTenants(t *testing.T) {
	setupTenantTest(t)
	app := authzApp()
	app.Put("/admin/customers/:id", CustomerUpdate)
	a := seedISP(t, "a", "", 1)
	b := seedISP(t, "b", "", 1)
	cust := seedCustomer(t, a.ZoneID, "C", "254711000777", "ZYR#C777")
	_, ownerTok := staffUser(t, a.OrgID, "super_admin")
	_, agentTok := staffUser(t, a.OrgID, "field_agent")
	path := fmt.Sprintf("/admin/customers/%d", cust.ID)

	if st := doAs(t, app, "PUT", path, ownerTok, `{"credit_balance":5000}`); st != 422 {
		t.Errorf("credit_balance-only edit: status %d, want 422", st)
	}
	if st := doAs(t, app, "PUT", path, ownerTok, `{"name":"Renamed","credit_balance":5000}`); st != 200 {
		t.Errorf("normal edit: status %d", st)
	}
	var got models.Customer
	config.DB.First(&got, cust.ID)
	if got.CreditBalance != 0 || got.Name != "Renamed" {
		t.Errorf("after edit: balance %.2f name %q — balance must not change", got.CreditBalance, got.Name)
	}
	if st := doAs(t, app, "PUT", path, ownerTok, fmt.Sprintf(`{"zone_id":%d}`, b.ZoneID)); st != 422 {
		t.Errorf("move to another ISP's zone: status %d, want 422", st)
	}
	if st := doAs(t, app, "PUT", path, agentTok, `{"status":"active"}`); st != 403 {
		t.Errorf("field agent edit: status %d, want 403", st)
	}
}
