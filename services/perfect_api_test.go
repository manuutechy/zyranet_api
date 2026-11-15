package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// The RADIUS restart goes through a request file a root unit watches; the
// API never runs sudo (its unit has NoNewPrivileges).
func TestRadiusRestart_RequestFileAndReceipt(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "reload")
	old := radiusReloadWait
	radiusReloadWait = 3 * time.Second
	t.Cleanup(func() { radiusReloadWait = old })

	// Not installed (file missing): refused, nothing created.
	if requestRadiusRestart(f, 200*time.Millisecond) {
		t.Fatal("restart reported done without the request file")
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("the API must never create the request file itself")
	}

	os.WriteFile(f, nil, 0o640)
	answer := func(verdict string) {
		go func() {
			for i := 0; i < 100; i++ {
				b, _ := os.ReadFile(f)
				if line := strings.TrimSpace(string(b)); line != "" {
					nonce := strings.Fields(line)[0]
					os.WriteFile(f+".done", []byte(nonce+" "+verdict+"\n"), 0o644)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	answer("ok")
	if !requestRadiusRestart(f, 3*time.Second) {
		t.Fatal("ok receipt not accepted")
	}
	b, _ := os.ReadFile(f)
	if parts := strings.Fields(string(b)); len(parts) != 2 || len(parts[0]) != 32 {
		t.Errorf("request line %q, want '<32 hex> <unix>'", b)
	}
	os.WriteFile(f, nil, 0o640)
	answer("fail config check")
	if requestRadiusRestart(f, 3*time.Second) {
		t.Error("a failed restart was reported as done")
	}
	// A stale receipt from an earlier request never counts.
	os.WriteFile(f+".done", []byte("00000000000000000000000000000000 ok\n"), 0o644)
	if requestRadiusRestart(f, 300*time.Millisecond) {
		t.Error("someone else's receipt was accepted")
	}

	config.Config.RadiusReloadFile, config.Config.RadiusReloadCmd = "", ""
	if (&RadiusService{}).ReloadServer() {
		t.Error("with nothing configured ReloadServer must say no (staff restart by hand)")
	}
}

// Sessions come from RADIUS accounting and router check-ins, not the dead
// sessions table.
func TestSessions_FromRadacctAndHeartbeat(t *testing.T) {
	db := setupTestDB(t)
	for _, stmt := range []string{
		`CREATE TABLE radcheck (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, attribute TEXT, op TEXT, value TEXT)`,
		`CREATE TABLE radreply (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, attribute TEXT, op TEXT, value TEXT)`,
		`CREATE TABLE radacct (radacctid INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, acctstarttime DATETIME, acctstoptime DATETIME, acctsessiontime INTEGER, acctinputoctets INTEGER, acctoutputoctets INTEGER, callingstationid TEXT, framedipaddress TEXT)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	ResetRadiusTableCache()
	t.Cleanup(ResetRadiusTableCache)
	z := models.Zone{Name: "Z", Location: "x", RouterName: "r", RouterIP: "1.1.1.1", OrganizationID: 1}
	db.Create(&z)
	c := models.Customer{Name: "C", Phone: "254700000001", ZoneID: z.ID, Type: "hotspot", Status: "active"}
	db.Create(&c)
	db.Create(&models.RadiusAccount{Username: "AA:BB", CustomerID: c.ID, ZoneID: z.ID, Kind: "hotspot", Active: true})
	now := time.Now()
	db.Exec(`INSERT INTO radacct (username, acctstarttime, acctstoptime, acctsessiontime, acctinputoctets, acctoutputoctets) VALUES ('AA:BB', ?, ?, 3600, 1048576, 2097152)`, now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	db.Exec(`INSERT INTO radacct (username, acctstarttime, acctsessiontime) VALUES ('AA:BB', ?, 60)`, now.Add(-time.Minute))

	if n := OpenRadiusSessions(db, []uint{z.ID}, "hotspot")[z.ID]; n != 1 {
		t.Errorf("open sessions %d, want 1", n)
	}
	if n := OpenRadiusSessions(db, []uint{z.ID}, "pppoe")[z.ID]; n != 0 {
		t.Errorf("pppoe open %d, want 0", n)
	}
	if n := RadiusSessionsStarted(db, []uint{z.ID}, "", now.Add(-4*time.Hour), now.Add(time.Hour)); n != 2 {
		t.Errorf("started %d, want 2", n)
	}
	list := CustomerSessions(db, c.ID, 10)
	if len(list) != 2 || list[1].DataUsedMB != 3 || list[1].DurationMinutes != 60 || list[0].EndedAt != nil {
		t.Errorf("customer sessions %+v", list)
	}
	db.Create(&models.ZoneStat{ZoneID: z.ID, ConnectedClients: 7, RecordedAt: now.Add(-time.Minute)})
	if n := OnlineNow(db, z.ID); n != 7 {
		t.Errorf("online now %d, want 7 (heartbeat beats radacct 1)", n)
	}
}
