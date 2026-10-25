package handlers

import (
	"strconv"
	"testing"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// The captive portal sends the picked package's id. It used to be ignored
// and charged as a KES 30 "Standard" plan.
func TestResolveHotspotPlan_ChargesThePickedPackage(t *testing.T) {
	setupC2BTestDB(t)
	a := seedISP(t, "plan-isp", "", 1)
	b := seedISP(t, "other-isp", "", 1)

	cheap := models.Package{ZoneID: a.ZoneID, Name: "30 min", Type: "hotspot", Price: 7, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "hourly", Status: "active"}
	thirty := models.Package{ZoneID: a.ZoneID, Name: "10 h", Type: "hotspot", Price: 30, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "active"}
	off := models.Package{ZoneID: a.ZoneID, Name: "old", Type: "hotspot", Price: 15, SpeedUploadKbps: 1, SpeedDownloadKbps: 1, BillingCycle: "daily", Status: "inactive"}
	for _, p := range []*models.Package{&cheap, &thirty, &off} {
		if err := config.DB.Create(p).Error; err != nil {
			t.Fatal(err)
		}
	}
	var otherPkg models.Package
	config.DB.Where("zone_id = ?", b.ZoneID).First(&otherPkg)

	got, err := resolveHotspotPlan(a.ZoneID, itoa(cheap.ID))
	if err != nil || got.ID != cheap.ID || got.Price != 7 {
		t.Fatalf("picked KES 7 plan: got %+v err %v", got, err)
	}
	for name, id := range map[string]string{"inactive": itoa(off.ID), "other ISP's": itoa(otherPkg.ID), "unknown": "999999", "junk": "abc"} {
		if p, err := resolveHotspotPlan(a.ZoneID, id); err == nil {
			t.Errorf("%s plan accepted: %+v", name, p)
		}
	}
	var before int64
	config.DB.Model(&models.Package{}).Count(&before)
	if _, err := resolveHotspotPlan(a.ZoneID, "premium"); err == nil {
		t.Errorf("legacy name with no matching package must be refused")
	}
	var after int64
	config.DB.Model(&models.Package{}).Count(&after)
	if after != before {
		t.Errorf("a payment attempt created a package (%d -> %d)", before, after)
	}
}

func itoa(id uint) string { return strconv.FormatUint(uint64(id), 10) }
