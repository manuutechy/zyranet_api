package services

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
)

const (
	testPubA = "yJNd1lsQXskvKii3wbbiu7tmTl0qr9MhQPmkEeMbBxE="
	testPubB = "AbCd1lsQXskvKii3wbbiu7tmTl0qr9MhQPmkEeMbBxE="
)

// fakeHub records each peer list handed to the hub.
func fakeHub(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	oldPush, oldHere := tunnelPush, tunnelHubHere
	tunnelPush = func(lines []string) error { calls = append(calls, strings.Join(lines, "|")); return nil }
	tunnelHubHere = func() bool { return true }
	t.Cleanup(func() { tunnelPush, tunnelHubHere = oldPush, oldHere })
	return &calls
}

func TestEnsureZoneTunnel_AssignsAddressAndLogin(t *testing.T) {
	db := setupRouterAccessDB(t)
	taken := "10.200.0.10"
	db.Create(&models.Zone{Name: "Oxygen", Location: "L", OrganizationID: 1, ProvisionToken: "t0", TunnelIP: &taken})
	owner, ownerPw := "admin", "OwnerSetThis1"
	z := models.Zone{Name: "New site", Location: "L", OrganizationID: 1, ProvisionToken: "tok", RouterUsername: &owner, RouterPassword: &ownerPw}
	db.Create(&z)
	fresh := models.Zone{Name: "Fresh", Location: "L", OrganizationID: 1, ProvisionToken: "tok2"}
	db.Create(&fresh)
	noToken := models.Zone{Name: "Not provisioned", Location: "L", OrganizationID: 1}
	db.Create(&noToken)

	if !EnsureZoneTunnel(db, &z) || strVal(z.TunnelIP) != "10.200.0.11" {
		t.Fatalf("want the next free address 10.200.0.11, got %q", strVal(z.TunnelIP))
	}
	if strVal(z.RouterUsername) != "admin" || strVal(z.RouterPassword) != ownerPw {
		t.Error("an owner-entered router login must be left alone")
	}
	if !EnsureZoneTunnel(db, &fresh) || strVal(fresh.TunnelIP) != "10.200.0.12" {
		t.Fatalf("fresh: %q", strVal(fresh.TunnelIP))
	}
	if strVal(fresh.RouterUsername) != tunnelAPIUser || !credPassRe.MatchString(strVal(fresh.RouterPassword)) || len(strVal(fresh.RouterPassword)) < 24 {
		t.Errorf("fresh zone should get the server's own login, got %q / %d chars", strVal(fresh.RouterUsername), len(strVal(fresh.RouterPassword)))
	}
	if EnsureZoneTunnel(db, &fresh) {
		t.Error("second call must change nothing")
	}
	noToken.ProvisionToken = "" // (zones get a token when created; this one lost it)
	if EnsureZoneTunnel(db, &noToken) || noToken.TunnelIP != nil {
		t.Error("a zone that was never provisioned gets no tunnel")
	}
	// Saved, and the sync script now carries the tunnel + login.
	var saved models.Zone
	db.First(&saved, fresh.ID)
	if strVal(saved.TunnelIP) != "10.200.0.12" {
		t.Fatal("tunnel address not saved")
	}
	script, _ := NewMikroTikScriptService().GenerateSyncScript(fresh.ID)
	for _, want := range []string{`address=10.200.0.12/24 interface=zyra-wg`, `/user add name=zyranet-api group=zyranet-api`, `wg-key/`} {
		if !strings.Contains(script, want) {
			t.Errorf("sync script missing %q", want)
		}
	}
}

func TestRegisterTunnelPeer(t *testing.T) {
	db := setupRouterAccessDB(t)
	calls := fakeHub(t)
	ip := "10.200.0.10"
	z := models.Zone{Name: "Oxygen", Location: "L", OrganizationID: 1, ProvisionToken: "tok", TunnelIP: &ip, RouterIP: "196.96.165.80"}
	db.Create(&z)

	if err := RegisterTunnelPeer(db, &z, testPubA); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0] != testPubA+" 10.200.0.10" {
		t.Fatalf("hub calls: %v", *calls)
	}
	var saved models.Zone
	db.First(&saved, z.ID)
	if strVal(saved.TunnelPubKey) != testPubA || saved.TunnelPeerAt == nil || saved.RouterIP != "10.200.0.10" {
		t.Errorf("saved: key=%q peerAt=%v routerIP=%q", strVal(saved.TunnelPubKey), saved.TunnelPeerAt, saved.RouterIP)
	}
	// The router reports the same key every minute: the hub is not touched again.
	for i := 0; i < 3; i++ {
		if err := RegisterTunnelPeer(db, &saved, testPubA); err != nil {
			t.Fatal(err)
		}
	}
	if len(*calls) != 1 {
		t.Errorf("same key re-sent to the hub %d times", len(*calls)-1)
	}
	// Router was reset: a new key replaces the old one at the same address.
	if err := RegisterTunnelPeer(db, &saved, testPubB); err != nil || len(*calls) != 2 || (*calls)[1] != testPubB+" 10.200.0.10" {
		t.Errorf("new key: err=%v calls=%v", err, *calls)
	}
	// A second router joins: the hub gets both, each on its own address.
	ip2 := "10.200.0.11"
	z2 := models.Zone{Name: "Second", Location: "L", OrganizationID: 1, ProvisionToken: "tok2", TunnelIP: &ip2}
	db.Create(&z2)
	if err := RegisterTunnelPeer(db, &z2, testPubA); err != nil || (*calls)[len(*calls)-1] != testPubB+" 10.200.0.10|"+testPubA+" 10.200.0.11" {
		t.Errorf("two routers: err=%v last=%q", err, (*calls)[len(*calls)-1])
	}
	before := len(*calls)
	// Bad input never reaches the hub.
	if RegisterTunnelPeer(db, &saved, "not-a-key; rm -rf /") == nil {
		t.Error("malformed key accepted")
	}
	noIP := models.Zone{Name: "No tunnel", Location: "L", OrganizationID: 1}
	db.Create(&noIP)
	if RegisterTunnelPeer(db, &noIP, testPubA) == nil || len(*calls) != before {
		t.Error("zone without a tunnel address must be refused")
	}
}

func TestRegisterTunnelPeer_HubFailureSavesNothing(t *testing.T) {
	db := setupRouterAccessDB(t)
	fakeHub(t)
	tunnelPush = func([]string) error { return errors.New("the hub did not confirm") }
	ip := "10.200.0.10"
	z := models.Zone{Name: "Oxygen", Location: "L", OrganizationID: 1, ProvisionToken: "tok", TunnelIP: &ip, RouterIP: "196.96.165.80"}
	db.Create(&z)
	if RegisterTunnelPeer(db, &z, testPubA) == nil {
		t.Fatal("hub failure must be reported so the router retries")
	}
	var saved models.Zone
	db.First(&saved, z.ID)
	if saved.TunnelPeerAt != nil || saved.RouterIP != "196.96.165.80" {
		t.Errorf("nothing may be marked done: %+v / %s", saved.TunnelPeerAt, saved.RouterIP)
	}
	// The key is remembered, so the router's next report retries and succeeds.
	calls := fakeHub(t)
	if err := RegisterTunnelPeer(db, &saved, testPubA); err != nil || len(*calls) != 1 {
		t.Errorf("retry: err=%v calls=%v", err, *calls)
	}
}

func TestReconcileTunnelPeers(t *testing.T) {
	db := setupRouterAccessDB(t)
	calls := fakeHub(t)
	ip, pub := "10.200.0.10", testPubA
	db.Create(&models.Zone{Name: "Oxygen", Location: "L", OrganizationID: 1, TunnelIP: &ip, TunnelPubKey: &pub})
	db.Create(&models.Zone{Name: "No key yet", Location: "L", OrganizationID: 1})
	ReconcileTunnelPeers(db)
	if len(*calls) != 1 || (*calls)[0] != testPubA+" 10.200.0.10" {
		t.Errorf("calls: %v", *calls)
	}
}

// The real hand-off: the list is written to the file and only counts once the
// root side has written its receipt (the file's SHA-256).
func TestPushTunnelPeers_WaitsForTheHubReceipt(t *testing.T) {
	path := t.TempDir() + "/peers"
	t.Setenv("WG_PEER_FILE", path)
	lines := []string{testPubA + " 10.200.0.10"}
	start := time.Now()
	if err := pushTunnelPeers(lines); err == nil || time.Since(start) < tunnelAckWait {
		t.Fatalf("no receipt → must fail after waiting, got %v after %s", err, time.Since(start))
	}
	body, _ := os.ReadFile(path)
	if string(body) != testPubA+" 10.200.0.10\n" {
		t.Fatalf("file: %q", body)
	}
	// The root side applies it and leaves the receipt.
	os.WriteFile(path+".applied", []byte(fmt.Sprintf("%x\n", sha256.Sum256(body))), 0o644)
	start = time.Now()
	if err := pushTunnelPeers(lines); err != nil || time.Since(start) > time.Second {
		t.Errorf("already applied → instant success, got %v after %s", err, time.Since(start))
	}
	// A different list is not covered by the old receipt.
	if err := pushTunnelPeers(append(lines, testPubB+" 10.200.0.11")); err == nil {
		t.Error("stale receipt accepted for a new list")
	}
}
