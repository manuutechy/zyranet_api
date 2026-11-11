package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/zyranet/zyranet-api/services"
)

func TestParseRouterMs(t *testing.T) {
	for in, want := range map[string]float64{"3": 3, "12ms": 12, "0.5": 0.5, "00:00:00.004": 4} {
		if got, ok := parseRouterMs(in); !ok || got != want {
			t.Errorf("%q → %v %v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1"} {
		if _, ok := parseRouterMs(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestParseLineReport(t *testing.T) {
	var got services.LineReport
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error { got = parseLineReport(c); return nil })
	call := func(q string) services.LineReport {
		got = services.LineReport{}
		if _, err := app.Test(httptest.NewRequest("GET", "/?"+q, nil)); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Old script: nothing for the watch.
	if r := call("cpu=12&totalmem=100&freemem=50&clients=4&board=RB"); r.HasWatchData() {
		t.Errorf("old heartbeat produced watch data: %+v", r)
	}
	r := call("cpu=12&totalmem=1000&freemem=250&clients=4&wanrx=18446744073709551000&wantx=55&pppoe=31&olt=3&oltms=2&board=RB")
	if !r.HasWatchData() || *r.RxBytes != 18446744073709551000 || *r.TxBytes != 55 || *r.PPPoE != 31 || *r.Hotspot != 4 || *r.CPU != 12 || *r.MemPct != 75 || !*r.OltOK || *r.OltMs != 2 {
		t.Errorf("full heartbeat: %+v", r)
	}
	// WAN counters failed on the router (empty), OLT unreachable.
	r = call("wanrx=&wantx=&pppoe=0&olt=0&oltms=0")
	if r.RxBytes != nil || *r.OltOK || r.OltMs != nil || *r.PPPoE != 0 {
		t.Errorf("partial heartbeat: %+v", r)
	}
}
