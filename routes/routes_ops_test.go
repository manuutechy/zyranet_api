package routes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"gorm.io/gorm"
)

func setupOpsRoutesDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Organization{}, &models.Zone{}, &models.Package{}, &models.Customer{},
		&models.Payment{}, &models.PendingRouterAction{}, &models.ZoneStat{}, &models.ZoneOutage{}); err != nil {
		t.Fatal(err)
	}
	if err := services.MigrateOpsTables(db); err != nil {
		t.Fatal(err)
	}
	config.DB = db
	return db
}

// opsApp mounts the real ops routes behind a fake login as the given user.
func opsApp(role string, org uint, zone *uint) *fiber.App {
	app := fiber.New()
	fakeAuth := func(c *fiber.Ctx) error {
		c.Locals("claims", &middleware.Claims{UserID: 7, Role: role, OrganizationID: org, ZoneID: zone, Type: "admin"})
		c.Locals("role", role)
		return c.Next()
	}
	registerOpsRoutes(app.Group("/api/v1"), fakeAuth)
	return app
}

func opsCall(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestOpsLiveRolesAndScope(t *testing.T) {
	db := setupOpsRoutesDB(t)
	now := time.Now()
	za := models.Zone{Name: "A", Location: "x", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: 1, Status: "active", LastSeenAt: &now}
	zb := models.Zone{Name: "B", Location: "x", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: 1, Status: "active", LastSeenAt: &now}
	zx := models.Zone{Name: "X", Location: "x", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: 2, Status: "active", LastSeenAt: &now}
	db.Create(&za)
	db.Create(&zb)
	db.Create(&zx)
	db.Create(&models.Payment{ZoneID: za.ID, Phone: "254711000001", Amount: 100, Method: "mpesa", Status: "completed"})
	db.Create(&models.Payment{ZoneID: zb.ID, Phone: "254711000002", Amount: 30, Method: "mpesa", Status: "completed"})
	db.Create(&models.Payment{ZoneID: zx.ID, Phone: "254711000003", Amount: 999, Method: "mpesa", Status: "completed"})

	today := func(out map[string]interface{}) float64 {
		return out["data"].(map[string]interface{})["money"].(map[string]interface{})["today_amount"].(float64)
	}

	if st, _ := opsCall(t, opsApp("field_agent", 1, nil), "GET", "/api/v1/ops/live", ""); st != 403 {
		t.Errorf("field_agent got %d, want 403", st)
	}
	for _, role := range []string{"super_admin", "finance"} {
		st, out := opsCall(t, opsApp(role, 1, nil), "GET", "/api/v1/ops/live", "")
		if st != 200 || today(out) != 130 {
			t.Errorf("%s: %d %v", role, st, out)
		}
	}
	// Another ISP's zone can't be asked for.
	if st, _ := opsCall(t, opsApp("super_admin", 1, nil), "GET", fmt.Sprintf("/api/v1/ops/live?zone_id=%d", zx.ID), ""); st != 404 {
		t.Errorf("foreign zone got %d", st)
	}
	st, out := opsCall(t, opsApp("super_admin", 1, nil), "GET", fmt.Sprintf("/api/v1/ops/live?zone_id=%d", zb.ID), "")
	if st != 200 || today(out) != 30 {
		t.Errorf("zone filter: %d %v", st, out)
	}
	// A zone manager only ever sees their own zone, whatever they ask for.
	st, out = opsCall(t, opsApp("zone_manager", 1, &za.ID), "GET", fmt.Sprintf("/api/v1/ops/live?zone_id=%d", zb.ID), "")
	if st != 200 || today(out) != 100 {
		t.Errorf("zone_manager: %d %v", st, out)
	}
}

func TestOpsAlertSettingsAndBanner(t *testing.T) {
	db := setupOpsRoutesDB(t)
	owner := opsApp("super_admin", 1, nil)

	for _, role := range []string{"finance", "zone_manager", "field_agent"} {
		if st, _ := opsCall(t, opsApp(role, 1, nil), "POST", "/api/v1/ops/alert-settings", `{"sms_enabled":true,"phones":["0712345678"]}`); st != 403 {
			t.Errorf("%s could change alert settings (%d)", role, st)
		}
	}
	// Default: off, no phones.
	_, out := opsCall(t, owner, "GET", "/api/v1/ops/alert-settings", "")
	d := out["data"].(map[string]interface{})
	if d["sms_enabled"] != false || len(d["phones"].([]interface{})) != 0 {
		t.Errorf("default settings: %v", d)
	}
	if st, _ := opsCall(t, owner, "POST", "/api/v1/ops/alert-settings", `{"sms_enabled":true,"phones":[]}`); st != 422 {
		t.Errorf("on without phones: %d", st)
	}
	if st, _ := opsCall(t, owner, "POST", "/api/v1/ops/alert-settings", `{"sms_enabled":true,"phones":["123"]}`); st != 422 {
		t.Errorf("bad phone: %d", st)
	}
	st, out := opsCall(t, owner, "POST", "/api/v1/ops/alert-settings", `{"sms_enabled":true,"phones":["0712 345 678","+254110000000"]}`)
	if st != 200 {
		t.Fatalf("save: %d %v", st, out)
	}
	var s models.OpsAlertSetting
	db.Where("organization_id = ?", 1).First(&s)
	if !s.SmsEnabled || s.Phones != "254712345678,254110000000" {
		t.Errorf("stored %+v", s)
	}
	// Other ISP unaffected.
	var n int64
	db.Model(&models.OpsAlertSetting{}).Where("organization_id = ?", 2).Count(&n)
	if n != 0 {
		t.Error("settings leaked to another ISP")
	}

	// Banner: org-scoped, zone managers see their zone, dismiss works once.
	old := time.Now().Add(-10 * time.Minute)
	za := models.Zone{Name: "A", Location: "x", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: 1, Status: "active", LastSeenAt: &old}
	zb := models.Zone{Name: "B", Location: "x", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: 1, Status: "active", LastSeenAt: &old}
	db.Create(&za)
	db.Create(&zb)
	a1 := models.OpsAlert{OrganizationID: 1, ZoneID: za.ID, ZoneName: "A", Kind: "offline", Message: "A down"}
	a2 := models.OpsAlert{OrganizationID: 1, ZoneID: zb.ID, ZoneName: "B", Kind: "offline", Message: "B down"}
	a3 := models.OpsAlert{OrganizationID: 2, ZoneID: 99, ZoneName: "X", Kind: "offline", Message: "X down"}
	db.Create(&a1)
	db.Create(&a2)
	db.Create(&a3)

	_, out = opsCall(t, opsApp("field_agent", 1, nil), "GET", "/api/v1/ops/alerts", "")
	data := out["data"].(map[string]interface{})
	if len(data["alerts"].([]interface{})) != 2 || len(data["offline"].([]interface{})) != 2 {
		t.Errorf("org banner: %v", data)
	}
	_, out = opsCall(t, opsApp("zone_manager", 1, &za.ID), "GET", "/api/v1/ops/alerts", "")
	data = out["data"].(map[string]interface{})
	if len(data["alerts"].([]interface{})) != 1 || len(data["offline"].([]interface{})) != 1 {
		t.Errorf("zone manager banner: %v", data)
	}
	if st, _ := opsCall(t, opsApp("zone_manager", 1, &za.ID), "POST", fmt.Sprintf("/api/v1/ops/alerts/%d/ack", a2.ID), ""); st != 404 {
		t.Errorf("zone manager dismissed another zone's alert: %d", st)
	}
	if st, _ := opsCall(t, owner, "POST", fmt.Sprintf("/api/v1/ops/alerts/%d/ack", a3.ID), ""); st != 404 {
		t.Errorf("dismissed another ISP's alert: %d", st)
	}
	if st, _ := opsCall(t, owner, "POST", fmt.Sprintf("/api/v1/ops/alerts/%d/ack", a1.ID), ""); st != 200 {
		t.Errorf("dismiss: %d", st)
	}
	_, out = opsCall(t, owner, "GET", "/api/v1/ops/alerts", "")
	if n := len(out["data"].(map[string]interface{})["alerts"].([]interface{})); n != 1 {
		t.Errorf("after dismiss: %d alerts", n)
	}
}
