package routes

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// Every /reports/* endpoint on the real router refuses field agents and
// installers; the admin menu shows Reports only to owner/finance/zone managers.
func TestAllReportRoutes_RefuseFieldAgents(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Organization{}, &models.User{}, &models.Zone{}); err != nil {
		t.Fatal(err)
	}
	config.DB = db
	oldSecret, oldExp := config.Config.JWTSecret, config.Config.JWTExpiry
	config.Config.JWTSecret, config.Config.JWTExpiry = "routes-test-secret-routes-test-secret", time.Hour
	t.Cleanup(func() { config.Config.JWTSecret, config.Config.JWTExpiry = oldSecret, oldExp })
	db.Create(&models.Organization{Name: "a", Slug: "a"})

	app := fiber.New()
	Register(app)
	var paths []string
	for _, r := range app.GetRoutes(true) {
		if r.Method == "GET" && strings.HasPrefix(r.Path, "/api/v1/reports") {
			paths = append(paths, r.Path)
		}
	}
	if len(paths) < 6 {
		t.Fatalf("found only %d report routes: %v", len(paths), paths)
	}
	for _, role := range []string{"field_agent", "installer"} {
		u := models.User{Name: role, Email: role + "@x.test", Password: "x", Role: role, Status: "active", OrganizationID: 1}
		db.Create(&u)
		tok, _ := middleware.GenerateAdminToken(u.ID, role, nil, 1)
		for _, p := range paths {
			req := httptest.NewRequest("GET", p, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 403 {
				t.Errorf("%s GET %s: %d, want 403", role, p, resp.StatusCode)
			}
		}
	}
}
