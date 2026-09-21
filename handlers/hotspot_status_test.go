package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func statusGet(t *testing.T, ref string) (int, map[string]interface{}, int) {
	t.Helper()
	app := fiber.New()
	app.Get("/hotspot/status/:reference", HotspotStatus)
	resp, err := app.Test(httptest.NewRequest("GET", "/hotspot/status/"+ref, nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, len(resp.Cookies())
}

// Regression: /hotspot/status used to fall back to the sequential payment id
// and, for a completed payment, sign in that payment's customer.
func TestHotspotStatus_PaymentIDIsNotACredential(t *testing.T) {
	setupC2BTestDB(t)
	config.Config.JWTSecret = "hotspot-test-secret-hotspot-test-secret"
	a := seedISP(t, "victim-isp", "", 1)
	victim := seedCustomer(t, a.ZoneID, "Victim", "254711000001", "ZYR#V1")
	var pkg models.Package
	config.DB.First(&pkg)
	receipt, checkout := "RCPT123", "ws_CO_21092026120000123456789"
	p := models.Payment{CustomerID: &victim.ID, ZoneID: a.ZoneID, PackageID: &pkg.ID, Phone: "254711000001", Amount: 100,
		Method: "mpesa", Status: "completed", MpesaReceiptNumber: &receipt, MpesaTransactionID: &checkout}
	if err := config.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	// Guessing the numeric id must get nothing: no token, no cookie.
	st, out, cookies := statusGet(t, "1")
	if st != 404 || out["token"] != nil || cookies != 0 {
		t.Errorf("numeric payment id: status=%d token=%v cookies=%d — must be a plain 404", st, out["token"], cookies)
	}
	// Short/blank-ish references are rejected outright.
	if st, _, _ := statusGet(t, "abc"); st != 404 {
		t.Errorf("short reference: status %d, want 404", st)
	}
	// The real checkout id (which only the payer holds) still works.
	st, out, cookies = statusGet(t, checkout)
	if st != 200 || out["status"] != "paid" || out["token"] == nil || cookies != 1 {
		t.Errorf("checkout id: status=%d out=%v cookies=%d — the payer must still be signed in", st, out, cookies)
	}
}
