package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
)

func areasApp() *fiber.App {
	app := fiber.New()
	adm := app.Group("/admin", middleware.AdminAuth())
	money := middleware.RequireRoles("super_admin", "finance", "zone_manager")
	pk := middleware.RequireRoles("super_admin", "zone_manager")
	adm.Get("/me", Me)
	adm.Get("/zones", ZoneIndex)
	adm.Get("/zones/:id", ZoneShow)
	adm.Put("/zones/:id", ZoneUpdate)
	adm.Get("/customers", CustomerIndex)
	adm.Post("/customers", CustomerStore)
	adm.Get("/customers/:id", CustomerShow)
	adm.Get("/payments", money, PaymentIndex)
	adm.Get("/reports/revenue", money, ReportRevenue)
	adm.Get("/reports/zones", money, ReportZones)
	adm.Get("/reports/areas", money, ReportAreas)
	adm.Get("/packages", PackageIndex)
	adm.Put("/packages/:id", pk, PackageUpdate)
	adm.Post("/packages/:id/use-area-price", pk, PackageUseAreaPrice)
	adm.Get("/tickets", TicketIndex)
	adm.Get("/areas", AreaIndex)
	adm.Post("/areas", AreaStore)
	adm.Get("/areas/:id", AreaShow)
	adm.Post("/areas/:id/zones", AreaAssignZones)
	adm.Post("/areas/:id/plans", AreaPlanStore)
	adm.Post("/areas/:id/apply", AreaApply)
	adm.Put("/users/:id/areas", UserAreasUpdate)
	return app
}

func areaCall(t *testing.T, app *fiber.App, method, path, token, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// zoneIDsIn pulls every "zone_id" (or "id" for zone lists) out of data.
func idsIn(data interface{}, key string) map[uint]bool {
	out := map[uint]bool{}
	list, _ := data.([]interface{})
	for _, it := range list {
		if m, ok := it.(map[string]interface{}); ok {
			if v, ok := m[key].(float64); ok {
				out[uint(v)] = true
			}
		}
	}
	return out
}

type areaWorld struct {
	org               uint
	a1, a2, b1, loose uint // zones: a1,a2 in area A; b1 in area B; loose has no area
	areaA, areaB      uint
	custA, custB      models.Customer
	owner, mgr        string
	mgrUser           models.User
	otherOrgZone      uint
	otherOrgMgr       string
	pkgA1, pkgB1      uint
}

func newAreaWorld(t *testing.T) areaWorld {
	t.Helper()
	setupTenantTest(t)
	if err := config.DB.AutoMigrate(&models.Area{}, &models.AreaPackage{}, &models.UserArea{}, &models.Ticket{}, &models.Session{}); err != nil {
		t.Fatal(err)
	}
	w := areaWorld{}
	isp := seedISP(t, "acme", "", 4)
	w.org = isp.OrgID
	var zones []models.Zone
	config.DB.Where("organization_id = ?", w.org).Order("id").Find(&zones)
	w.a1, w.a2, w.b1, w.loose = zones[0].ID, zones[1].ID, zones[2].ID, zones[3].ID
	A := models.Area{OrganizationID: w.org, Name: "Maseno"}
	B := models.Area{OrganizationID: w.org, Name: "Kisumu"}
	config.DB.Create(&A)
	config.DB.Create(&B)
	w.areaA, w.areaB = A.ID, B.ID
	config.DB.Model(&models.Zone{}).Where("id IN ?", []uint{w.a1, w.a2}).Update("area_id", A.ID)
	config.DB.Model(&models.Zone{}).Where("id = ?", w.b1).Update("area_id", B.ID)

	w.custA = seedCustomer(t, w.a1, "In A", "254711000001", "ZYR#A")
	w.custB = seedCustomer(t, w.b1, "In B", "254711000002", "ZYR#B")
	for _, z := range []uint{w.a1, w.b1, w.loose} {
		config.DB.Create(&models.Payment{ZoneID: z, Phone: fmt.Sprintf("2547%08d", z), Amount: float64(100 * z), Method: "mpesa", Status: "completed"})
	}
	var pa, pb models.Package
	config.DB.Where("zone_id = ?", w.a1).First(&pa)
	config.DB.Where("zone_id = ?", w.b1).First(&pb)
	w.pkgA1, w.pkgB1 = pa.ID, pb.ID

	_, w.owner = staffUser(t, w.org, "super_admin")
	w.mgrUser, w.mgr = staffUser(t, w.org, "area_manager")
	config.DB.Create(&models.UserArea{UserID: w.mgrUser.ID, AreaID: A.ID})

	// A second ISP with its own area manager on an area id that collides.
	other := seedISP(t, "other", "", 1)
	w.otherOrgZone = other.ZoneID
	ou, otok := staffUser(t, other.OrgID, "area_manager")
	config.DB.Create(&models.UserArea{UserID: ou.ID, AreaID: A.ID}) // forged: area of another ISP
	w.otherOrgMgr = otok
	return w
}

func TestAreaManager_SeesOnlyTheirAreasZones(t *testing.T) {
	w := newAreaWorld(t)
	app := areasApp()

	st, body := areaCall(t, app, "GET", "/admin/zones", w.mgr, "")
	got := idsIn(body["data"], "id")
	if st != 200 || len(got) != 2 || !got[w.a1] || !got[w.a2] {
		t.Fatalf("zones: %d %v", st, got)
	}
	if st, _ := areaCall(t, app, "GET", fmt.Sprintf("/admin/zones/%d", w.b1), w.mgr, ""); st != 404 {
		t.Errorf("other area's zone: %d", st)
	}
	if st, _ := areaCall(t, app, "GET", fmt.Sprintf("/admin/zones/%d", w.a2), w.mgr, ""); st != 200 {
		t.Errorf("own zone: %d", st)
	}
	if st, _ := areaCall(t, app, "PUT", fmt.Sprintf("/admin/zones/%d", w.b1), w.mgr, `{"name":"x"}`); st != 403 {
		t.Errorf("update other area's zone: %d", st)
	}
	if st, _ := areaCall(t, app, "PUT", fmt.Sprintf("/admin/zones/%d", w.a1), w.mgr, `{"location":"Gate","area_id":null}`); st != 200 {
		t.Errorf("update own zone: %d", st)
	}
	var z models.Zone
	config.DB.First(&z, w.a1)
	if z.AreaID == nil {
		t.Error("zone update cleared area_id")
	}

	st, body = areaCall(t, app, "GET", "/admin/me", w.mgr, "")
	if d, _ := body["data"].(map[string]interface{}); st != 200 || d["role"] != "area_manager" {
		t.Errorf("me: %d %v", st, body)
	}
}

func TestAreaManager_CustomersPaymentsReportsScoped(t *testing.T) {
	w := newAreaWorld(t)
	app := areasApp()

	_, body := areaCall(t, app, "GET", "/admin/customers", w.mgr, "")
	if got := idsIn(body["data"], "id"); len(got) != 1 || !got[w.custA.ID] {
		t.Errorf("customers: %v", got)
	}
	if st, _ := areaCall(t, app, "GET", fmt.Sprintf("/admin/customers/%d", w.custB.ID), w.mgr, ""); st != 404 {
		t.Errorf("other area's customer: %d", st)
	}
	if st, _ := areaCall(t, app, "GET", fmt.Sprintf("/admin/customers?zone_id=%d", w.b1), w.mgr, ""); st != 200 {
		t.Errorf("filter: %d", st)
	} else if _, b := areaCall(t, app, "GET", fmt.Sprintf("/admin/customers?zone_id=%d", w.b1), w.mgr, ""); len(idsIn(b["data"], "id")) != 0 {
		t.Error("zone_id filter leaked another area's customers")
	}
	st, _ := areaCall(t, app, "POST", "/admin/customers", w.mgr, fmt.Sprintf(`{"name":"N","phone":"254711000009","zone_id":%d,"package_id":%d,"type":"hotspot","status":"inactive"}`, w.b1, w.pkgB1))
	if st != 403 {
		t.Errorf("create customer in other area: %d", st)
	}

	_, body = areaCall(t, app, "GET", "/admin/payments", w.mgr, "")
	if got := idsIn(body["data"], "zone_id"); len(got) != 1 || !got[w.a1] {
		t.Errorf("payments zones: %v", got)
	}
	_, body = areaCall(t, app, "GET", "/admin/reports/zones", w.mgr, "")
	if got := idsIn(body["data"], "zone_id"); len(got) != 2 || got[w.b1] || got[w.loose] {
		t.Errorf("reports/zones: %v", got)
	}
	_, body = areaCall(t, app, "GET", "/admin/reports/revenue", w.mgr, "")
	rev := body["data"].(map[string]interface{})["summary"].(map[string]interface{})["total_revenue"].(float64)
	if rev != float64(100*w.a1) {
		t.Errorf("revenue total = %v, want %d", rev, 100*w.a1)
	}

	from := time.Now().In(time.FixedZone("EAT", 3*3600)).Format("2006-01-02")
	_, body = areaCall(t, app, "GET", "/admin/reports/areas?from="+from+"&to="+from, w.mgr, "")
	d, _ := body["data"].(map[string]interface{})
	areas, _ := d["areas"].([]interface{})
	if len(areas) != 1 || areas[0].(map[string]interface{})["name"] != "Maseno" {
		t.Fatalf("reports/areas for manager: %v", body)
	}
	if tot := d["total"].(map[string]interface{})["takings"].(float64); tot != float64(100*w.a1) {
		t.Errorf("area manager total = %v", tot)
	}

	// Owner sees both areas plus No area, and the full total.
	_, body = areaCall(t, app, "GET", "/admin/reports/areas?from="+from+"&to="+from, w.owner, "")
	d, _ = body["data"].(map[string]interface{})
	areas, _ = d["areas"].([]interface{})
	if len(areas) != 3 || areas[2].(map[string]interface{})["name"] != "No area" {
		t.Fatalf("owner areas: %v", areas)
	}
	if tot := d["total"].(map[string]interface{})["takings"].(float64); tot != float64(100*(w.a1+w.b1+w.loose)) {
		t.Errorf("owner total = %v", tot)
	}
}

func TestAreaManager_PackagesAndAreasScoped(t *testing.T) {
	w := newAreaWorld(t)
	app := areasApp()

	_, body := areaCall(t, app, "GET", "/admin/packages", w.mgr, "")
	got := idsIn(body["data"], "zone_id")
	if len(got) != 2 || got[w.b1] {
		t.Errorf("packages: %v", got)
	}
	if st, _ := areaCall(t, app, "PUT", fmt.Sprintf("/admin/packages/%d", w.pkgB1), w.mgr, `{"price":1}`); st != 404 {
		t.Errorf("edit other area's package: %d", st)
	}
	if st, _ := areaCall(t, app, "PUT", fmt.Sprintf("/admin/packages/%d", w.pkgA1), w.mgr, `{"price":90}`); st != 200 {
		t.Errorf("edit own package: %d", st)
	}

	_, body = areaCall(t, app, "GET", "/admin/areas", w.mgr, "")
	if got := idsIn(body["data"], "id"); len(got) != 1 || !got[w.areaA] {
		t.Errorf("areas list: %v", got)
	}
	if st, _ := areaCall(t, app, "GET", fmt.Sprintf("/admin/areas/%d", w.areaB), w.mgr, ""); st != 404 {
		t.Errorf("other area: %d", st)
	}
	if st, _ := areaCall(t, app, "POST", "/admin/areas", w.mgr, `{"name":"Mine"}`); st != 403 {
		t.Errorf("area manager created an area: %d", st)
	}
	if st, _ := areaCall(t, app, "POST", fmt.Sprintf("/admin/areas/%d/apply", w.areaA), w.mgr, ""); st != 403 {
		t.Errorf("area manager applied prices: %d", st)
	}

	_, body = areaCall(t, app, "GET", "/admin/tickets", w.mgr, "")
	_ = body // tickets: scoped by query; no tickets seeded, must just not fail
}

func TestAreaManager_ForgedAssignmentAcrossISPsSeesNothing(t *testing.T) {
	w := newAreaWorld(t)
	app := areasApp()
	_, body := areaCall(t, app, "GET", "/admin/zones", w.otherOrgMgr, "")
	if got := idsIn(body["data"], "id"); len(got) != 0 {
		t.Errorf("other ISP's area manager sees zones: %v", got)
	}
	_, body = areaCall(t, app, "GET", "/admin/customers", w.otherOrgMgr, "")
	if got := idsIn(body["data"], "id"); len(got) != 0 {
		t.Errorf("other ISP's area manager sees customers: %v", got)
	}
	_, body = areaCall(t, app, "GET", "/admin/payments", w.otherOrgMgr, "")
	if got := idsIn(body["data"], "id"); len(got) != 0 {
		t.Errorf("other ISP's area manager sees payments: %v", got)
	}
	_, body = areaCall(t, app, "GET", "/admin/areas", w.otherOrgMgr, "")
	if got := idsIn(body["data"], "id"); len(got) != 0 {
		t.Errorf("other ISP's area manager sees areas: %v", got)
	}
}

func TestAreaManager_WithNoAreasSeesNothing(t *testing.T) {
	w := newAreaWorld(t)
	app := areasApp()
	config.DB.Where("user_id = ?", w.mgrUser.ID).Delete(&models.UserArea{})
	for _, p := range []string{"/admin/zones", "/admin/customers", "/admin/payments", "/admin/packages", "/admin/reports/zones"} {
		st, body := areaCall(t, app, "GET", p, w.mgr, "")
		if st != 200 {
			t.Errorf("%s: %d", p, st)
			continue
		}
		key := "id"
		if p == "/admin/reports/zones" {
			key = "zone_id"
		}
		if got := idsIn(body["data"], key); len(got) != 0 {
			t.Errorf("%s with no areas: %v", p, got)
		}
	}
}

func TestOwner_AreaFlowAndOverride(t *testing.T) {
	w := newAreaWorld(t)
	app := areasApp()

	st, body := areaCall(t, app, "POST", fmt.Sprintf("/admin/areas/%d/plans", w.areaA), w.owner,
		`{"name":"Daily 30","type":"hotspot","price":30,"speed_download_kbps":5000,"speed_upload_kbps":2000,"billing_cycle":"daily"}`)
	if st != 201 {
		t.Fatalf("plan: %d %v", st, body)
	}
	st, body = areaCall(t, app, "POST", fmt.Sprintf("/admin/areas/%d/apply", w.areaA), w.owner, "")
	if st != 200 || body["data"].(map[string]interface{})["added"].(float64) != 2 {
		t.Fatalf("apply: %d %v", st, body)
	}
	var p models.Package
	config.DB.Where("zone_id = ? AND area_package_id IS NOT NULL", w.a1).First(&p)

	// Zone edits the price → own price.
	if st, _ := areaCall(t, app, "PUT", fmt.Sprintf("/admin/packages/%d", p.ID), w.mgr, `{"price":25}`); st != 200 {
		t.Fatalf("edit: %d", st)
	}
	config.DB.First(&p, p.ID)
	if !p.AreaOverride || p.Price != 25 {
		t.Fatalf("override not set: %+v", p)
	}
	if st, _ := areaCall(t, app, "POST", fmt.Sprintf("/admin/packages/%d/use-area-price", p.ID), w.mgr, ""); st != 200 {
		t.Fatalf("use area price: %d", st)
	}
	config.DB.First(&p, p.ID)
	if p.AreaOverride || p.Price != 30 {
		t.Fatalf("use area price again: %+v", p)
	}
	// Plain zone package can't "use area price".
	if st, _ := areaCall(t, app, "POST", fmt.Sprintf("/admin/packages/%d/use-area-price", w.pkgA1), w.owner, ""); st != 422 {
		t.Errorf("non-area plan: %d", st)
	}

	// Adding the loose zone applies the list.
	st, body = areaCall(t, app, "POST", fmt.Sprintf("/admin/areas/%d/zones", w.areaA), w.owner, fmt.Sprintf(`{"zone_ids":[%d]}`, w.loose))
	if st != 200 || body["data"].(map[string]interface{})["added"].(float64) != 1 {
		t.Fatalf("assign: %d %v", st, body)
	}
	// Another ISP's zone is refused.
	if st, _ := areaCall(t, app, "POST", fmt.Sprintf("/admin/areas/%d/zones", w.areaA), w.owner, fmt.Sprintf(`{"zone_ids":[%d]}`, w.otherOrgZone)); st != 422 {
		t.Errorf("other ISP zone: %d", st)
	}
	// Area-manager assignment only for area managers, only own areas.
	if st, _ := areaCall(t, app, "PUT", fmt.Sprintf("/admin/users/%d/areas", w.mgrUser.ID), w.owner, fmt.Sprintf(`{"area_ids":[%d,%d]}`, w.areaA, w.areaB)); st != 200 {
		t.Errorf("assign areas: %d", st)
	}
	_, body = areaCall(t, app, "GET", "/admin/zones", w.mgr, "")
	if got := idsIn(body["data"], "id"); len(got) != 4 {
		t.Errorf("after adding area B + loose zone: %v", got)
	}
}

// A single-zone manager sees their zone's area, and its takings are only
// their own zone's, never the rest of the area (lead security review).
func TestZoneManager_AreaSummaryOnlyOwnZoneMoney(t *testing.T) {
	w := newAreaWorld(t)
	app := areasApp()
	config.DB.Create(&models.Payment{ZoneID: w.a2, Phone: "254700000777", Amount: 5000, Method: "mpesa", Status: "completed"})
	zm, ztok := staffUser(t, w.org, "zone_manager")
	config.DB.Model(&models.User{}).Where("id = ?", zm.ID).Update("zone_id", w.a1)

	st, body := areaCall(t, app, "GET", "/admin/areas", ztok, "")
	list, _ := body["data"].([]interface{})
	if st != 200 || len(list) != 1 {
		t.Fatalf("areas for zone manager: %d %v", st, body)
	}
	a := list[0].(map[string]interface{})
	if a["takings_today"].(float64) != float64(100*w.a1) || a["takings_month"].(float64) != float64(100*w.a1) {
		t.Errorf("zone manager sees area takings %v / %v, want only own zone %d", a["takings_today"], a["takings_month"], 100*w.a1)
	}
	if a["zones_count"].(float64) != 1 {
		t.Errorf("zone manager sees %v zones in the area", a["zones_count"])
	}

	from := time.Now().In(time.FixedZone("EAT", 3*3600)).Format("2006-01-02")
	_, body = areaCall(t, app, "GET", "/admin/reports/areas?from="+from+"&to="+from, ztok, "")
	d := body["data"].(map[string]interface{})
	if tot := d["total"].(map[string]interface{})["takings"].(float64); tot != float64(100*w.a1) {
		t.Errorf("zone manager report total = %v", tot)
	}

	// The owner's summary for the same area does include a2's money.
	_, body = areaCall(t, app, "GET", fmt.Sprintf("/admin/areas/%d", w.areaA), w.owner, "")
	oa := body["data"].(map[string]interface{})["area"].(map[string]interface{})
	if oa["takings_today"].(float64) != float64(100*w.a1+5000) {
		t.Errorf("owner area takings = %v", oa["takings_today"])
	}
}
