package models

import "time"

// Home installs: crews, install jobs (request → live), equipment stock and
// ONT serials. Money columns are KES decimals; the maths is done in cents in
// services/installs.go so totals never drift.

// InstallCrew is a team that installs fibre in homes.
type InstallCrew struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	OrganizationID uint      `gorm:"not null;index" json:"organization_id"`
	Name           string    `gorm:"size:120;not null" json:"name"`
	LeaderName     string    `gorm:"size:120" json:"leader_name"`
	Phone          string    `gorm:"size:20" json:"phone"`
	Active         bool      `gorm:"not null;default:true" json:"active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (InstallCrew) TableName() string { return "install_crews" }

// InstallerUser adds users.crew_id (nullable) without touching models.User:
// a staff user with role "installer" and a crew_id sees only that crew's
// jobs. Migrated as an extra column on the users table.
type InstallerUser struct {
	ID     uint  `gorm:"primaryKey"`
	CrewID *uint `gorm:"index"`
}

func (InstallerUser) TableName() string { return "users" }

// Install job statuses, in order.
const (
	InstallRequested = "requested"
	InstallScheduled = "scheduled"
	InstallAssigned  = "assigned"
	InstallInstalled = "installed"
	InstallLive      = "live"
	InstallCancelled = "cancelled"
)

// InstallJob is one home's fibre install, from request to paying customer.
// The Growth report reads status, installed_at and total_cost — keep them.
type InstallJob struct {
	ID              uint       `gorm:"primaryKey" json:"id"`
	OrganizationID  uint       `gorm:"not null;index" json:"organization_id"`
	ZoneID          *uint      `gorm:"index" json:"zone_id"`
	CustomerName    string     `gorm:"size:255;not null" json:"customer_name"`
	Phone           string     `gorm:"size:20;not null;index" json:"phone"`
	Location        string     `gorm:"size:255" json:"location"` // landmark / directions
	Lat             *float64   `gorm:"type:decimal(10,7)" json:"lat"`
	Lng             *float64   `gorm:"type:decimal(10,7)" json:"lng"`
	PlanPackageID   *uint      `json:"plan_package_id"`
	Status          string     `gorm:"size:20;not null;default:requested;index" json:"status"`
	ScheduledFor    *time.Time `json:"scheduled_for"`
	CrewID          *uint      `gorm:"index" json:"crew_id"`
	OntSerial       *string    `gorm:"size:64" json:"ont_serial"`
	LabourCost      float64    `gorm:"type:decimal(12,2);not null;default:0" json:"labour_cost"`
	MaterialsCost   float64    `gorm:"type:decimal(12,2);not null;default:0" json:"materials_cost"`
	TotalCost       float64    `gorm:"type:decimal(12,2);not null;default:0" json:"total_cost"`
	CustomerID      *uint      `gorm:"index" json:"customer_id"`
	Notes           string     `gorm:"type:text" json:"notes"`
	RequestedAt     time.Time  `gorm:"not null;index" json:"requested_at"`
	InstalledAt     *time.Time `gorm:"index" json:"installed_at"`
	LiveAt          *time.Time `json:"live_at"`
	CancelledReason string     `gorm:"size:500" json:"cancelled_reason"`
	CreatedBy       uint       `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`

	Crew    *InstallCrew `gorm:"foreignKey:CrewID" json:"crew,omitempty"`
	Zone    *Zone        `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
	Package *Package     `gorm:"foreignKey:PlanPackageID" json:"package,omitempty"`
}

func (InstallJob) TableName() string { return "install_jobs" }

// InstallJobEvent is the job's timeline / audit trail (append-only).
type InstallJobEvent struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	JobID      uint      `gorm:"not null;index" json:"job_id"`
	FromStatus string    `gorm:"size:20" json:"from_status"`
	ToStatus   string    `gorm:"size:20" json:"to_status"`
	Action     string    `gorm:"size:40;not null" json:"action"` // created|scheduled|assigned|installed|live|cancelled|edited
	Note       string    `gorm:"size:500" json:"note"`
	UserID     *uint     `json:"user_id"` // nil = the system (e.g. first payment seen)
	UserName   string    `gorm:"size:255" json:"user_name"`
	CreatedAt  time.Time `json:"created_at"`
}

func (InstallJobEvent) TableName() string { return "install_job_events" }

// StockItem is a kind of equipment or material the ISP keeps.
// OnHand is a running total kept in step with stock_moves inside the same
// transaction (the DB-checked guard that stops stock going negative);
// UnitCost is the weighted average purchase cost.
type StockItem struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	OrganizationID uint      `gorm:"not null;index" json:"organization_id"`
	Name           string    `gorm:"size:120;not null" json:"name"`
	Unit           string    `gorm:"size:20;not null;default:pcs" json:"unit"` // pcs | metres
	UnitCost       float64   `gorm:"type:decimal(12,2);not null;default:0" json:"unit_cost"`
	ReorderLevel   float64   `gorm:"type:decimal(12,2);not null;default:0" json:"reorder_level"`
	TracksSerials  bool      `gorm:"not null;default:false" json:"tracks_serials"`
	OnHand         float64   `gorm:"type:decimal(12,2);not null;default:0" json:"on_hand"`
	Active         bool      `gorm:"not null;default:true" json:"active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (StockItem) TableName() string { return "stock_items" }

// Stock move types. Quantity is always positive; the type gives the sign.
const (
	MovePurchase   = "purchase"   // in, with the price paid
	MoveUsed       = "used"       // out, onto an install job
	MoveReturned   = "returned"   // in, back from a job / a home
	MoveWriteOff   = "write_off"  // out, faulty or lost
	MoveAdjustment = "adjustment" // in or out (stock count correction); Direction says which
)

// StockMove is the append-only stock ledger. On hand = sum of signed moves.
type StockMove struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	OrganizationID uint      `gorm:"not null;index" json:"organization_id"`
	StockItemID    uint      `gorm:"not null;index" json:"stock_item_id"`
	Type           string    `gorm:"size:20;not null" json:"type"`
	Direction      int       `gorm:"not null" json:"direction"` // +1 in, -1 out
	Quantity       float64   `gorm:"type:decimal(12,2);not null" json:"quantity"`
	UnitCost       float64   `gorm:"type:decimal(12,2);not null;default:0" json:"unit_cost"`
	JobID          *uint     `gorm:"index" json:"job_id"`
	OntUnitID      *uint     `json:"ont_unit_id"`
	UserID         *uint     `json:"user_id"`
	Note           string    `gorm:"size:500" json:"note"`
	CreatedAt      time.Time `gorm:"index" json:"created_at"`

	StockItem *StockItem `gorm:"foreignKey:StockItemID" json:"stock_item,omitempty"`
}

func (StockMove) TableName() string { return "stock_moves" }

// ONT unit statuses.
const (
	OntInStock   = "in_stock"
	OntInstalled = "installed"
	OntFaulty    = "faulty"
	OntReturned  = "returned" // taken back from a home, waiting to be checked
)

// OntUnit is one serial-numbered ONT (or other serial-tracked unit).
type OntUnit struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	OrganizationID uint      `gorm:"not null;uniqueIndex:uniq_ont_org_serial,priority:1" json:"organization_id"`
	Serial         string    `gorm:"size:64;not null;uniqueIndex:uniq_ont_org_serial,priority:2" json:"serial"`
	StockItemID    uint      `gorm:"not null;index" json:"stock_item_id"`
	Status         string    `gorm:"size:20;not null;default:in_stock;index" json:"status"`
	JobID          *uint     `gorm:"index" json:"job_id"`
	CustomerID     *uint     `gorm:"index" json:"customer_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	StockItem *StockItem `gorm:"foreignKey:StockItemID" json:"stock_item,omitempty"`
}

func (OntUnit) TableName() string { return "ont_units" }

// InstallSetting holds per-ISP install options.
type InstallSetting struct {
	ID             uint `gorm:"primaryKey" json:"id"`
	OrganizationID uint `gorm:"not null;uniqueIndex" json:"organization_id"`
	// FreeFirstDay: a newly installed home gets 24 hours of internet before
	// its first payment. Off by default (the home starts OFF until it pays).
	FreeFirstDay bool      `gorm:"not null;default:false" json:"free_first_day"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (InstallSetting) TableName() string { return "install_settings" }

// InstallModels is the AutoMigrate list for this feature's own tables
// (users.crew_id is added separately, see services.MigrateInstalls).
func InstallModels() []interface{} {
	return []interface{}{&InstallCrew{}, &InstallJob{}, &InstallJobEvent{},
		&StockItem{}, &StockMove{}, &OntUnit{}, &InstallSetting{}}
}
