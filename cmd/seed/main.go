// Command seed populates the database with realistic demo data (zones,
// packages, customers, sessions, payments, vouchers) so the admin panel and
// customer portal have something to show in a demo.
//
// It talks to the database directly via GORM — it never calls the M-Pesa or
// SMS services, so it cannot trigger a real STK push or text message even
// though the configured .env points at live credentials.
//
// Usage:
//
//	go run ./cmd/seed            // seeds only if the database is empty
//	go run ./cmd/seed -force     // wipes existing demo data first, then reseeds
package main

import (
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const demoPassword = "Demo@1234"

var firstNames = []string{
	"James", "Grace", "Brian", "Faith", "Kevin", "Mercy", "Dennis", "Linet",
	"Peter", "Esther", "Samuel", "Joy", "Daniel", "Ann", "Moses", "Sharon",
	"Patrick", "Catherine", "Robert", "Victor", "Lucy", "Felix", "Irene", "Collins",
}

var lastNames = []string{
	"Mwangi", "Wanjiru", "Otieno", "Achieng", "Kamau", "Njeri", "Kiptoo", "Naliaka",
	"Mutua", "Cherono", "Barasa", "Akinyi", "Njoroge", "Wambui", "Omondi", "Auma",
	"Kiprono", "Nyambura", "Wairimu", "Chebet",
}

func main() {
	config.Load()
	config.ConnectDatabase()
	db := config.DB

	log.Println("[seed] Running schema auto-migrations...")
	db.Exec("SET FOREIGN_KEY_CHECKS = 0")
	if err := db.AutoMigrate(
		&models.User{},
		&models.Zone{},
		&models.Package{},
		&models.Customer{},
		&models.Ticket{},
		&models.Payment{},
		&models.Session{},
		&models.Setting{},
		&models.Voucher{},
		&models.AuditLog{},
		&models.CreditLog{},
		&models.SmsLog{},
		&models.ZoneAlert{},
		&models.ZoneStat{},
	); err != nil {
		db.Exec("SET FOREIGN_KEY_CHECKS = 1")
		log.Fatalf("[seed] AutoMigrate failed: %v", err)
	}
	db.Exec("SET FOREIGN_KEY_CHECKS = 1")

	var zoneCount int64
	db.Model(&models.Zone{}).Count(&zoneCount)
	if zoneCount > 0 {
		log.Printf("[seed] Found %d existing zone(s) — adding demo data alongside them (nothing existing will be deleted or modified).", zoneCount)
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	log.Println("[seed] Creating admin user...")
	admin := seedAdmin(db)

	log.Println("[seed] Creating zones...")
	zones := seedZones(db)

	log.Println("[seed] Creating zone manager...")
	seedZoneManager(db, zones[0])

	log.Println("[seed] Creating packages...")
	pkgs := seedPackages(db, zones)

	log.Println("[seed] Creating customers...")
	customers := seedCustomers(db, rng, zones, pkgs)

	log.Println("[seed] Creating credit logs...")
	seedCreditLogs(db, customers)

	log.Println("[seed] Creating sessions...")
	seedSessions(db, rng, customers)

	log.Println("[seed] Creating payments...")
	seedPayments(db, rng, customers, pkgs)

	log.Println("[seed] Creating vouchers...")
	seedVouchers(db, rng, zones, pkgs, customers)

	log.Println("[seed] Creating a router alert...")
	seedAlert(db, zones)

	_ = admin

	fmt.Println()
	fmt.Println("==================================================")
	fmt.Println(" Demo data seeded successfully.")
	fmt.Println("==================================================")
	fmt.Println(" zyranet-admin login:")
	fmt.Println("   super_admin  -> demo@zyranet.co.ke / " + demoPassword)
	fmt.Println("   zone_manager -> manager@zyranet.co.ke / " + demoPassword)
	fmt.Println("   (zone manager is scoped to Kasarani Hotspot Hub)")
	fmt.Println()
	fmt.Println(" Customer portal: try 'Connect to Internet' (guest), 'Buy access")
	fmt.Println(" without an account', or log in with one of the seeded customer")
	fmt.Println(" phone numbers via OTP (any 4-digit code works outside production).")
	fmt.Println("==================================================")
}

// seedAdmin is idempotent (FirstOrCreate by email) so re-running the seeder
// to add another batch of demo zones/customers/payments doesn't blow up on
// a duplicate-email error the first time it hits the users table.
func seedAdmin(db *gorm.DB) models.User {
	hash, err := bcrypt.GenerateFromPassword([]byte(demoPassword), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("[seed] bcrypt failed: %v", err)
	}
	admin := models.User{
		Name:     "Demo Admin",
		Email:    "demo@zyranet.co.ke",
		Password: string(hash),
		Role:     "super_admin",
		Status:   "active",
	}
	if err := db.Where(models.User{Email: admin.Email}).FirstOrCreate(&admin).Error; err != nil {
		log.Fatalf("[seed] failed to create admin user: %v", err)
	}
	return admin
}

func seedZoneManager(db *gorm.DB, zone models.Zone) {
	hash, err := bcrypt.GenerateFromPassword([]byte(demoPassword), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("[seed] bcrypt failed: %v", err)
	}
	manager := models.User{
		Name:     "Zone Manager",
		Email:    "manager@zyranet.co.ke",
		Password: string(hash),
		Role:     "zone_manager",
		Status:   "active",
		ZoneID:   &zone.ID,
	}
	if err := db.Where(models.User{Email: manager.Email}).FirstOrCreate(&manager).Error; err != nil {
		log.Fatalf("[seed] failed to create zone manager: %v", err)
	}
	db.Model(&zone).Update("manager_id", manager.ID)
}

func seedZones(db *gorm.DB) []models.Zone {
	zones := []models.Zone{
		{Name: "Kasarani Hotspot Hub", Location: "Kasarani, Nairobi", RouterName: "MikroTik hAP ac2", RouterIP: "10.5.50.1", ConnectionType: "api", RouterPort: 8728, Status: "active", LastStatus: "online", LastSeenAt: ptrTime(time.Now().Add(-2 * time.Minute))},
		{Name: "Westlands Business Park", Location: "Westlands, Nairobi", RouterName: "MikroTik RB4011", RouterIP: "10.5.60.1", ConnectionType: "api", RouterPort: 8728, Status: "active", LastStatus: "online", LastSeenAt: ptrTime(time.Now().Add(-5 * time.Minute))},
		{Name: "Ruiru Junction", Location: "Ruiru, Kiambu", RouterName: "MikroTik hAP ac3", RouterIP: "10.5.70.1", ConnectionType: "api", RouterPort: 8728, Status: "active", LastStatus: "offline", LastSeenAt: ptrTime(time.Now().Add(-3 * time.Hour))},
	}
	for i := range zones {
		if err := db.Create(&zones[i]).Error; err != nil {
			log.Fatalf("[seed] failed to create zone: %v", err)
		}
	}
	return zones
}

func seedPackages(db *gorm.DB, zones []models.Zone) []models.Package {
	pkgs := []models.Package{
		{ZoneID: zones[0].ID, Name: "1 Hour Quick Browse", Type: "hotspot", Category: "single", DeviceLimit: 1, Price: 10, SpeedUploadKbps: 1024, SpeedDownloadKbps: 2048, BillingCycle: "hourly", Status: "active"},
		{ZoneID: zones[0].ID, Name: "Daily Hotspot 5GB", Type: "hotspot", Category: "single", DeviceLimit: 1, Price: 30, SpeedUploadKbps: 2048, SpeedDownloadKbps: 5120, BillingCycle: "daily", Status: "active"},
		{ZoneID: zones[0].ID, Name: "Weekly Unlimited", Type: "hotspot", Category: "single", DeviceLimit: 1, Price: 150, SpeedUploadKbps: 4096, SpeedDownloadKbps: 8192, BillingCycle: "weekly", Status: "active"},
		{ZoneID: zones[1].ID, Name: "Home WiFi 2 Devices", Type: "hotspot", Category: "multi", DeviceLimit: 2, Price: 100, SpeedUploadKbps: 5120, SpeedDownloadKbps: 10240, BillingCycle: "daily", Status: "active"},
		{ZoneID: zones[1].ID, Name: "Office WiFi 5 Devices", Type: "hotspot", Category: "multi", DeviceLimit: 5, Price: 300, SpeedUploadKbps: 8192, SpeedDownloadKbps: 15360, BillingCycle: "weekly", Status: "active"},
		{ZoneID: zones[1].ID, Name: "PPPoE Home 10Mbps", Type: "pppoe", Category: "multi", DeviceLimit: 10, Price: 2500, SpeedUploadKbps: 10240, SpeedDownloadKbps: 10240, BillingCycle: "monthly", Status: "active"},
		{ZoneID: zones[1].ID, Name: "PPPoE Business 20Mbps", Type: "pppoe", Category: "multi", DeviceLimit: 25, Price: 5000, SpeedUploadKbps: 20480, SpeedDownloadKbps: 20480, BillingCycle: "monthly", Status: "active"},
	}
	for i := range pkgs {
		if err := db.Create(&pkgs[i]).Error; err != nil {
			log.Fatalf("[seed] failed to create package: %v", err)
		}
	}
	return pkgs
}

func seedCustomers(db *gorm.DB, rng *rand.Rand, zones []models.Zone, pkgs []models.Package) []models.Customer {
	// Seed `used` with phones already in the DB so randomPhone can never
	// collide with a pre-existing customer's ZYR#<phone> account number.
	used := map[string]bool{}
	var existingPhones []string
	db.Model(&models.Customer{}).Pluck("phone", &existingPhones)
	for _, p := range existingPhones {
		used[p] = true
	}

	// Guest account numbers follow CustomerAuthGuest's own "ZYR#GUEST#N"
	// counter convention — continue from whatever's already in the DB
	// instead of hardcoding a start value, so we never collide.
	var existingGuestCount int64
	db.Unscoped().Model(&models.Customer{}).Where("account_number LIKE ?", "ZYR#GUEST#%").Count(&existingGuestCount)
	guestStart := 10001 + int(existingGuestCount)

	var customers []models.Customer
	now := time.Now()

	hotspotSingle := []models.Package{pkgs[0], pkgs[1], pkgs[2]}
	hotspotMulti := []models.Package{pkgs[3], pkgs[4]}
	pppoePkgs := []models.Package{pkgs[5], pkgs[6]}

	create := func(c models.Customer) {
		if err := db.Create(&c).Error; err != nil {
			log.Fatalf("[seed] failed to create customer %q: %v", c.Name, err)
		}
		customers = append(customers, c)
	}

	// Regular single-device hotspot subscribers
	for i := 0; i < 10; i++ {
		pkg := hotspotSingle[rng.Intn(len(hotspotSingle))]
		status := "active"
		var expiresAt time.Time
		if i < 7 {
			expiresAt = now.Add(time.Duration(rng.Intn(48)+1) * time.Hour)
		} else {
			status = "expired"
			expiresAt = now.Add(-time.Duration(rng.Intn(72)+1) * time.Hour)
		}
		create(models.Customer{
			Name: randomName(rng), Phone: randomPhone(rng, used), ZoneID: zones[0].ID, PackageID: pkg.ID,
			Type: "hotspot", Status: status, ExpiresAt: &expiresAt,
		})
	}

	// Multi-device hotspot subscribers (Westlands)
	for i := 0; i < 4; i++ {
		pkg := hotspotMulti[rng.Intn(len(hotspotMulti))]
		expiresAt := now.Add(time.Duration(rng.Intn(96)+1) * time.Hour)
		create(models.Customer{
			Name: randomName(rng), Phone: randomPhone(rng, used), ZoneID: zones[1].ID, PackageID: pkg.ID,
			Type: "hotspot", Status: "active", ExpiresAt: &expiresAt,
		})
	}

	// Guest captive-portal customers, mirroring CustomerAuthGuest's own naming
	for i := 0; i < 6; i++ {
		n := guestStart + i
		pkg := hotspotSingle[rng.Intn(len(hotspotSingle))]
		guestPhone := fmt.Sprintf("GUEST%d", n)
		pppoeUser := "guest_" + guestPhone
		expiresAt := now.Add(time.Duration(rng.Intn(24)+1) * time.Hour)
		c := models.Customer{
			Name: fmt.Sprintf("Guest_%d", n), Phone: guestPhone, ZoneID: zones[0].ID, PackageID: pkg.ID,
			Type: "hotspot", Status: "active", AccountNumber: fmt.Sprintf("ZYR#GUEST#%d", n),
			PPPoEUsername: &pppoeUser, ExpiresAt: &expiresAt,
		}
		if i < 3 {
			mac := randomMac(rng)
			c.MacAddress = &mac
			c.CreditBalance = float64([]int{20, 50, 100}[i])
		}
		create(c)
	}

	// PPPoE wireline subscribers (Westlands)
	for i := 0; i < 8; i++ {
		pkg := pppoePkgs[rng.Intn(len(pppoePkgs))]
		phone := randomPhone(rng, used)
		status := "active"
		var expiresAt time.Time
		if i < 7 {
			expiresAt = now.AddDate(0, 0, rng.Intn(28)+1)
		} else {
			status = "suspended"
			expiresAt = now.AddDate(0, 0, -rng.Intn(10)-1)
		}
		uname := "home_" + phone[len(phone)-6:]
		pass := randomAlnum(rng, 8)
		create(models.Customer{
			Name: randomName(rng), Phone: phone, ZoneID: zones[1].ID, PackageID: pkg.ID,
			Type: "pppoe", Status: status, ExpiresAt: &expiresAt,
			PPPoEUsername: &uname, PPPoEPassword: &pass,
		})
	}

	return customers
}

func seedCreditLogs(db *gorm.DB, customers []models.Customer) {
	for _, c := range customers {
		if c.CreditBalance > 0 {
			note := "M-Pesa top up"
			db.Create(&models.CreditLog{CustomerID: c.ID, Amount: c.CreditBalance, Type: "credit", Note: &note})
		}
	}
}

func seedSessions(db *gorm.DB, rng *rand.Rand, customers []models.Customer) {
	now := time.Now()
	hotspotActive := filterCustomers(customers, "hotspot", "active")
	pppoeActive := filterCustomers(customers, "pppoe", "active")

	for i, c := range hotspotActive {
		if i >= 8 {
			break
		}
		started := now.Add(-time.Duration(rng.Intn(170)+5) * time.Minute)
		db.Create(&models.Session{CustomerID: c.ID, ZoneID: c.ZoneID, StartedAt: started, DataUsedMB: rng.Intn(400) + 10})
	}

	for i, c := range pppoeActive {
		if i >= 4 {
			break
		}
		started := now.Add(-time.Duration(rng.Intn(2880)+30) * time.Minute)
		db.Create(&models.Session{CustomerID: c.ID, ZoneID: c.ZoneID, StartedAt: started, DataUsedMB: rng.Intn(5000) + 200})
	}

	all := append(append([]models.Customer{}, hotspotActive...), pppoeActive...)
	if len(all) == 0 {
		return
	}
	for day := 0; day < 14; day++ {
		count := rng.Intn(4) + 1
		for i := 0; i < count; i++ {
			c := all[rng.Intn(len(all))]
			dayRef := now.AddDate(0, 0, -day)
			started := time.Date(dayRef.Year(), dayRef.Month(), dayRef.Day(), rng.Intn(20)+1, rng.Intn(60), 0, 0, dayRef.Location())
			duration := time.Duration(rng.Intn(170)+10) * time.Minute
			ended := started.Add(duration)
			db.Create(&models.Session{
				CustomerID: c.ID, ZoneID: c.ZoneID, StartedAt: started, EndedAt: &ended,
				DataUsedMB: rng.Intn(800) + 20, DurationMinutes: int(duration.Minutes()),
			})
		}
	}
}

func seedPayments(db *gorm.DB, rng *rand.Rand, customers []models.Customer, pkgs []models.Package) {
	now := time.Now()
	var hotspotPkgs, pppoePkgs []models.Package
	for _, p := range pkgs {
		if p.Type == "hotspot" {
			hotspotPkgs = append(hotspotPkgs, p)
		} else {
			pppoePkgs = append(pppoePkgs, p)
		}
	}
	hotspotCustomers := filterByType(customers, "hotspot")
	pppoeCustomers := filterByType(customers, "pppoe")

	createPayment := func(daysAgo int, pkg models.Package, customer *models.Customer, status string) {
		ref := now.AddDate(0, 0, -daysAgo)
		createdAt := time.Date(ref.Year(), ref.Month(), ref.Day(), rng.Intn(14)+7, rng.Intn(60), rng.Intn(60), 0, ref.Location())
		p := models.Payment{
			ZoneID: pkg.ZoneID, PackageID: &pkg.ID, Amount: pkg.Price, Currency: "KES",
			Method: "mpesa", Status: status, CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if customer != nil {
			p.CustomerID = &customer.ID
			p.Phone = customer.Phone
		} else {
			p.Phone = fmt.Sprintf("2547%08d", rng.Intn(100000000))
		}
		if status == "completed" {
			txn := fmt.Sprintf("ws_CO_DEMO_%d", rng.Int63())
			p.MpesaTransactionID = &txn
		}
		if status == "failed" {
			reason := "Request cancelled by user."
			p.StatusReason = &reason
		}
		if err := db.Create(&p).Error; err != nil {
			log.Fatalf("[seed] failed to create payment: %v", err)
		}
	}

	for day := 29; day >= 0; day-- {
		hCount := rng.Intn(4) + 1
		if day == 0 {
			hCount += 3 // make sure "today" has a healthy amount of hotspot revenue
		}
		for i := 0; i < hCount; i++ {
			pkg := hotspotPkgs[rng.Intn(len(hotspotPkgs))]
			var cust *models.Customer
			if len(hotspotCustomers) > 0 && rng.Intn(2) == 0 {
				cust = &hotspotCustomers[rng.Intn(len(hotspotCustomers))]
			}
			createPayment(day, pkg, cust, "completed")
		}

		pCount := rng.Intn(2)
		if day == 0 {
			pCount += 2 // and a healthy amount of pppoe revenue today too
		}
		for i := 0; i < pCount; i++ {
			pkg := pppoePkgs[rng.Intn(len(pppoePkgs))]
			var cust *models.Customer
			if len(pppoeCustomers) > 0 {
				cust = &pppoeCustomers[rng.Intn(len(pppoeCustomers))]
			}
			createPayment(day, pkg, cust, "completed")
		}
	}

	for i := 0; i < 4; i++ {
		createPayment(rng.Intn(3), hotspotPkgs[rng.Intn(len(hotspotPkgs))], nil, "pending")
	}
	for i := 0; i < 3; i++ {
		createPayment(rng.Intn(5), hotspotPkgs[rng.Intn(len(hotspotPkgs))], nil, "failed")
	}
}

func seedVouchers(db *gorm.DB, rng *rand.Rand, zones []models.Zone, pkgs []models.Package, customers []models.Customer) {
	var hotspotPkgs []models.Package
	for _, p := range pkgs {
		if p.Type == "hotspot" {
			hotspotPkgs = append(hotspotPkgs, p)
		}
	}
	var guestCustomers []models.Customer
	for _, c := range customers {
		if strings.HasPrefix(c.AccountNumber, "ZYR#GUEST#") {
			guestCustomers = append(guestCustomers, c)
		}
	}

	// Tag codes with a random run id so re-running the seeder never collides
	// with vouchers already in the DB (Code has a unique index).
	runTag := rng.Intn(90000) + 10000

	now := time.Now()
	statuses := []string{"unused", "unused", "unused", "unused", "active", "active", "active", "depleted", "depleted", "depleted"}
	for i, status := range statuses {
		pkg := hotspotPkgs[rng.Intn(len(hotspotPkgs))]
		exp := now.AddDate(0, 0, rng.Intn(30)+1)
		v := models.Voucher{
			Code: fmt.Sprintf("ZN%d%02d", runTag, i), ZoneID: zones[0].ID, PackageID: pkg.ID,
			Type: "single_use", Status: status, ExpiresAt: &exp,
		}
		if status != "unused" {
			v.UsageCount = 1
			if len(guestCustomers) > 0 {
				cust := guestCustomers[rng.Intn(len(guestCustomers))]
				v.UsedBy = &cust.ID
			}
		}
		if err := db.Create(&v).Error; err != nil {
			log.Fatalf("[seed] failed to create voucher: %v", err)
		}
	}
}

func seedAlert(db *gorm.DB, zones []models.Zone) {
	offlineZone := zones[len(zones)-1] // Ruiru Junction, seeded with last_status "offline"
	alert := models.ZoneAlert{
		ZoneID:    offlineZone.ID,
		Type:      "offline",
		Message:   fmt.Sprintf("%s router unreachable — connection timed out.", offlineZone.Name),
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	if err := db.Create(&alert).Error; err != nil {
		log.Fatalf("[seed] failed to create zone alert: %v", err)
	}
}

func randomName(rng *rand.Rand) string {
	return firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
}

func randomPhone(rng *rand.Rand, used map[string]bool) string {
	for {
		prefix := []string{"7", "1"}[rng.Intn(2)]
		num := fmt.Sprintf("254%s%08d", prefix, rng.Intn(100000000))
		if !used[num] {
			used[num] = true
			return num
		}
	}
}

func randomMac(rng *rand.Rand) string {
	b := make([]byte, 6)
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5])
}

func randomAlnum(rng *rand.Rand, n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rng.Intn(len(chars))]
	}
	return string(b)
}

func filterCustomers(customers []models.Customer, ctype, status string) []models.Customer {
	var out []models.Customer
	for _, c := range customers {
		if c.Type == ctype && c.Status == status {
			out = append(out, c)
		}
	}
	return out
}

func filterByType(customers []models.Customer, ctype string) []models.Customer {
	var out []models.Customer
	for _, c := range customers {
		if c.Type == ctype {
			out = append(out, c)
		}
	}
	return out
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
