package handlers

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/middleware"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/services"
	"github.com/zyranet/zyranet-api/utils"
	"golang.org/x/crypto/bcrypt"
)

// Home installs and stock. Rules (services/installs.go, services/stock.go):
//   - super_admin: everything.
//   - zone_manager: install jobs in their zone (and requests with no zone
//     yet); sees crews and stock, can't buy or write off stock.
//   - finance: reads jobs, stock and the cost report.
//   - installer: only their crew's jobs; marks them installed.
//   - field_agent: nothing here.

func installActor(c *fiber.Ctx) services.InstallActor {
	claims := middleware.GetClaims(c)
	a := services.InstallActor{OrgID: claims.OrganizationID, UserID: claims.UserID, Role: claims.Role, ZoneID: claims.ZoneID}
	var u struct {
		Name   string
		CrewID *uint
	}
	config.DB.Table("users").Select("name, crew_id").Where("id = ?", claims.UserID).Scan(&u)
	a.UserName = u.Name
	if a.Role == "installer" {
		a.CrewID = u.CrewID
	}
	return a
}

func canSeeInstalls(role string) bool {
	switch role {
	case "super_admin", "zone_manager", "finance", "installer":
		return true
	}
	return false
}

func installFail(c *fiber.Ctx, err error) error { return ledgerFail(c, err) }

func paramID(c *fiber.Ctx, name string) uint {
	n, _ := strconv.ParseUint(c.Params(name), 10, 64)
	return uint(n)
}

// parseEATTime accepts "2026-11-12T09:30" (EAT, from a datetime-local input),
// "2026-11-12" (09:00 EAT) or RFC3339.
func parseEATTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	for _, f := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(f, s, utils.EAT); err == nil {
			return t, true
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, utils.EAT); err == nil {
		return t.Add(9 * time.Hour), true
	}
	return time.Time{}, false
}

// ---- crews ----

type crewOut struct {
	models.InstallCrew
	Members []crewMember `json:"members"`
}

type crewMember struct {
	ID     uint    `json:"id"`
	Name   string  `json:"name"`
	Email  string  `json:"email"`
	Phone  *string `json:"phone"`
	Status string  `json:"status"`
}

// CrewIndex lists the ISP's crews with their installer logins.
func CrewIndex(c *fiber.Ctx) error {
	a := installActor(c)
	if !canSeeInstalls(a.Role) {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	q := config.DB.Where("organization_id = ?", a.OrgID)
	if a.Role == "installer" {
		if a.CrewID == nil {
			return utils.SuccessResponse(c, []crewOut{}, "")
		}
		q = q.Where("id = ?", *a.CrewID)
	}
	var crews []models.InstallCrew
	if err := q.Order("active DESC, name ASC").Find(&crews).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to load crews.", "", fiber.StatusInternalServerError)
	}
	out := make([]crewOut, 0, len(crews))
	if len(crews) > 0 {
		ids := make([]uint, len(crews))
		for i, cr := range crews {
			ids[i] = cr.ID
		}
		var members []struct {
			ID     uint
			Name   string
			Email  string
			Phone  *string
			Status string
			CrewID uint
		}
		config.DB.Table("users").Select("id, name, email, phone, status, crew_id").
			Where("organization_id = ? AND role = ? AND crew_id IN ? AND deleted_at IS NULL", a.OrgID, "installer", ids).
			Order("name").Scan(&members)
		byCrew := map[uint][]crewMember{}
		for _, m := range members {
			byCrew[m.CrewID] = append(byCrew[m.CrewID], crewMember{ID: m.ID, Name: m.Name, Email: m.Email, Phone: m.Phone, Status: m.Status})
		}
		for _, cr := range crews {
			ms := byCrew[cr.ID]
			if ms == nil {
				ms = []crewMember{}
			}
			out = append(out, crewOut{InstallCrew: cr, Members: ms})
		}
	}
	return utils.SuccessResponse(c, out, "")
}

type crewBody struct {
	Name       string `json:"name"`
	LeaderName string `json:"leader_name"`
	Phone      string `json:"phone"`
	Active     *bool  `json:"active"`
}

func (b *crewBody) clean() string {
	b.Name, b.LeaderName = strings.TrimSpace(b.Name), strings.TrimSpace(b.LeaderName)
	b.Phone = strings.TrimSpace(b.Phone)
	if b.Phone != "" {
		b.Phone = utils.FormatPhone(b.Phone)
	}
	if b.Name == "" || len(b.Name) > 120 || len(b.LeaderName) > 120 || len(b.Phone) > 20 {
		return "Give the crew a name (up to 120 letters)."
	}
	return ""
}

// CrewStore adds a crew (owner only).
func CrewStore(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" {
		return utils.ErrorResponse(c, "Only the owner can add crews.", "", fiber.StatusForbidden)
	}
	var b crewBody
	if err := c.BodyParser(&b); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if msg := b.clean(); msg != "" {
		return utils.ErrorResponse(c, msg, "", fiber.StatusUnprocessableEntity)
	}
	crew := models.InstallCrew{OrganizationID: a.OrgID, Name: b.Name, LeaderName: b.LeaderName, Phone: b.Phone, Active: true}
	if err := config.DB.Create(&crew).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to save the crew.", "", fiber.StatusInternalServerError)
	}
	auditAgent(c, "install_crew_created", "InstallCrew", crew.ID, crew)
	return utils.SuccessResponse(c, crew, "Crew added.", fiber.StatusCreated)
}

// CrewUpdate edits a crew or switches it off (owner only).
func CrewUpdate(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" {
		return utils.ErrorResponse(c, "Only the owner can edit crews.", "", fiber.StatusForbidden)
	}
	var crew models.InstallCrew
	if err := config.DB.Where("id = ? AND organization_id = ?", paramID(c, "id"), a.OrgID).First(&crew).Error; err != nil {
		return utils.ErrorResponse(c, "Crew not found.", "", fiber.StatusNotFound)
	}
	var b crewBody
	if err := c.BodyParser(&b); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	if msg := b.clean(); msg != "" {
		return utils.ErrorResponse(c, msg, "", fiber.StatusUnprocessableEntity)
	}
	upd := map[string]interface{}{"name": b.Name, "leader_name": b.LeaderName, "phone": b.Phone}
	if b.Active != nil {
		upd["active"] = *b.Active
	}
	config.DB.Model(&crew).Updates(upd)
	auditAgent(c, "install_crew_updated", "InstallCrew", crew.ID, upd)
	return utils.SuccessResponse(c, crew, "Crew saved.")
}

// CrewMemberStore creates an installer login for a crew (owner only). The
// installer sees only this crew's jobs.
func CrewMemberStore(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" {
		return utils.ErrorResponse(c, "Only the owner can add installer logins.", "", fiber.StatusForbidden)
	}
	var crew models.InstallCrew
	if err := config.DB.Where("id = ? AND organization_id = ?", paramID(c, "id"), a.OrgID).First(&crew).Error; err != nil {
		return utils.ErrorResponse(c, "Crew not found.", "", fiber.StatusNotFound)
	}
	var b struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&b); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	b.Name, b.Email = strings.TrimSpace(b.Name), strings.ToLower(strings.TrimSpace(b.Email))
	if b.Name == "" || !strings.Contains(b.Email, "@") {
		return utils.ErrorResponse(c, "Enter the installer's name and email.", "", fiber.StatusUnprocessableEntity)
	}
	if len(b.Password) < 8 {
		return utils.ErrorResponse(c, "The password needs at least 8 characters.", "", fiber.StatusUnprocessableEntity)
	}
	var n int64
	config.DB.Unscoped().Model(&models.User{}).Where("email = ?", b.Email).Count(&n)
	if n > 0 {
		return utils.ErrorResponse(c, "An account with this email already exists.", "", fiber.StatusConflict)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(b.Password), bcrypt.DefaultCost)
	if err != nil {
		return utils.ErrorResponse(c, "Password hashing failed.", "", fiber.StatusInternalServerError)
	}
	u := models.User{Name: b.Name, Email: b.Email, Password: string(hash), Role: "installer", OrganizationID: a.OrgID, Status: "active"}
	if p := strings.TrimSpace(b.Phone); p != "" {
		p = utils.FormatPhone(p)
		u.Phone = &p
	}
	if err := config.DB.Create(&u).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to create the login.", "", fiber.StatusInternalServerError)
	}
	config.DB.Table("users").Where("id = ?", u.ID).Update("crew_id", crew.ID)
	auditAgent(c, "installer_created", "User", u.ID, fiber.Map{"crew_id": crew.ID, "email": u.Email})
	return utils.SuccessResponse(c, fiber.Map{"id": u.ID, "name": u.Name, "email": u.Email, "phone": u.Phone, "status": u.Status,
		"login_url": orgLoginURL(a.OrgID)}, "Installer login created.", fiber.StatusCreated)
}

// CrewMemberRemove switches an installer login off and takes it off the crew.
func CrewMemberRemove(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" {
		return utils.ErrorResponse(c, "Only the owner can remove installer logins.", "", fiber.StatusForbidden)
	}
	res := config.DB.Table("users").
		Where("id = ? AND organization_id = ? AND role = ? AND crew_id = ?", paramID(c, "userId"), a.OrgID, "installer", paramID(c, "id")).
		Updates(map[string]interface{}{"crew_id": nil, "status": "inactive"})
	if res.RowsAffected != 1 {
		return utils.ErrorResponse(c, "Installer not found on this crew.", "", fiber.StatusNotFound)
	}
	middleware.RevokeAdminSessions(config.DB, paramID(c, "userId"))
	auditAgent(c, "installer_removed", "User", paramID(c, "userId"), fiber.Map{"crew_id": paramID(c, "id")})
	return utils.SuccessResponse(c, nil, "Installer removed and signed out.")
}

// ---- jobs ----

// InstallJobIndex lists jobs the caller may see. ?status= (comma list),
// crew_id, zone_id, q (name/phone/location/serial), page, per_page.
func InstallJobIndex(c *fiber.Ctx) error {
	a := installActor(c)
	if !canSeeInstalls(a.Role) {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	q := services.InstallJobScope(config.DB.Model(&models.InstallJob{}), a)
	if s := strings.TrimSpace(c.Query("status")); s != "" && s != "all" {
		if s == "open" {
			q = q.Where("status IN ?", []string{models.InstallRequested, models.InstallScheduled, models.InstallAssigned})
		} else {
			q = q.Where("status IN ?", strings.Split(s, ","))
		}
	}
	if v := c.QueryInt("crew_id"); v > 0 {
		q = q.Where("crew_id = ?", v)
	}
	if v := c.QueryInt("zone_id"); v > 0 {
		q = q.Where("zone_id = ?", v)
	}
	if s := strings.TrimSpace(c.Query("q")); s != "" {
		like := "%" + s + "%"
		q = q.Where("(customer_name LIKE ? OR phone LIKE ? OR location LIKE ? OR ont_serial LIKE ?)", like, like, like, like)
	}
	var total int64
	q.Count(&total)
	page, perPage := utils.ParsePage(c)
	var jobs []models.InstallJob
	order := "CASE status WHEN 'assigned' THEN 0 WHEN 'scheduled' THEN 1 WHEN 'requested' THEN 2 WHEN 'installed' THEN 3 ELSE 4 END, COALESCE(scheduled_for, requested_at) ASC"
	if c.Query("sort") == "recent" {
		order = "id DESC"
	}
	if err := q.Preload("Crew").Preload("Zone").Preload("Package").Order(order).
		Offset(utils.Offset(page, perPage)).Limit(perPage).Find(&jobs).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to load install jobs.", "", fiber.StatusInternalServerError)
	}
	// Counts per status for the board tabs (same scope, no status filter).
	var counts []struct {
		Status string
		N      int
	}
	services.InstallJobScope(config.DB.Model(&models.InstallJob{}), a).Select("status, COUNT(*) AS n").Group("status").Scan(&counts)
	cm := map[string]int{}
	for _, r := range counts {
		cm[r.Status] = r.N
	}
	return c.JSON(fiber.Map{"success": true, "data": jobs, "counts": cm,
		"meta": fiber.Map{"total": total, "page": page, "per_page": perPage}})
}

// InstallJobShow returns one job with its timeline and materials.
func InstallJobShow(c *fiber.Ctx) error {
	a := installActor(c)
	if !canSeeInstalls(a.Role) {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	var job models.InstallJob
	if err := services.InstallJobScope(config.DB.Model(&models.InstallJob{}), a).
		Preload("Crew").Preload("Zone").Preload("Package").First(&job, paramID(c, "id")).Error; err != nil {
		return utils.ErrorResponse(c, "Install job not found.", "", fiber.StatusNotFound)
	}
	var events []models.InstallJobEvent
	config.DB.Where("job_id = ?", job.ID).Order("id ASC").Find(&events)
	var moves []models.StockMove
	config.DB.Preload("StockItem").Where("job_id = ? AND organization_id = ?", job.ID, a.OrgID).Order("id ASC").Find(&moves)
	out := fiber.Map{"job": job, "events": events, "materials": moves, "status_label": services.InstallStatusLabel(job.Status)}
	if job.CustomerID != nil && a.Role != "installer" {
		var cust models.Customer
		if config.DB.Select("id, name, account_number, pppoe_username, status, expires_at").First(&cust, *job.CustomerID).Error == nil {
			out["customer"] = fiber.Map{"id": cust.ID, "name": cust.Name, "account_number": cust.AccountNumber,
				"pppoe_username": cust.PPPoEUsername, "status": cust.Status, "expires_at": cust.ExpiresAt}
		}
	}
	return utils.SuccessResponse(c, out, "")
}

// InstallJobStore records a new install request.
func InstallJobStore(c *fiber.Ctx) error {
	var in services.InstallJobInput
	if err := c.BodyParser(&in); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	job, err := services.CreateInstallJob(config.DB, installActor(c), in, time.Now())
	if err != nil {
		return installFail(c, err)
	}
	return utils.SuccessResponse(c, job, "Install request added.", fiber.StatusCreated)
}

// InstallJobUpdate edits a job's details before install.
func InstallJobUpdate(c *fiber.Ctx) error {
	var in services.InstallJobInput
	if err := c.BodyParser(&in); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	job, err := services.UpdateInstallJob(config.DB, installActor(c), paramID(c, "id"), in)
	if err != nil {
		return installFail(c, err)
	}
	return utils.SuccessResponse(c, job, "Saved.")
}

// InstallJobSchedule sets the visit date: {scheduled_for}.
func InstallJobSchedule(c *fiber.Ctx) error {
	var b struct {
		ScheduledFor string `json:"scheduled_for"`
	}
	if err := c.BodyParser(&b); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	when, ok := parseEATTime(b.ScheduledFor)
	if !ok {
		return utils.ErrorResponse(c, "Pick the visit date and time.", "", fiber.StatusUnprocessableEntity)
	}
	job, err := services.ScheduleInstallJob(config.DB, installActor(c), paramID(c, "id"), when)
	if err != nil {
		return installFail(c, err)
	}
	return utils.SuccessResponse(c, job, "Visit scheduled.")
}

// InstallJobAssign gives the job to a crew: {crew_id, scheduled_for?}.
func InstallJobAssign(c *fiber.Ctx) error {
	var b struct {
		CrewID       uint   `json:"crew_id"`
		ScheduledFor string `json:"scheduled_for"`
	}
	if err := c.BodyParser(&b); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	var when *time.Time
	if strings.TrimSpace(b.ScheduledFor) != "" {
		t, ok := parseEATTime(b.ScheduledFor)
		if !ok {
			return utils.ErrorResponse(c, "The visit date is not valid.", "", fiber.StatusUnprocessableEntity)
		}
		when = &t
	}
	job, err := services.AssignInstallJob(config.DB, installActor(c), paramID(c, "id"), b.CrewID, when)
	if err != nil {
		return installFail(c, err)
	}
	return utils.SuccessResponse(c, job, "Crew assigned.")
}

// InstallJobCancel cancels with a reason: {reason}.
func InstallJobCancel(c *fiber.Ctx) error {
	var b struct {
		Reason string `json:"reason"`
	}
	if err := c.BodyParser(&b); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	job, err := services.CancelInstallJob(config.DB, installActor(c), paramID(c, "id"), b.Reason)
	if err != nil {
		return installFail(c, err)
	}
	return utils.SuccessResponse(c, job, "Install cancelled.")
}

// InstallJobInstalled: the crew finished. {ont_serial, materials[], labour_cost}.
func InstallJobInstalled(c *fiber.Ctx) error {
	var in services.MarkInstalledInput
	if err := c.BodyParser(&in); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	a := installActor(c)
	job, cust, err := services.MarkInstalled(config.DB, a, paramID(c, "id"), in, time.Now())
	if err != nil {
		return installFail(c, err)
	}
	out := fiber.Map{"job": job}
	if a.Role != "installer" {
		out["customer"] = fiber.Map{"id": cust.ID, "account_number": cust.AccountNumber, "pppoe_username": cust.PPPoEUsername}
	} else {
		// The crew needs the PPPoE login to set up the ONT on site.
		out["customer"] = fiber.Map{"pppoe_username": cust.PPPoEUsername, "pppoe_password": cust.PPPoEPassword}
	}
	return utils.SuccessResponse(c, out, "Installed. The home account is ready and switches on with the first payment.")
}

// InstallOptions is what the forms need: zones, plans, active crews, and
// stock to pick from (in-stock ONT serials and materials).
func InstallOptions(c *fiber.Ctx) error {
	a := installActor(c)
	if !canSeeInstalls(a.Role) {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	type opt struct {
		ID   uint   `json:"id"`
		Name string `json:"name"`
	}
	type pkgOpt struct {
		ID           uint    `json:"id"`
		ZoneID       uint    `json:"zone_id"`
		Name         string  `json:"name"`
		Type         string  `json:"type"`
		Price        float64 `json:"price"`
		BillingCycle string  `json:"billing_cycle"`
	}
	type itemOpt struct {
		ID            uint    `json:"id"`
		Name          string  `json:"name"`
		Unit          string  `json:"unit"`
		OnHand        float64 `json:"on_hand"`
		UnitCost      float64 `json:"unit_cost"`
		TracksSerials bool    `json:"tracks_serials"`
	}
	type ontOpt struct {
		ID          uint   `json:"id"`
		Serial      string `json:"serial"`
		StockItemID uint   `json:"stock_item_id"`
	}
	zones, pkgs, crews := []opt{}, []pkgOpt{}, []opt{}
	if a.Role != "installer" {
		zq := config.DB.Table("zones").Select("id, name").Where("organization_id = ? AND deleted_at IS NULL", a.OrgID)
		if a.Role == "zone_manager" && a.ZoneID != nil {
			zq = zq.Where("id = ?", *a.ZoneID)
		}
		zq.Order("name").Scan(&zones)
		zids := make([]uint, len(zones))
		for i, z := range zones {
			zids[i] = z.ID
		}
		if len(zids) > 0 {
			config.DB.Table("packages").Select("id, zone_id, name, type, price, billing_cycle").
				Where("zone_id IN ? AND deleted_at IS NULL AND status = ?", zids, "active").Order("type DESC, price").Scan(&pkgs)
		}
		config.DB.Table("install_crews").Select("id, name").Where("organization_id = ? AND active = ?", a.OrgID, true).Order("name").Scan(&crews)
	}
	items, onts := []itemOpt{}, []ontOpt{}
	config.DB.Table("stock_items").Select("id, name, unit, on_hand, unit_cost, tracks_serials").
		Where("organization_id = ? AND active = ?", a.OrgID, true).Order("name").Scan(&items)
	config.DB.Table("ont_units").Select("id, serial, stock_item_id").
		Where("organization_id = ? AND status = ?", a.OrgID, models.OntInStock).Order("serial").Scan(&onts)
	return utils.SuccessResponse(c, fiber.Map{"zones": zones, "packages": pkgs, "crews": crews, "stock_items": items, "onts": onts}, "")
}

// InstallSummary: GET /installs/summary?from=YYYY-MM-DD&to=YYYY-MM-DD (EAT days).
// Default: the last 8 weeks up to today.
func InstallSummary(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" && a.Role != "finance" && a.Role != "zone_manager" {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	now := time.Now()
	today := utils.Kenya(now)
	to, err := time.ParseInLocation("2006-01-02", c.Query("to", today.Format("2006-01-02")), utils.EAT)
	if err != nil {
		return utils.ErrorResponse(c, "to must be YYYY-MM-DD.", "", fiber.StatusUnprocessableEntity)
	}
	defFrom := services.WeekStartEAT(to).AddDate(0, 0, -7*7)
	from, err := time.ParseInLocation("2006-01-02", c.Query("from", defFrom.Format("2006-01-02")), utils.EAT)
	if err != nil {
		return utils.ErrorResponse(c, "from must be YYYY-MM-DD.", "", fiber.StatusUnprocessableEntity)
	}
	if from.After(to) {
		return utils.ErrorResponse(c, "from must be on or before to.", "", fiber.StatusUnprocessableEntity)
	}
	if to.Sub(from).Hours()/24 > 366 {
		from = to.AddDate(0, 0, -366)
	}
	out, err := services.BuildInstallSummary(config.DB, a, from, to, now)
	if err != nil {
		return utils.ErrorResponse(c, "Failed to build the install summary.", "", fiber.StatusInternalServerError)
	}
	return utils.SuccessResponse(c, out, "")
}

// InstallSettingsShow / InstallSettingsUpdate: {free_first_day}.
func InstallSettingsShow(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" && a.Role != "zone_manager" {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	var s models.InstallSetting
	config.DB.Where("organization_id = ?", a.OrgID).First(&s)
	return utils.SuccessResponse(c, fiber.Map{"free_first_day": s.FreeFirstDay}, "")
}

func InstallSettingsUpdate(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" {
		return utils.ErrorResponse(c, "Only the owner can change install settings.", "", fiber.StatusForbidden)
	}
	var b struct {
		FreeFirstDay bool `json:"free_first_day"`
	}
	if err := c.BodyParser(&b); err != nil {
		return utils.ErrorResponse(c, "Invalid request body.", "", fiber.StatusBadRequest)
	}
	var s models.InstallSetting
	if config.DB.Where("organization_id = ?", a.OrgID).First(&s).Error != nil {
		s = models.InstallSetting{OrganizationID: a.OrgID}
	}
	s.FreeFirstDay = b.FreeFirstDay
	if err := config.DB.Save(&s).Error; err != nil {
		return utils.ErrorResponse(c, "Failed to save.", "", fiber.StatusInternalServerError)
	}
	auditAgent(c, "install_settings_updated", "InstallSetting", s.ID, b)
	return utils.SuccessResponse(c, fiber.Map{"free_first_day": s.FreeFirstDay}, "Saved.")
}

// InstallCheckLive runs the first-payment check now for this ISP's jobs
// (it also runs by itself every 2 minutes).
func InstallCheckLive(c *fiber.Ctx) error {
	a := installActor(c)
	if a.Role != "super_admin" {
		return utils.ErrorResponse(c, "Forbidden.", "", fiber.StatusForbidden)
	}
	var ids []uint
	config.DB.Model(&models.InstallJob{}).Where("organization_id = ? AND status = ? AND customer_id IS NOT NULL", a.OrgID, models.InstallInstalled).Pluck("customer_id", &ids)
	moved := 0
	for _, id := range ids {
		id := id
		n, _ := services.MarkInstallsLive(config.DB, &id)
		moved += n
	}
	return utils.SuccessResponse(c, fiber.Map{"moved_to_live": moved}, "")
}
