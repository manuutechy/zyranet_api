package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func restZoneFor(t *testing.T, srv *httptest.Server) *models.Zone {
	t.Helper()
	old := config.Config.MikroTikAllowPrivateIPs
	config.Config.MikroTikAllowPrivateIPs = true
	t.Cleanup(func() { config.Config.MikroTikAllowPrivateIPs = old })
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port := u.Hostname(), u.Port()
	var p int
	fmtSscanf(port, &p)
	return &models.Zone{RouterIP: host, RouterPort: p, ConnectionType: "rest"}
}

func fmtSscanf(s string, out *int) { // avoids importing fmt just for this
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return
		}
		n = n*10 + int(c-'0')
	}
	*out = n
}

// Regression: ExecCommand on a REST zone used to return a fake success
// string without contacting the router at all.
func TestExecCommandREST_PrintQueryActuallyHitsTheRouter(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode([]map[string]interface{}{{"name": "ether1", "running": "true"}})
	}))
	defer srv.Close()
	zone := restZoneFor(t, srv)

	out, err := (&MikroTikService{}).ExecCommand(zone, "/interface/print")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/rest/interface" {
		t.Errorf("request path = %q, want /rest/interface — the command must actually reach the router", gotPath)
	}
	if !strings.Contains(out, "ether1") || !strings.Contains(out, "running") {
		t.Errorf("output should contain the router's real reply, got %q", out)
	}
	if strings.Contains(out, "Dispatched") {
		t.Error("must not return the old canned/fake response")
	}
}

func TestExecCommandREST_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	}))
	defer srv.Close()
	out, err := (&MikroTikService{}).ExecCommand(restZoneFor(t, srv), "/ppp/active/print")
	if err != nil || !strings.Contains(out, "no results") {
		t.Errorf("out=%q err=%v", out, err)
	}
}

// Non-print (write) commands must be refused, never silently faked.
func TestExecCommandREST_WriteCommandsAreRefusedNotFaked(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	zone := restZoneFor(t, srv)

	for _, cmd := range []string{"/ip/firewall/nat/add", "/system/reboot", "", "/"} {
		out, err := (&MikroTikService{}).ExecCommand(zone, cmd)
		if err == nil {
			t.Errorf("command %q should be refused, got output %q", cmd, out)
		}
	}
	if hit {
		t.Error("a refused command must never reach the router")
	}
}

func TestExecCommandREST_RouterUnreachable(t *testing.T) {
	zone := &models.Zone{RouterIP: "10.200.0.253", ConnectionType: "rest"} // unused address, nothing listening
	old := config.Config.MikroTikAllowPrivateIPs
	config.Config.MikroTikAllowPrivateIPs = true
	t.Cleanup(func() { config.Config.MikroTikAllowPrivateIPs = old })
	if _, err := (&MikroTikService{}).ExecCommand(zone, "/system/resource/print"); err == nil {
		t.Error("an unreachable router must return an error, not a fake success")
	}
}
