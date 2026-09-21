package routes

import (
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
)

// A method the CORS list omits is blocked by the browser before the request is
// sent, so the UI just shows a generic failure. This guards every route.
func TestCORSAllowsEveryMethodTheRoutesUse(t *testing.T) {
	app := fiber.New()
	Register(app)

	allowed := map[string]bool{}
	for _, m := range strings.Split(config.CORSAllowMethods, ",") {
		allowed[strings.TrimSpace(m)] = true
	}
	used := map[string]int{}
	for _, r := range app.GetRoutes(true) { // true = skip middleware-only entries
		used[r.Method]++
		if r.Method == "HEAD" {
			continue // added automatically for GET; browsers never preflight it
		}
		if !allowed[r.Method] {
			t.Errorf("route %s %s uses a method missing from CORSAllowMethods (%s)", r.Method, r.Path, config.CORSAllowMethods)
		}
	}
	if used["PATCH"] == 0 {
		t.Error("expected the API to register PATCH routes (organizations, staff, invoices …)")
	}
	if !allowed["PATCH"] {
		t.Error("PATCH must be allowed: the platform app edits organizations with it")
	}
}
