package services

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Home installs: request → scheduled → assigned → installed → live.
//
// Money rule: every cost is worked out in whole cents (int64) and only turned
// back into a KES decimal when stored, so a week of 300 installs adds up to
// exactly the sum of its parts. Stock quantities are kept in hundredths
// (0.01 m of cable) the same way.
//
// Stock rule: stock_items.on_hand is moved by one conditional UPDATE in the
// same transaction as the stock_moves row (the AdjustCredit pattern), so two
// crews using the last ONT at the same moment can't both succeed and stock
// can never go below zero. On hand always equals the sum of signed moves.

// InstallActor is who is acting, from the session only.
type InstallActor struct {
	OrgID    uint
	UserID   uint
	UserName string
	Role     string
	ZoneID   *uint // zone_manager's zone
	CrewID   *uint // installer's crew
}

func installErr(status int, format string, args ...interface{}) error {
	return &LedgerError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// ErrInstallJobNotFound hides other ISPs' / crews' / zones' jobs.
var ErrInstallJobNotFound = &LedgerError{Status: 404, Msg: "Install job not found."}

// ---- cents / hundredths ----

func toCents(kes float64) int64       { return int64(math.Round(kes * 100)) }
func centsToKES(c int64) float64      { return float64(c) / 100 }
func toHundredths(q float64) int64    { return int64(math.Round(q * 100)) }
func hundredthsToQty(h int64) float64 { return float64(h) / 100 }

// lineCents is quantity × unit cost, rounded to the cent.
func lineCents(qtyHundredths, unitCents int64) int64 {
	// qty/100 × cents, rounded half away from zero.
	n := qtyHundredths * unitCents
	if n >= 0 {
		return (n + 50) / 100
	}
	return -((-n + 50) / 100)
}

// weightedAverageCents is the new average unit cost after buying more.
func weightedAverageCents(onHandH, avgCents, buyH, buyCents int64) int64 {
	if onHandH < 0 {
		onHandH = 0
	}
	total := onHandH + buyH
	if total <= 0 {
		return buyCents
	}
	v := float64(onHandH*avgCents+buyH*buyCents) / float64(total)
	return int64(math.Round(v))
}

const (
	maxLabourCents   = 100_000_00 // KES 100,000 per install: a typo guard
	maxUnitCostCents = 1_000_000_00
	maxQtyHundredths = 1_000_000_00 // 1,000,000 units in one move
)

// ---- transitions ----

var installTransitions = map[string][]string{
	models.InstallRequested: {models.InstallScheduled, models.InstallAssigned, models.InstallCancelled},
	models.InstallScheduled: {models.InstallScheduled, models.InstallAssigned, models.InstallCancelled},
	models.InstallAssigned:  {models.InstallScheduled, models.InstallAssigned, models.InstallInstalled, models.InstallCancelled},
	models.InstallInstalled: {models.InstallLive},
	models.InstallLive:      {},
	models.InstallCancelled: {},
}

// CanInstallTransition reports whether a job may move from one status to another.
func CanInstallTransition(from, to string) bool {
	for _, s := range installTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// InstallStatusLabel is the plain-English name of a status.
func InstallStatusLabel(s string) string {
	switch s {
	case models.InstallRequested:
		return "Requested"
	case models.InstallScheduled:
		return "Scheduled"
	case models.InstallAssigned:
		return "Crew assigned"
	case models.InstallInstalled:
		return "Installed, waiting for first payment"
	case models.InstallLive:
		return "Live (paying)"
	case models.InstallCancelled:
		return "Cancelled"
	}
	return s
}

// ---- visibility ----

// InstallJobScope narrows a jobs query to what the actor may see.
func InstallJobScope(q *gorm.DB, a InstallActor) *gorm.DB {
	q = q.Where("install_jobs.organization_id = ?", a.OrgID)
	switch a.Role {
	case "super_admin", "finance":
		return q
	case "zone_manager":
		if a.ZoneID == nil {
			return q.Where("1 = 0")
		}
		return q.Where("(install_jobs.zone_id = ? OR install_jobs.zone_id IS NULL)", *a.ZoneID)
	case "installer":
		if a.CrewID == nil {
			return q.Where("1 = 0")
		}
		return q.Where("install_jobs.crew_id = ?", *a.CrewID)
	}
	return q.Where("1 = 0")
}

// CanManageInstalls: the office roles that create, schedule, assign, cancel.
func CanManageInstalls(role string) bool { return role == "super_admin" || role == "zone_manager" }

func loadJobForUpdate(tx *gorm.DB, a InstallActor, jobID uint) (*models.InstallJob, error) {
	var job models.InstallJob
	err := InstallJobScope(tx.Clauses(clause.Locking{Strength: "UPDATE"}), a).First(&job, jobID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInstallJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func addJobEvent(tx *gorm.DB, a *InstallActor, jobID uint, from, to, action, note string) error {
	ev := models.InstallJobEvent{JobID: jobID, FromStatus: from, ToStatus: to, Action: action, Note: clip(note, 500)}
	if a != nil {
		uid := a.UserID
		ev.UserID = &uid
		ev.UserName = a.UserName
	} else {
		ev.UserName = "System"
	}
	return tx.Create(&ev).Error
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// zoneInOrg / packageForZone check references belong to the ISP.
func installZoneOK(tx *gorm.DB, orgID, zoneID uint) bool {
	var n int64
	tx.Model(&models.Zone{}).Where("id = ? AND organization_id = ?", zoneID, orgID).Count(&n)
	return n > 0
}

func installPackageOK(tx *gorm.DB, zoneID, pkgID uint) bool {
	var n int64
	tx.Model(&models.Package{}).Where("id = ? AND zone_id = ?", pkgID, zoneID).Count(&n)
	return n > 0
}

func crewOK(tx *gorm.DB, orgID, crewID uint) bool {
	var n int64
	tx.Model(&models.InstallCrew{}).Where("id = ? AND organization_id = ? AND active = ?", crewID, orgID, true).Count(&n)
	return n > 0
}

// ---- jobs ----

// InstallJobInput is a new request / an edit of its details.
type InstallJobInput struct {
	CustomerName  string   `json:"customer_name"`
	Phone         string   `json:"phone"`
	Location      string   `json:"location"`
	Lat           *float64 `json:"lat"`
	Lng           *float64 `json:"lng"`
	ZoneID        *uint    `json:"zone_id"`
	PlanPackageID *uint    `json:"plan_package_id"`
	Notes         string   `json:"notes"`
}

func (in *InstallJobInput) clean(tx *gorm.DB, a InstallActor) error {
	in.CustomerName = clip(in.CustomerName, 255)
	in.Location = clip(in.Location, 255)
	in.Notes = clip(in.Notes, 5000)
	if in.CustomerName == "" {
		return installErr(422, "Enter the customer's name.")
	}
	in.Phone = utils.FormatPhone(strings.TrimSpace(in.Phone))
	if len(in.Phone) < 9 || len(in.Phone) > 15 {
		return installErr(422, "Enter a valid phone number.")
	}
	if (in.Lat == nil) != (in.Lng == nil) {
		return installErr(422, "GPS needs both latitude and longitude.")
	}
	if in.Lat != nil && (*in.Lat < -90 || *in.Lat > 90 || *in.Lng < -180 || *in.Lng > 180) {
		return installErr(422, "GPS position is not valid.")
	}
	if in.ZoneID != nil && *in.ZoneID == 0 {
		in.ZoneID = nil
	}
	if in.PlanPackageID != nil && *in.PlanPackageID == 0 {
		in.PlanPackageID = nil
	}
	if a.Role == "zone_manager" {
		if in.ZoneID == nil || a.ZoneID == nil || *in.ZoneID != *a.ZoneID {
			in.ZoneID = a.ZoneID
		}
	}
	if in.ZoneID != nil && !installZoneOK(tx, a.OrgID, *in.ZoneID) {
		return installErr(422, "That zone is not one of yours.")
	}
	if in.PlanPackageID != nil {
		if in.ZoneID == nil {
			return installErr(422, "Pick the zone before the plan.")
		}
		if !installPackageOK(tx, *in.ZoneID, *in.PlanPackageID) {
			return installErr(422, "That plan is not sold in this zone.")
		}
	}
	return nil
}

// CreateInstallJob records a new install request.
func CreateInstallJob(db *gorm.DB, a InstallActor, in InstallJobInput, now time.Time) (*models.InstallJob, error) {
	if !CanManageInstalls(a.Role) {
		return nil, installErr(403, "Only the owner or a zone manager can add install requests.")
	}
	var job models.InstallJob
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := in.clean(tx, a); err != nil {
			return err
		}
		job = models.InstallJob{
			OrganizationID: a.OrgID, ZoneID: in.ZoneID, CustomerName: in.CustomerName, Phone: in.Phone,
			Location: in.Location, Lat: in.Lat, Lng: in.Lng, PlanPackageID: in.PlanPackageID,
			Status: models.InstallRequested, Notes: in.Notes, RequestedAt: now, CreatedBy: a.UserID,
		}
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		return addJobEvent(tx, &a, job.ID, "", models.InstallRequested, "created", "Install requested")
	})
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// UpdateInstallJob edits a job's details (not its status) before it is installed.
func UpdateInstallJob(db *gorm.DB, a InstallActor, jobID uint, in InstallJobInput) (*models.InstallJob, error) {
	if !CanManageInstalls(a.Role) {
		return nil, installErr(403, "Only the owner or a zone manager can edit install requests.")
	}
	var job *models.InstallJob
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		if job, err = loadJobForUpdate(tx, a, jobID); err != nil {
			return err
		}
		switch job.Status {
		case models.InstallInstalled, models.InstallLive, models.InstallCancelled:
			return installErr(409, "This job is %s; its details can no longer change.", strings.ToLower(InstallStatusLabel(job.Status)))
		}
		if err := in.clean(tx, a); err != nil {
			return err
		}
		updates := map[string]interface{}{
			"customer_name": in.CustomerName, "phone": in.Phone, "location": in.Location,
			"lat": in.Lat, "lng": in.Lng, "zone_id": in.ZoneID, "plan_package_id": in.PlanPackageID, "notes": in.Notes,
		}
		if err := tx.Model(job).Updates(updates).Error; err != nil {
			return err
		}
		return addJobEvent(tx, &a, job.ID, job.Status, job.Status, "edited", "Details updated")
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ScheduleInstallJob sets (or moves) the visit date. A job that already has
// a crew stays "assigned"; otherwise it becomes "scheduled".
func ScheduleInstallJob(db *gorm.DB, a InstallActor, jobID uint, when time.Time) (*models.InstallJob, error) {
	if !CanManageInstalls(a.Role) {
		return nil, installErr(403, "Only the owner or a zone manager can schedule installs.")
	}
	var job *models.InstallJob
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		if job, err = loadJobForUpdate(tx, a, jobID); err != nil {
			return err
		}
		to := models.InstallScheduled
		if job.Status == models.InstallAssigned {
			to = models.InstallAssigned
		}
		if !CanInstallTransition(job.Status, to) {
			return installErr(409, "A job that is %s can't be scheduled.", strings.ToLower(InstallStatusLabel(job.Status)))
		}
		from := job.Status
		if err := tx.Model(job).Updates(map[string]interface{}{"status": to, "scheduled_for": when}).Error; err != nil {
			return err
		}
		job.Status, job.ScheduledFor = to, &when
		return addJobEvent(tx, &a, job.ID, from, to, "scheduled", "Visit set for "+utils.Kenya(when).Format("Mon 2 Jan 2006, 15:04"))
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// AssignInstallJob gives the job to a crew (optionally with a visit date).
func AssignInstallJob(db *gorm.DB, a InstallActor, jobID, crewID uint, when *time.Time) (*models.InstallJob, error) {
	if !CanManageInstalls(a.Role) {
		return nil, installErr(403, "Only the owner or a zone manager can assign crews.")
	}
	var job *models.InstallJob
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		if job, err = loadJobForUpdate(tx, a, jobID); err != nil {
			return err
		}
		if !CanInstallTransition(job.Status, models.InstallAssigned) {
			return installErr(409, "A job that is %s can't be given to a crew.", strings.ToLower(InstallStatusLabel(job.Status)))
		}
		if !crewOK(tx, a.OrgID, crewID) {
			return installErr(422, "Pick an active crew.")
		}
		var crew models.InstallCrew
		tx.First(&crew, crewID)
		from := job.Status
		upd := map[string]interface{}{"status": models.InstallAssigned, "crew_id": crewID}
		note := "Given to " + crew.Name
		if when != nil {
			upd["scheduled_for"] = *when
			note += ", visit " + utils.Kenya(*when).Format("Mon 2 Jan 2006, 15:04")
		}
		if err := tx.Model(job).Updates(upd).Error; err != nil {
			return err
		}
		job.Status, job.CrewID = models.InstallAssigned, &crewID
		if when != nil {
			job.ScheduledFor = when
		}
		return addJobEvent(tx, &a, job.ID, from, models.InstallAssigned, "assigned", note)
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// CancelInstallJob stops a job that has not been installed. Materials
// already booked to it (none, before install) are untouched.
func CancelInstallJob(db *gorm.DB, a InstallActor, jobID uint, reason string) (*models.InstallJob, error) {
	if !CanManageInstalls(a.Role) {
		return nil, installErr(403, "Only the owner or a zone manager can cancel installs.")
	}
	reason = clip(reason, 500)
	if reason == "" {
		return nil, installErr(422, "Say why the install is cancelled.")
	}
	var job *models.InstallJob
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		if job, err = loadJobForUpdate(tx, a, jobID); err != nil {
			return err
		}
		if !CanInstallTransition(job.Status, models.InstallCancelled) {
			return installErr(409, "A job that is %s can't be cancelled.", strings.ToLower(InstallStatusLabel(job.Status)))
		}
		from := job.Status
		if err := tx.Model(job).Updates(map[string]interface{}{"status": models.InstallCancelled, "cancelled_reason": reason}).Error; err != nil {
			return err
		}
		job.Status, job.CancelledReason = models.InstallCancelled, reason
		return addJobEvent(tx, &a, job.ID, from, models.InstallCancelled, "cancelled", reason)
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// MaterialLine is stock used on a job.
type MaterialLine struct {
	StockItemID uint    `json:"stock_item_id"`
	Quantity    float64 `json:"quantity"`
}

// MarkInstalledInput is what the crew enters at the house.
type MarkInstalledInput struct {
	OntSerial  string         `json:"ont_serial"`
	Materials  []MaterialLine `json:"materials"`
	LabourCost float64        `json:"labour_cost"`
}

// MarkInstalled books the ONT and materials out of stock, records the costs
// and creates the home's account (OFF until its first payment), all in one
// transaction. Only an "assigned" job can be installed, and the job row is
// locked, so a double tap can't install (or take stock) twice.
func MarkInstalled(db *gorm.DB, a InstallActor, jobID uint, in MarkInstalledInput, now time.Time) (*models.InstallJob, *models.Customer, error) {
	if !CanManageInstalls(a.Role) && a.Role != "installer" {
		return nil, nil, installErr(403, "You can't mark installs done.")
	}
	labour := toCents(in.LabourCost)
	if labour < 0 || labour > maxLabourCents {
		return nil, nil, installErr(422, "Labour cost must be between KES 0 and KES 100,000.")
	}
	serial := strings.ToUpper(strings.TrimSpace(in.OntSerial))
	if serial == "" {
		return nil, nil, installErr(422, "Pick the ONT serial fitted in the house.")
	}
	var job *models.InstallJob
	var cust *models.Customer
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		if job, err = loadJobForUpdate(tx, a, jobID); err != nil {
			return err
		}
		if !CanInstallTransition(job.Status, models.InstallInstalled) {
			if job.Status == models.InstallInstalled || job.Status == models.InstallLive {
				return installErr(409, "This job is already installed.")
			}
			return installErr(409, "Give the job to a crew before marking it installed.")
		}
		if job.ZoneID == nil || job.PlanPackageID == nil {
			return installErr(422, "The office must set the zone and the plan for this home first.")
		}
		if !installZoneOK(tx, a.OrgID, *job.ZoneID) || !installPackageOK(tx, *job.ZoneID, *job.PlanPackageID) {
			return installErr(422, "The zone or plan on this job is no longer valid. Ask the office to fix it.")
		}

		// The ONT.
		var ont models.OntUnit
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND serial = ?", a.OrgID, serial).First(&ont).Error; err != nil {
			return installErr(422, "ONT %s is not in your stock list.", serial)
		}
		if ont.Status != models.OntInStock {
			return installErr(409, "ONT %s is not in stock (it is %s).", serial, strings.ReplaceAll(ont.Status, "_", " "))
		}
		jid := job.ID
		ontCost, err := useStock(tx, a, ont.StockItemID, 100, &jid, &ont.ID, "ONT "+serial)
		if err != nil {
			return err
		}
		materials := ontCost

		// Other materials (one line per item).
		seen := map[uint]bool{}
		for _, m := range in.Materials {
			if m.StockItemID == 0 || m.Quantity == 0 {
				continue
			}
			if seen[m.StockItemID] {
				return installErr(422, "Each material can be listed once.")
			}
			seen[m.StockItemID] = true
			q := toHundredths(m.Quantity)
			if q <= 0 || q > maxQtyHundredths {
				return installErr(422, "Material quantities must be more than zero.")
			}
			var item models.StockItem
			if err := tx.Where("id = ? AND organization_id = ?", m.StockItemID, a.OrgID).First(&item).Error; err != nil {
				return installErr(422, "A material on the list is not in your stock.")
			}
			if item.TracksSerials {
				return installErr(422, "%s is tracked by serial; pick it as the ONT, not as a material.", item.Name)
			}
			c, err := useStock(tx, a, item.ID, q, &jid, nil, "")
			if err != nil {
				return err
			}
			materials += c
		}

		var settings models.InstallSetting
		tx.Where("organization_id = ?", a.OrgID).First(&settings)
		cust, err = CreateHomeFromInstall(tx, HomeFromInstall{
			OrgID: a.OrgID, ZoneID: *job.ZoneID, PackageID: *job.PlanPackageID,
			Name: job.CustomerName, Phone: job.Phone, FreeFirstDay: settings.FreeFirstDay, Now: now,
		})
		if err != nil {
			return err
		}

		if err := tx.Model(&models.OntUnit{}).Where("id = ?", ont.ID).
			Updates(map[string]interface{}{"status": models.OntInstalled, "job_id": job.ID, "customer_id": cust.ID}).Error; err != nil {
			return err
		}
		total := labour + materials
		from := job.Status
		if err := tx.Model(job).Updates(map[string]interface{}{
			"status": models.InstallInstalled, "installed_at": now, "ont_serial": serial,
			"labour_cost": centsToKES(labour), "materials_cost": centsToKES(materials), "total_cost": centsToKES(total),
			"customer_id": cust.ID,
		}).Error; err != nil {
			return err
		}
		job.Status, job.InstalledAt, job.OntSerial, job.CustomerID = models.InstallInstalled, &now, &serial, &cust.ID
		job.LabourCost, job.MaterialsCost, job.TotalCost = centsToKES(labour), centsToKES(materials), centsToKES(total)
		note := fmt.Sprintf("ONT %s fitted. Cost KES %s (labour %s + materials %s). Home account %s created, off until first payment.",
			serial, FormatKES(total), FormatKES(labour), FormatKES(materials), cust.AccountNumber)
		if settings.FreeFirstDay {
			note = strings.Replace(note, "off until first payment", "free for the first 24 hours", 1)
		}
		return addJobEvent(tx, &a, job.ID, from, models.InstallInstalled, "installed", note)
	})
	if err != nil {
		return nil, nil, err
	}
	return job, cust, nil
}

// FormatKES prints cents as "1,234" or "1,234.50".
func FormatKES(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	whole, frac := cents/100, cents%100
	s := fmt.Sprintf("%d", whole)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if frac != 0 {
		s += fmt.Sprintf(".%02d", frac)
	}
	if neg {
		s = "-" + s
	}
	return s
}

// ---- the home account ----

// HomeFromInstall is what an install knows about the new home.
type HomeFromInstall struct {
	OrgID        uint
	ZoneID       uint
	PackageID    uint
	Name         string
	Phone        string
	FreeFirstDay bool
	Now          time.Time
}

// GeneratePPPoEPassword is the same 12-hex-character random secret
// handlers.generatePPPoEPassword gives customers added by hand.
func GeneratePPPoEPassword() string {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

// newPPPoEUsername is a short, unique-per-ISP login like "h7k2m9q4".
func newPPPoEUsername(tx *gorm.DB, zoneID uint) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	for i := 0; i < 10; i++ {
		b := make([]byte, 7)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		out := []byte{'h'}
		for _, x := range b {
			out = append(out, alphabet[int(x)%len(alphabet)])
		}
		name := string(out)
		var n int64
		tx.Unscoped().Model(&models.Customer{}).Where("pppoe_username = ? AND zone_id IN (?)", name, sameISPZones(zoneID)).Count(&n)
		if n == 0 {
			return name, nil
		}
	}
	return "", errors.New("could not pick a free PPPoE username")
}

// CreateHomeFromInstall creates the paying-home account for a finished
// install: a PPPoE customer on the job's zone and plan, with a generated
// username and password, active but expired now (so the router keeps it off
// until its first payment), or with 24 free hours when the ISP turned on
// "free first day". It is the one place installs create customers, so the
// homes feature can swap in a richer version later.
func CreateHomeFromInstall(tx *gorm.DB, in HomeFromInstall) (*models.Customer, error) {
	user, err := newPPPoEUsername(tx, in.ZoneID)
	if err != nil {
		return nil, err
	}
	pw := GeneratePPPoEPassword()
	exp := in.Now
	if in.FreeFirstDay {
		exp = in.Now.Add(24 * time.Hour)
	}
	c := models.Customer{
		Name: in.Name, Phone: in.Phone, ZoneID: in.ZoneID, PackageID: in.PackageID, Type: "pppoe",
		PPPoEUsername: &user, PPPoEPassword: &pw, Status: "active", ExpiresAt: &exp,
	}
	if err := tx.Create(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// ---- going live ----

// MarkInstallsLive moves every installed job whose home has made its first
// real payment (M-Pesa, paybill or cash at the office — not free credit) to
// "live", dated at that payment. Safe to run any time and as often as you
// like: each job moves once (conditional update), so it is the hook the
// payment code or a timer can call.
func MarkInstallsLive(db *gorm.DB, customerID *uint) (int, error) {
	type cand struct {
		ID        uint
		FirstPaid time.Time
	}
	var rows []cand
	q := db.Table("install_jobs").
		Select("install_jobs.id AS id, MIN(payments.created_at) AS first_paid").
		Joins("JOIN payments ON payments.customer_id = install_jobs.customer_id").
		Where("install_jobs.status = ? AND install_jobs.customer_id IS NOT NULL", models.InstallInstalled).
		Where("payments.status = ? AND payments.amount > 0 AND payments.method <> ?", "completed", "credit").
		Group("install_jobs.id")
	if customerID != nil {
		q = q.Where("install_jobs.customer_id = ?", *customerID)
	}
	// MIN() over a datetime comes back as a string on some drivers; scan both.
	raw, err := q.Rows()
	if err != nil {
		return 0, err
	}
	for raw.Next() {
		var id uint
		var first interface{}
		if err := raw.Scan(&id, &first); err != nil {
			raw.Close()
			return 0, err
		}
		rows = append(rows, cand{ID: id, FirstPaid: parseDBTime(first)})
	}
	raw.Close()
	moved := 0
	for _, r := range rows {
		err := db.Transaction(func(tx *gorm.DB) error {
			at := r.FirstPaid
			if at.IsZero() {
				at = time.Now()
			}
			res := tx.Model(&models.InstallJob{}).Where("id = ? AND status = ?", r.ID, models.InstallInstalled).
				Updates(map[string]interface{}{"status": models.InstallLive, "live_at": at})
			if res.Error != nil || res.RowsAffected != 1 {
				return res.Error
			}
			moved++
			return addJobEvent(tx, nil, r.ID, models.InstallInstalled, models.InstallLive, "live", "First payment received; the home is paying")
		})
		if err != nil {
			return moved, err
		}
	}
	return moved, nil
}

func parseDBTime(v interface{}) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case []byte:
		return parseTimeString(string(t))
	case string:
		return parseTimeString(t)
	}
	return time.Time{}
}

func parseTimeString(s string) time.Time {
	for _, f := range []string{"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if t, err := time.ParseInLocation(f, s, time.UTC); err == nil {
			return t
		}
	}
	return time.Time{}
}

// InstallLiveWatcher runs MarkInstallsLive every couple of minutes.
type InstallLiveWatcher struct {
	db   *gorm.DB
	stop chan struct{}
}

// StartInstallLiveWatcher starts the periodic first-payment check.
func StartInstallLiveWatcher(db *gorm.DB) *InstallLiveWatcher {
	w := &InstallLiveWatcher{db: db, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-t.C:
				MarkInstallsLive(w.db, nil) //nolint:errcheck
			}
		}
	}()
	return w
}

// Stop ends the watcher.
func (w *InstallLiveWatcher) Stop() { close(w.stop) }

// MigrateInstalls creates the install/stock tables and users.crew_id
// (additive only).
func MigrateInstalls(db *gorm.DB) error {
	if err := db.AutoMigrate(models.InstallModels()...); err != nil {
		return err
	}
	m := db.Migrator()
	if !m.HasColumn(&models.InstallerUser{}, "CrewID") {
		if err := m.AddColumn(&models.InstallerUser{}, "CrewID"); err != nil {
			return err
		}
	}
	if !m.HasIndex(&models.InstallerUser{}, "idx_users_crew_id") {
		m.CreateIndex(&models.InstallerUser{}, "CrewID") //nolint:errcheck
	}
	return nil
}
