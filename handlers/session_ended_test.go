package handlers

import (
	"fmt"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// The portal learns only "this device's plan here just ended" — nothing else.
func TestHotspotSession_EndedRecently(t *testing.T) {
	setupRouterAccessHandlers(t)
	now := time.Now()
	zone := models.Zone{Name: "Oxygen", Location: "L", OrganizationID: 1}
	config.DB.Create(&zone)
	other := models.Zone{Name: "Other ISP", Location: "L", OrganizationID: 2}
	config.DB.Create(&other)
	mk := func(user, mac, status string, exp time.Time) {
		c := models.HotspotCredential{OrganizationID: 1, ZoneID: zone.ID, CustomerID: uint(len(user) + int(exp.Unix()%1000)), PackageID: 1,
			Username: user, Password: "Secret123", MacAddress: mac, Status: status, ExpiresAt: exp}
		if err := config.DB.Create(&c).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk("znended001", "AA:BB:CC:DD:EE:01", "active", now.Add(-10*time.Minute)) // ended 10 min ago
	mk("znoldend01", "AA:BB:CC:DD:EE:02", "active", now.Add(-48*time.Hour))   // ended 2 days ago
	mk("znrenew001", "AA:BB:CC:DD:EE:03", "active", now.Add(-5*time.Minute))  // ended, then renewed:
	mk("znrenew002", "AA:BB:CC:DD:EE:03", "active", now.Add(time.Hour))

	app := fiber.New()
	app.Get("/hotspot/session", HotspotSession)
	get := func(mac string, z uint) map[string]interface{} {
		_, body := doJSON(t, app, "GET", fmt.Sprintf("/hotspot/session?mac=%s&zone_id=%d", mac, z), nil)
		return body
	}
	if b := get("aa-bb-cc-dd-ee-01", zone.ID); b["active"] != false || b["ended_recently"] != true {
		t.Errorf("just ended: %v", b)
	}
	if b := get("AA:BB:CC:DD:EE:01", zone.ID); len(b) != 3 { // + doJSON's _raw
		t.Errorf("must carry nothing but active + ended_recently: %v", b)
	}
	if b := get("AA:BB:CC:DD:EE:02", zone.ID); b["ended_recently"] != false {
		t.Errorf("ended 2 days ago: %v", b)
	}
	if b := get("AA:BB:CC:DD:EE:03", zone.ID); b["ended_recently"] != false {
		t.Errorf("renewed device must not be told its plan ended: %v", b)
	}
	if b := get("AA:BB:CC:DD:EE:01", other.ID); b["ended_recently"] != false {
		t.Errorf("another ISP's WiFi must not see it: %v", b)
	}
	if b := get("AA:BB:CC:DD:EE:01", 0); b["ended_recently"] != false {
		t.Errorf("no zone → no answer: %v", b)
	}
}
