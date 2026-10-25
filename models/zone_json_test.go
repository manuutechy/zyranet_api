package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestZoneJSONHidesRouterPassword(t *testing.T) {
	pw := "s3cret-router-pass"
	z := Zone{ID: 9, Name: "Oxygen", RouterPassword: &pw}
	b, err := json.Marshal(z)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), pw) {
		t.Fatalf("router password leaked in zone JSON: %s", b)
	}
	if !strings.Contains(string(b), `"has_router_password":true`) || !strings.Contains(string(b), `"name":"Oxygen"`) {
		t.Fatalf("unexpected zone JSON: %s", b)
	}
	// Pointers and nested zones (payments preload Zone) go through it too.
	b, _ = json.Marshal(struct{ Zone *Zone }{&z})
	if strings.Contains(string(b), pw) {
		t.Fatalf("router password leaked via pointer: %s", b)
	}
	// Request bodies still decode it.
	var in Zone
	if err := json.Unmarshal([]byte(`{"router_password":"abc"}`), &in); err != nil || in.RouterPassword == nil || *in.RouterPassword != "abc" {
		t.Fatalf("router_password no longer decodes: %v %v", err, in.RouterPassword)
	}
}
