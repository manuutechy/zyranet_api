package services

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

func TestLineRate(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, EAT)
	// 75 MB in, 15 MB out over 60 s = 10 Mbps down, 2 Mbps up.
	down, up, ok := LineRate(LineCounters{t0, 1_000_000_000, 500_000_000}, LineCounters{t0.Add(time.Minute), 1_075_000_000, 515_000_000})
	if !ok || down != 10 || up != 2 {
		t.Fatalf("got %v %v %v, want 10 2 true", down, up, ok)
	}
	// Big 64-bit counters keep full precision.
	big := uint64(1) << 62
	down, _, ok = LineRate(LineCounters{t0, big, big}, LineCounters{t0.Add(90 * time.Second), big + 1_125_000_000, big})
	if !ok || down != 100 {
		t.Errorf("big counters: %v %v", down, ok)
	}
	cases := []struct {
		name string
		prev LineCounters
		cur  LineCounters
	}{
		{"counter reset (reboot)", LineCounters{t0, 5_000_000, 5_000_000}, LineCounters{t0.Add(time.Minute), 1_000, 9_000_000}},
		{"32-bit wrap", LineCounters{t0, math.MaxUint32 - 10, 0}, LineCounters{t0.Add(time.Minute), 50, 10}},
		{"too close", LineCounters{t0, 0, 0}, LineCounters{t0.Add(5 * time.Second), 100, 100}},
		{"long gap", LineCounters{t0, 0, 0}, LineCounters{t0.Add(30 * time.Minute), 100, 100}},
		{"impossible speed", LineCounters{t0, 0, 0}, LineCounters{t0.Add(time.Minute), 1 << 50, 0}},
		{"out of order", LineCounters{t0.Add(time.Minute), 0, 0}, LineCounters{t0, 100, 100}},
	}
	for _, c := range cases {
		if _, _, ok := LineRate(c.prev, c.cur); ok {
			t.Errorf("%s: expected no figure", c.name)
		}
	}
	if p := LinePct(45, 50); p == nil || *p != 90 {
		t.Errorf("LinePct(45,50) = %v", p)
	}
	if LinePct(45, 0) != nil {
		t.Error("no uplink set → no %")
	}
}

func pctp(v float64) *float64 { return &v }
func boolp(v bool) *bool      { return &v }

func TestEvaluateWatchLineHysteresis(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 18, 0, 0, 0, EAT)
	st := models.ZoneWatchState{}
	run := func(min int, pct float64) []string {
		var ev []string
		st, ev = EvaluateWatch(st, WatchInput{At: t0.Add(time.Duration(min) * time.Minute), LinePct: pctp(pct)})
		return ev
	}
	// 14 minutes at 85%: not yet.
	for m := 0; m <= 14; m++ {
		if ev := run(m, 85); len(ev) != 0 {
			t.Fatalf("minute %d: alerted too early %v", m, ev)
		}
	}
	// 15th minute: full.
	if ev := run(15, 90); len(ev) != 1 || ev[0] != WatchLineFull {
		t.Fatalf("minute 15: %v", ev)
	}
	if st.FullSince == nil || !st.FullSince.Equal(t0) {
		t.Errorf("full since %v, want run start", st.FullSince)
	}
	// Still full: no repeat.
	if ev := run(16, 95); len(ev) != 0 {
		t.Errorf("repeat alert %v", ev)
	}
	// 75% is under 80 but over 70: stays full, no event.
	if ev := run(17, 75); len(ev) != 0 || st.LineStatus != "full" {
		t.Errorf("75%% should hold the full state: %v %s", ev, st.LineStatus)
	}
	// 65%: clears.
	if ev := run(18, 65); len(ev) != 1 || ev[0] != WatchLineOK || st.LineStatus != "ok" {
		t.Errorf("clear: %v %s", ev, st.LineStatus)
	}

	// A dip under 80 restarts the 15 minutes.
	st = models.ZoneWatchState{}
	for m := 0; m < 10; m++ {
		run(100+m, 85)
	}
	run(110, 79)
	for m := 111; m < 125; m++ {
		if ev := run(m, 85); len(ev) != 0 {
			t.Fatalf("minute %d: dip should have restarted the run", m)
		}
	}
	if ev := run(126, 85); len(ev) != 1 {
		t.Errorf("15 min after the dip it should alert: %v", ev)
	}

	// A 10-minute silence breaks a run too.
	st = models.ZoneWatchState{}
	run(200, 90)
	run(205, 90)
	run(216, 90) // gap > 5 min: run restarts here
	for m := 217; m <= 230; m++ {
		if ev := run(m, 90); len(ev) != 0 {
			t.Errorf("minute %d: gap should have restarted the run: %v", m, ev)
		}
	}
	if ev := run(231, 90); len(ev) != 1 {
		t.Errorf("15 min after the restart: %v", ev)
	}

	// No figure (nil) changes nothing.
	before := st
	st, ev := EvaluateWatch(st, WatchInput{At: t0.Add(300 * time.Minute)})
	if len(ev) != 0 || st.LineStatus != before.LineStatus {
		t.Errorf("nil pct changed state")
	}
}

func TestEvaluateWatchOlt(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 18, 0, 0, 0, EAT)
	st := models.ZoneWatchState{}
	step := func(i int, ok bool) []string {
		var ev []string
		st, ev = EvaluateWatch(st, WatchInput{At: t0.Add(time.Duration(i) * time.Minute), OltOK: boolp(ok)})
		return ev
	}
	step(0, true)
	if st.OltStatus != "up" {
		t.Fatalf("status %s", st.OltStatus)
	}
	step(1, false)
	step(2, false)
	step(3, true) // one answer resets the count
	step(4, false)
	if ev := step(5, false); len(ev) != 0 {
		t.Fatalf("2 fails alerted: %v", ev)
	}
	if ev := step(6, false); len(ev) != 1 || ev[0] != WatchOltDown {
		t.Fatalf("3rd fail: %v", ev)
	}
	if !st.OltDownSince.Equal(t0.Add(4 * time.Minute)) {
		t.Errorf("down since %v", st.OltDownSince)
	}
	if ev := step(7, false); len(ev) != 0 {
		t.Errorf("repeat: %v", ev)
	}
	if ev := step(8, true); len(ev) != 1 || ev[0] != WatchOltUp {
		t.Errorf("back: %v", ev)
	}
}

func setupWatchDB(t *testing.T) *gorm.DB {
	db := setupOpsDB(t)
	if err := MigrateNetWatch(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func u64p(v uint64) *uint64 { return &v }

func TestIngestLineReportAlertsAndSMS(t *testing.T) {
	db := setupWatchDB(t)
	sms := &fakeAlertSender{}
	NetWatchSender = sms
	netWatchGo = func(fn func()) { fn() }
	defer func() { NetWatchSender = nil; netWatchGo = func(fn func()) { go fn() } }()

	db.Create(&models.OpsAlertSetting{OrganizationID: 1, SmsEnabled: true, Phones: "254700000001"})
	z := mkZone(t, db, 1, "Maseno", nil)
	z.UplinkDownMbps, z.UplinkUpMbps = 100, 20
	olt := "192.168.100.1"
	z.OltIP = &olt
	db.Model(&models.Zone{}).Where("id = ?", z.ID).Updates(map[string]interface{}{"uplink_down_mbps": 100, "uplink_up_mbps": 20, "olt_ip": olt})

	t0 := eat(2026, 11, 11, 19, 0) // evening, not quiet hours
	var rx, tx uint64 = 1e9, 1e8
	feed := func(min int, downMbps float64, oltOK bool) *models.ZoneLineStat {
		rx += uint64(downMbps * 1e6 / 8 * 60)
		tx += uint64(1e6 / 8 * 60)
		return IngestLineReport(db, &z, LineReport{RxBytes: u64p(rx), TxBytes: u64p(tx), PPPoE: intp(40), Hotspot: intp(3), OltOK: boolp(oltOK), OltMs: pctp(2)}, t0.Add(time.Duration(min)*time.Minute))
	}
	first := feed(0, 0, true)
	if first == nil || first.DownMbps != nil {
		t.Fatalf("first reading has no previous one: %+v", first)
	}
	r := feed(1, 50, true)
	if r.DownMbps == nil || math.Abs(*r.DownMbps-50) > 0.01 || r.DownPct == nil || math.Abs(*r.DownPct-50) > 0.01 {
		t.Fatalf("rate: %+v", r)
	}
	for m := 2; m <= 17; m++ {
		feed(m, 90, true)
	}
	var alerts []models.OpsAlert
	db.Where("zone_id = ?", z.ID).Order("id").Find(&alerts)
	if len(alerts) != 1 || alerts[0].Kind != WatchLineFull || alerts[0].Delivery != "sms_sent" {
		t.Fatalf("want one line_full sms_sent alert, got %+v", alerts)
	}
	if got := sms.take(); len(got) != 1 || !strings.Contains(got[0].msg, "Maseno internet line is FULL") {
		t.Fatalf("sms: %+v", got)
	}
	// Back under 70%.
	feed(18, 30, true)
	db.Where("zone_id = ?", z.ID).Order("id").Find(&alerts)
	if len(alerts) != 2 || alerts[1].Kind != WatchLineOK || alerts[1].Delivery != "sms_sent" {
		t.Fatalf("clear alert: %+v", alerts)
	}
	sms.take()
	// Full again within 2 h: recorded, SMS held.
	for m := 19; m <= 35; m++ {
		feed(m, 95, true)
	}
	db.Where("zone_id = ?", z.ID).Order("id").Find(&alerts)
	if alerts[len(alerts)-1].Kind != WatchLineFull || alerts[len(alerts)-1].Delivery != "flap_hold" || len(sms.take()) != 0 {
		t.Fatalf("flap: %+v", alerts[len(alerts)-1])
	}
	// OLT stops answering for 3 checks.
	feed(36, 20, false)
	feed(37, 20, false)
	feed(38, 20, false)
	db.Where("zone_id = ? AND kind = ?", z.ID, WatchOltDown).Find(&alerts)
	if len(alerts) != 1 || alerts[0].Delivery != "sms_sent" || !strings.Contains(alerts[0].Message, "192.168.100.1") {
		t.Fatalf("olt down: %+v", alerts)
	}
	// The line cleared at 20 Mbps (20%) but its "full" was never texted → not_needed.
	var ok models.OpsAlert
	db.Where("zone_id = ? AND kind = ?", z.ID, WatchLineOK).Order("id DESC").First(&ok)
	if ok.Delivery != "not_needed" {
		t.Errorf("clear after a held full: %s", ok.Delivery)
	}

	// Counter reset (router reboot): stored, no speed, no crash.
	rx, tx = 10, 10
	rr := IngestLineReport(db, &z, LineReport{RxBytes: u64p(rx), TxBytes: u64p(tx), PPPoE: intp(0)}, t0.Add(39*time.Minute))
	if rr == nil || rr.DownMbps != nil {
		t.Errorf("reset reading: %+v", rr)
	}

	// The watch list.
	rep, err := BuildZonesWatch(db, 1, nil, t0.Add(39*time.Minute+10*time.Second))
	if err != nil || len(rep.Zones) != 1 {
		t.Fatalf("watch: %v %+v", err, rep)
	}
	w := rep.Zones[0]
	if !w.HasLineData || w.OltStatus != "down" || w.PeakTodayPct == nil || *w.PeakTodayPct < 94 || w.PPPoE == nil || *w.PPPoE != 0 {
		t.Errorf("watch row: %+v", w)
	}
	// Other ISP sees nothing.
	if rep2, _ := BuildZonesWatch(db, 2, nil, t0); len(rep2.Zones) != 0 {
		t.Error("cross-tenant leak")
	}
	series, err := BuildZoneWatchSeries(db, &z, 24, t0.Add(40*time.Minute))
	if err != nil || len(series.Points) != 288 || series.BucketMinutes != 5 || series.Readings != 40 {
		t.Fatalf("series: %v points=%d bucket=%d readings=%d", err, len(series.Points), series.BucketMinutes, series.Readings)
	}
}

func TestIngestQuietHoursAndSmsOff(t *testing.T) {
	db := setupWatchDB(t)
	sms := &fakeAlertSender{}
	NetWatchSender = sms
	netWatchGo = func(fn func()) { fn() }
	defer func() { NetWatchSender = nil; netWatchGo = func(fn func()) { go fn() } }()

	z := mkZone(t, db, 1, "Night", nil)
	z.UplinkDownMbps, z.UplinkUpMbps = 10, 10
	var rx uint64
	feed := func(at time.Time) {
		rx += uint64(9.5 * 1e6 / 8 * 60)
		IngestLineReport(db, &z, LineReport{RxBytes: u64p(rx), TxBytes: u64p(0)}, at)
	}
	// No alert settings row: SMS off by default.
	t0 := eat(2026, 11, 11, 15, 0)
	for m := 0; m <= 17; m++ {
		feed(t0.Add(time.Duration(m) * time.Minute))
	}
	var a models.OpsAlert
	db.Where("zone_id = ?", z.ID).First(&a)
	if a.Kind != WatchLineFull || a.Delivery != "sms_off" {
		t.Fatalf("sms off by default: %+v", a)
	}

	// Night: switched on, but quiet hours → no SMS.
	z2 := mkZone(t, db, 1, "Night2", nil)
	z2.UplinkDownMbps = 10
	db.Create(&models.OpsAlertSetting{OrganizationID: 1, SmsEnabled: true, Phones: "254700000001"})
	n0 := eat(2026, 11, 11, 23, 30)
	var rx2 uint64
	for m := 0; m <= 17; m++ {
		rx2 += uint64(9.5 * 1e6 / 8 * 60)
		IngestLineReport(db, &z2, LineReport{RxBytes: u64p(rx2), TxBytes: u64p(0)}, n0.Add(time.Duration(m)*time.Minute))
	}
	var b models.OpsAlert
	db.Where("zone_id = ?", z2.ID).First(&b)
	if b.Delivery != "night_no_sms" || len(sms.take()) != 0 {
		t.Fatalf("quiet hours: %+v", b)
	}
}

// The heartbeat keeps its old part byte-for-byte, adds the watch fields,
// escapes everything, and only pings an OLT that is a real IP.
func TestHeartbeatSourceWatchFields(t *testing.T) {
	z := models.Zone{ID: 7, ProvisionToken: "tok", WanPort: "ether1"}
	got := heartbeatSource(&z)
	oldPrefix := fmt.Sprintf("/tool fetch url=\\\"https://api.zyranet.co.ke/api/v1/public/zones/sync/%d?token=%s\\\" dst-path=\\\"zyra-sync.rsc\\\" %s; :if ([:len [/file find name=\\\"zyra-sync.rsc\\\"]] > 0) do={ /import file-name=zyra-sync.rsc; /file remove [find name=\\\"zyra-sync.rsc\\\"] }; :local c [/system resource get cpu-load]; :local tm [/system resource get total-memory]; :local fm [/system resource get free-memory]; :local cl [:len [/ip hotspot active find]]; :local b [/system resource get board-name]; ", 7, "tok", "mode=https")
	if !strings.HasPrefix(got, oldPrefix) {
		t.Fatalf("old heartbeat part changed:\n%s", got)
	}
	for _, must := range []string{
		`[/interface get [find name=\"ether1\"] rx-byte]`,
		`[/interface get [find name=\"ether1\"] tx-byte]`,
		`:local pp [:len [/ppp active find]]`,
		`\"&wanrx=\" . \$wr . \"&wantx=\" . \$wt`,
		`\"&pppoe=\" . \$pp . \"&board=\" . \$b) mode=https keep-result=no`,
	} {
		if !strings.Contains(got, must) {
			t.Errorf("missing %s in\n%s", must, got)
		}
	}
	if strings.Contains(got, "flood-ping") {
		t.Error("no OLT set: must not ping")
	}
	// No unescaped quote or $ anywhere (it sits inside source="...").
	for i := 0; i < len(got); i++ {
		if (got[i] == '"' || got[i] == '$') && (i == 0 || got[i-1] != '\\') {
			t.Fatalf("unescaped %q at %d: %s", got[i], i, got[max(0, i-30):min(len(got), i+30)])
		}
	}

	olt := "192.168.100.1"
	z.OltIP = &olt
	got = heartbeatSource(&z)
	if !strings.Contains(got, `/tool flood-ping 192.168.100.1 count=3`) || !strings.Contains(got, `\"&olt=\" . \$olr . \"&oltms=\" . \$olm`) || !strings.Contains(got, `\$\"avg-rtt\"`) {
		t.Errorf("OLT ping missing:\n%s", got)
	}

	bad := `1.2.3.4"; /system reset-configuration; "`
	z.OltIP = &bad
	z.WanPort = `ether1"] ; /user add name=x`
	got = heartbeatSource(&z)
	if strings.Contains(got, "flood-ping") || strings.Contains(got, "reset-configuration") || strings.Contains(got, "wanrx") || strings.Contains(got, "user add") {
		t.Errorf("unsafe values reached the script:\n%s", got)
	}
}
