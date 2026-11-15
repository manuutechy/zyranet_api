// routertest writes the real setup and sync scripts for a sample router, for
// the RouterOS lab (test/chr/run.sh). Nothing touches a real database: it
// uses a throwaway in-memory one.
//
//	go run ./cmd/routertest <out-dir>
//
// Writes setup.rsc, sync1.rsc (first sync), sync2.rsc (the next minute: must
// change nothing and must not kick the ended device again) and expect.json.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: routertest <out-dir>")
	}
	out := os.Args[1]
	must(os.MkdirAll(out, 0o755))
	// The lab reaches the router from QEMU's host address; keep management open to it.
	os.Setenv("ROUTER_MGMT_ALLOWED", "10.0.2.2")
	os.Setenv("ROUTER_API_ALLOWED", "10.0.2.2")

	db, err := gorm.Open(sqlite.Open("file:routertest?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(err)
	must(db.AutoMigrate(&models.Organization{}, &models.Zone{}, &models.Package{}, &models.Customer{}, &models.Setting{}, &models.Voucher{}))
	must(services.MigrateRouterAccess(db))
	config.DB = db

	now := time.Now()
	org := models.Organization{Name: "Lab ISP", Slug: "lab"}
	must(db.Create(&org).Error)
	tun, user, pw := "10.200.0.10", "zyranet-api", "LabApiPassw0rdLabApiPassw0rd"
	last := now.Add(-time.Minute)
	zone := models.Zone{Name: "Lab", Location: "Lab", OrganizationID: org.ID, RouterName: "lab", RouterIP: "10.200.0.10",
		WanPort: "ether1", LanPorts: "ether2,ether3", ServiceMode: "hotspot", HotspotAddress: "10.5.50.1/24",
		UplinkDownMbps: 20, UplinkUpMbps: 10, SmartSpeed: true, TunnelIP: &tun, RouterUsername: &user, RouterPassword: &pw, LastSyncAt: &last}
	must(db.Create(&zone).Error)

	min30, gb := 30, 1024
	hour := models.Package{ZoneID: zone.ID, Name: "1 Hour", Type: "hotspot", Status: "active", Price: 10, SpeedUploadKbps: 1024, SpeedDownloadKbps: 2048, BillingCycle: "hourly"}
	data := models.Package{ZoneID: zone.ID, Name: "1 GB 30 min", Type: "hotspot", Status: "active", Price: 20, SpeedUploadKbps: 2048, SpeedDownloadKbps: 4096, TimeLimitMinutes: &min30, DataLimitMB: &gb, BillingCycle: "hourly"}
	must(db.Create(&hour).Error)
	must(db.Create(&data).Error)

	creds := []models.HotspotCredential{
		{Username: "znlabuser1", Password: "LabPass111", MacAddress: "02:00:00:00:00:01", PackageID: hour.ID, Status: "active", ActivatedAt: now.Add(-20 * time.Minute), ExpiresAt: now.Add(40 * time.Minute)},
		{Username: "znlabuser2", Password: "LabPass222", MacAddress: "02:00:00:00:00:02", PackageID: data.ID, Status: "active", ActivatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(10 * time.Minute)},
		// Ended 20 s ago (after the router's last sync): must be removed and kicked once.
		{Username: "znlabuser3", Password: "LabPass333", MacAddress: "02:00:00:00:00:03", PackageID: hour.ID, Status: "active", ActivatedAt: now.Add(-61 * time.Minute), ExpiresAt: now.Add(-20 * time.Second)},
	}
	for i := range creds {
		creds[i].OrganizationID, creds[i].ZoneID, creds[i].CustomerID, creds[i].Kind = org.ID, zone.ID, uint(i+1), "paid"
		must(db.Create(&creds[i]).Error)
	}
	// The second login was changed just now (new expiry): written in full.
	db.Model(&models.HotspotCredential{}).Where("id = ?", creds[1].ID).UpdateColumn("updated_at", now)

	svc := services.NewMikroTikScriptService()
	setup, _, err := svc.GenerateScript(zone.ID)
	must(err)
	sync1, err := svc.GenerateSyncScript(zone.ID)
	must(err)
	db.Model(&models.Zone{}).Where("id = ?", zone.ID).UpdateColumn("last_sync_at", now)
	sync2, err := svc.GenerateSyncScript(zone.ID)
	must(err)

	for name, body := range map[string]string{"setup.rsc": setup, "sync1.rsc": sync1, "sync2.rsc": sync2} {
		must(os.WriteFile(filepath.Join(out, name), []byte(body), 0o644))
	}
	expect := map[string]interface{}{
		"profiles":       []string{"1_Hour", "1_GB_30_min"},
		"active_users":   []string{"znlabuser1", "znlabuser2"},
		"removed_users":  []string{"znlabuser3"},
		"kicked_mac":     "02:00:00:00:00:03",
		"down_kbps":      20 * 1000 * 92 / 100,
		"up_kbps":        10 * 1000 * 92 / 100,
		"tunnel_ip":      tun,
		"api_user":       user,
		"data_limit_b":   int64(gb) * 1024 * 1024,
		"hotspot_server": "hs-zyranet",
	}
	b, _ := json.MarshalIndent(expect, "", "  ")
	must(os.WriteFile(filepath.Join(out, "expect.json"), b, 0o644))
	fmt.Printf("wrote setup.rsc (%d B), sync1.rsc (%d B), sync2.rsc (%d B) to %s\n", len(setup), len(sync1), len(sync2), out)
}
