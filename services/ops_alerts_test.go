package services

import (
	"strings"
	"testing"
	"time"

	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// fakeAlertSender records owner SMS instead of sending them.
type fakeAlertSender struct{ sent []fakeSMS }

type fakeSMS struct {
	org        uint
	phone, msg string
}

func (f *fakeAlertSender) SendAlert(org uint, phone, msg string) error {
	f.sent = append(f.sent, fakeSMS{org, phone, msg})
	return nil
}

func (f *fakeAlertSender) take() []fakeSMS {
	s := f.sent
	f.sent = nil
	return s
}

type alertHarness struct {
	t   *testing.T
	db  *gorm.DB
	svc *OpsAlertService
	sms *fakeAlertSender
	now time.Time
}

func newAlertHarness(t *testing.T, start time.Time) *alertHarness {
	db := setupOpsDB(t)
	h := &alertHarness{t: t, db: db, sms: &fakeAlertSender{}, now: start}
	h.svc = NewOpsAlertService(db, h.sms)
	h.svc.Now = func() time.Time { return h.now }
	return h
}

func (h *alertHarness) seen(z *models.Zone, at time.Time) {
	l := at.In(time.Local)
	z.LastSeenAt = &l
	h.db.Model(&models.Zone{}).Where("id = ?", z.ID).Update("last_seen_at", l)
}

// heartbeat keeps the router "online" at the current time.
func (h *alertHarness) heartbeat(zs ...*models.Zone) {
	for _, z := range zs {
		h.seen(z, h.now)
	}
}

func (h *alertHarness) sweepAt(t time.Time) {
	h.now = t
	h.svc.Sweep()
}

func (h *alertHarness) alerts(zoneID uint) []models.OpsAlert {
	var out []models.OpsAlert
	h.db.Where("zone_id = ?", zoneID).Order("id ASC").Find(&out)
	return out
}

func (h *alertHarness) settings(org uint, enabled bool, phones string) {
	h.db.Where("organization_id = ?", org).Delete(&models.OpsAlertSetting{})
	h.db.Create(&models.OpsAlertSetting{OrganizationID: org, SmsEnabled: enabled, Phones: phones})
}

func TestOpsAlertsDownAndBack(t *testing.T) {
	h := newAlertHarness(t, eat(2026, 10, 22, 13, 0))
	h.settings(1, true, "254700000001,254700000002")
	z := mkZone(t, h.db, 1, "Oxygen", nil)
	h.heartbeat(&z)

	h.sweepAt(h.now) // first sight: remember, no alert
	if n := len(h.alerts(z.ID)); n != 0 || len(h.sms.take()) != 0 {
		t.Fatalf("first sight should not alert (%d alerts)", n)
	}

	// Last check-in 13:00; at 13:06 it's offline.
	h.sweepAt(eat(2026, 10, 22, 13, 6))
	sent := h.sms.take()
	if len(sent) != 2 || sent[0].phone != "254700000001" || sent[1].phone != "254700000002" {
		t.Fatalf("want one down SMS per phone, got %+v", sent)
	}
	if !strings.Contains(sent[0].msg, "Oxygen") || !strings.Contains(sent[0].msg, "OFFLINE since 13:00") {
		t.Errorf("down SMS text: %q", sent[0].msg)
	}
	al := h.alerts(z.ID)
	if len(al) != 1 || al[0].Kind != "offline" || al[0].Delivery != "sms_sent" {
		t.Fatalf("alerts = %+v", al)
	}

	// Still down: nothing more.
	for m := 7; m < 60; m += 5 {
		h.sweepAt(eat(2026, 10, 22, 13, m))
	}
	if len(h.sms.take()) != 0 || len(h.alerts(z.ID)) != 1 {
		t.Fatal("repeated SMS/alerts while still offline")
	}

	// Back at 14:00; the heartbeat recorded the outage and gave 4 customers time back.
	h.now = eat(2026, 10, 22, 14, 0)
	h.heartbeat(&z)
	h.db.Create(&models.ZoneOutage{OrganizationID: 1, ZoneID: z.ID, StartedAt: eat(2026, 10, 22, 13, 0).In(time.Local), EndedAt: h.now.In(time.Local), DurationSeconds: 3600, CustomersCompensated: 4})
	h.sweepAt(eat(2026, 10, 22, 14, 1))
	sent = h.sms.take()
	if len(sent) != 2 {
		t.Fatalf("want back-online SMS to both phones, got %d", len(sent))
	}
	if !strings.Contains(sent[0].msg, "back ONLINE (14:00)") || !strings.Contains(sent[0].msg, "Down 1h 00m") || !strings.Contains(sent[0].msg, "4 customers got the lost time back") {
		t.Errorf("back SMS text: %q", sent[0].msg)
	}
	al = h.alerts(z.ID)
	if len(al) != 2 || al[1].Kind != "recovered" || al[1].OutageSeconds != 3600 || al[1].CustomersCompensated == nil || *al[1].CustomersCompensated != 4 || al[1].Delivery != "sms_sent" {
		t.Fatalf("recovered alert = %+v", al[len(al)-1])
	}
	h.heartbeat(&z)
	h.sweepAt(eat(2026, 10, 22, 14, 2))
	if len(h.sms.take()) != 0 {
		t.Fatal("repeated back-online SMS")
	}
}

func TestOpsAlertsFlappingRouter(t *testing.T) {
	h := newAlertHarness(t, eat(2026, 10, 22, 9, 0))
	h.settings(1, true, "254700000001")
	z := mkZone(t, h.db, 1, "Flappy", nil)
	h.heartbeat(&z)
	h.sweepAt(h.now)

	// Down at 09:06 -> SMS.
	h.sweepAt(eat(2026, 10, 22, 9, 6))
	if n := len(h.sms.take()); n != 1 {
		t.Fatalf("first drop: %d SMS", n)
	}
	// Back 09:10 -> back SMS (pairs with the down SMS).
	h.now = eat(2026, 10, 22, 9, 10)
	h.heartbeat(&z)
	h.sweepAt(h.now)
	if n := len(h.sms.take()); n != 1 {
		t.Fatalf("first recovery: %d SMS", n)
	}
	// Drops again 09:20 (last seen 09:14), back 09:30, drops again 09:40: all
	// within 2 h of the first SMS -> no SMS, but every transition is recorded.
	h.now = eat(2026, 10, 22, 9, 14)
	h.heartbeat(&z)
	h.sweepAt(eat(2026, 10, 22, 9, 20))
	h.now = eat(2026, 10, 22, 9, 30)
	h.heartbeat(&z)
	h.sweepAt(h.now)
	h.sweepAt(eat(2026, 10, 22, 9, 40))
	if sent := h.sms.take(); len(sent) != 0 {
		t.Fatalf("flapping router texted again: %+v", sent)
	}
	al := h.alerts(z.ID)
	if len(al) != 5 {
		t.Fatalf("want 5 transitions recorded, got %d", len(al))
	}
	if al[2].Delivery != "flap_hold" || al[3].Delivery != "not_needed" || al[4].Delivery != "flap_hold" {
		t.Errorf("deliveries: %s %s %s", al[2].Delivery, al[3].Delivery, al[4].Delivery)
	}
	// Still down when 2 h since the first SMS are up (11:06) -> one SMS.
	h.sweepAt(eat(2026, 10, 22, 11, 5))
	if len(h.sms.take()) != 0 {
		t.Fatal("sent before the 2 h window ended")
	}
	h.sweepAt(eat(2026, 10, 22, 11, 6))
	sent := h.sms.take()
	if len(sent) != 1 || !strings.Contains(sent[0].msg, "OFFLINE since 09:30") {
		t.Fatalf("want one late down SMS, got %+v", sent)
	}
	if al := h.alerts(z.ID); al[4].Delivery != "sms_sent" {
		t.Errorf("held alert should now be sms_sent, got %s", al[4].Delivery)
	}
	h.sweepAt(eat(2026, 10, 22, 11, 30))
	if len(h.sms.take()) != 0 {
		t.Fatal("repeat after late SMS")
	}
}

func TestOpsAlertsQuietHoursSummary(t *testing.T) {
	h := newAlertHarness(t, eat(2026, 10, 22, 22, 0))
	h.settings(1, true, "254700000001")
	a := mkZone(t, h.db, 1, "Alpha", nil)
	b := mkZone(t, h.db, 1, "Bravo", nil)
	c := mkZone(t, h.db, 1, "Charlie", nil)
	h.heartbeat(&a, &b, &c)
	h.sweepAt(h.now)

	// Alpha: last seen 23:20, down 23:26, back 01:00.
	h.now = eat(2026, 10, 22, 23, 20)
	h.heartbeat(&a, &b, &c)
	h.now = eat(2026, 10, 22, 23, 26)
	h.heartbeat(&b, &c)
	h.sweepAt(h.now)
	h.now = eat(2026, 10, 23, 1, 0)
	h.heartbeat(&a, &b, &c)
	h.sweepAt(h.now)
	// Bravo: last seen 04:00, still down at 06:00.
	h.now = eat(2026, 10, 23, 4, 0)
	h.heartbeat(&a, &b, &c)
	h.now = eat(2026, 10, 23, 4, 6)
	h.heartbeat(&a, &c)
	h.sweepAt(h.now)
	// Charlie: down overnight, then deleted -> never mentioned.
	h.now = eat(2026, 10, 23, 4, 10)
	h.heartbeat(&a)
	h.sweepAt(h.now)
	h.now = eat(2026, 10, 23, 4, 30)
	h.heartbeat(&a)
	h.sweepAt(h.now)
	h.db.Delete(&c)

	h.now = eat(2026, 10, 23, 5, 59)
	h.heartbeat(&a)
	h.sweepAt(h.now)
	if sent := h.sms.take(); len(sent) != 0 {
		t.Fatalf("SMS during quiet hours: %+v", sent)
	}
	for _, al := range append(h.alerts(a.ID), h.alerts(b.ID)...) {
		if al.Delivery != "quiet_hours" {
			t.Errorf("alert %d delivery %s, want quiet_hours", al.ID, al.Delivery)
		}
	}

	h.now = eat(2026, 10, 23, 6, 0)
	h.heartbeat(&a)
	h.sweepAt(h.now)
	sent := h.sms.take()
	if len(sent) != 1 {
		t.Fatalf("want exactly one summary SMS, got %+v", sent)
	}
	msg := sent[0].msg
	if !strings.Contains(msg, "overnight") || !strings.Contains(msg, "Bravo STILL OFFLINE since 04:00") || !strings.Contains(msg, "Alpha went down 1x, back now") {
		t.Errorf("summary text: %q", msg)
	}
	if strings.Contains(msg, "Charlie") {
		t.Errorf("deleted zone in summary: %q", msg)
	}
	if strings.Index(msg, "Bravo") > strings.Index(msg, "Alpha") {
		t.Errorf("still-down routers should come first: %q", msg)
	}
	for _, al := range append(h.alerts(a.ID), h.alerts(b.ID)...) {
		if al.Delivery != "in_summary" {
			t.Errorf("alert %d delivery %s, want in_summary", al.ID, al.Delivery)
		}
	}
	var sums int64
	h.db.Model(&models.OpsAlert{}).Where("kind = ?", "summary").Count(&sums)
	if sums != 1 {
		t.Errorf("summary alerts = %d", sums)
	}

	// No repeats; Bravo's recovery later is texted normally.
	h.now = eat(2026, 10, 23, 6, 30)
	h.heartbeat(&a)
	h.sweepAt(h.now)
	if len(h.sms.take()) != 0 {
		t.Fatal("repeat after summary")
	}
	h.now = eat(2026, 10, 23, 8, 0)
	h.heartbeat(&a, &b)
	h.sweepAt(h.now)
	sent = h.sms.take()
	if len(sent) != 1 || !strings.Contains(sent[0].msg, "Bravo router is back ONLINE") {
		t.Fatalf("Bravo recovery SMS: %+v", sent)
	}
}

func TestOpsAlertsNoSMSUnlessConfigured(t *testing.T) {
	h := newAlertHarness(t, eat(2026, 10, 22, 10, 0))
	z1 := mkZone(t, h.db, 1, "NoSettings", nil)
	z2 := mkZone(t, h.db, 2, "SwitchedOff", nil)
	z3 := mkZone(t, h.db, 3, "NoPhones", nil)
	never := mkZone(t, h.db, 1, "NeverConnected", nil)
	h.settings(2, false, "254700000001")
	h.settings(3, true, "")
	h.heartbeat(&z1, &z2, &z3)
	h.sweepAt(h.now)
	h.sweepAt(eat(2026, 10, 22, 10, 10))
	if sent := h.sms.take(); len(sent) != 0 {
		t.Fatalf("SMS without settings: %+v", sent)
	}
	want := map[uint]string{z1.ID: "sms_off", z2.ID: "sms_off", z3.ID: "no_phones"}
	for id, d := range want {
		al := h.alerts(id)
		if len(al) != 1 || al[0].Delivery != d {
			t.Errorf("zone %d alerts %+v, want one %s", id, al, d)
		}
	}
	if len(h.alerts(never.ID)) != 0 {
		t.Error("never-connected router alerted")
	}

	// Switched on after a day of silence: an outage that old isn't texted.
	h.settings(1, true, "254700000001")
	h.sweepAt(eat(2026, 10, 23, 11, 0))
	if sent := h.sms.take(); len(sent) != 0 {
		t.Fatalf("stale outage texted: %+v", sent)
	}
}

func TestOpsAlertsDeletedOrDisabledZone(t *testing.T) {
	h := newAlertHarness(t, eat(2026, 10, 22, 10, 0))
	h.settings(1, true, "254700000001")
	del := mkZone(t, h.db, 1, "Deleted", nil)
	dis := mkZone(t, h.db, 1, "Disabled", nil)
	h.heartbeat(&del, &dis)
	h.sweepAt(h.now)
	h.db.Delete(&del)
	h.db.Model(&models.Zone{}).Where("id = ?", dis.ID).Update("status", "inactive")
	h.sweepAt(eat(2026, 10, 22, 10, 30))
	if sent := h.sms.take(); len(sent) != 0 {
		t.Fatalf("deleted/disabled zone texted: %+v", sent)
	}
	if len(h.alerts(del.ID))+len(h.alerts(dis.ID)) != 0 {
		t.Fatal("deleted/disabled zone alerted")
	}
}

func TestNormalizeAlertPhones(t *testing.T) {
	got, err := NormalizeAlertPhones([]string{"0712 345 678", "+254712345678", " 0110000000 ", ""})
	if err != nil || len(got) != 2 || got[0] != "254712345678" || got[1] != "254110000000" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := NormalizeAlertPhones([]string{"12345"}); err == nil {
		t.Error("bad number accepted")
	}
	if _, err := NormalizeAlertPhones([]string{"0711000001", "0711000002", "0711000003", "0711000004"}); err == nil {
		t.Error("more than 3 phones accepted")
	}
}

func TestInQuietHours(t *testing.T) {
	for h, want := range map[int]bool{22: false, 23: true, 0: true, 5: true, 6: false, 12: false} {
		if got := InQuietHours(eat(2026, 10, 22, h, 30)); got != want {
			t.Errorf("%02d:30 quiet=%v want %v", h, got, want)
		}
	}
}
