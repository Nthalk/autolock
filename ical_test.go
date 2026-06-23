package main

import (
	"strings"
	"testing"
	"time"
)

const sample = `BEGIN:VCALENDAR
PRODID:-//Airbnb Inc//Hosting Calendar 1.0//EN
VERSION:2.0
BEGIN:VEVENT
DTSTART;VALUE=DATE:20260620
DTEND;VALUE=DATE:20260701
SUMMARY:Reserved
UID:a@airbnb.com
DESCRIPTION:Reservation URL: https://www.airbnb.com/hosting/reservations/de
 tails/HMWCKBJHSH\nPhone Number (Last 4 Digits): 3341
END:VEVENT
BEGIN:VEVENT
DTSTART;VALUE=DATE:20260809
DTEND;VALUE=DATE:20260906
SUMMARY:Airbnb (Not available)
UID:b@airbnb.com
END:VEVENT
END:VCALENDAR
`

func TestParseICS(t *testing.T) {
	res, err := ParseICS(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 reservation (host block skipped), got %d", len(res))
	}
	r := res[0]
	if r.Code != "HMWCKBJHSH" {
		t.Errorf("code = %q", r.Code)
	}
	if r.Last4 != "3341" {
		t.Errorf("last4 = %q", r.Last4)
	}
	if got := r.CheckIn.Format("2006-01-02"); got != "2026-06-20" {
		t.Errorf("checkin = %q", got)
	}
	if got := r.CheckOut.Format("2006-01-02"); got != "2026-07-01" {
		t.Errorf("checkout = %q", got)
	}
}

func TestPlan(t *testing.T) {
	r := Reservation{Code: "X", Last4: "3341", Slot: 2}
	r.CheckIn, _ = parseDate("20260620")
	r.CheckOut, _ = parseDate("20260701")
	cfg := &Config{
		SetCodeBeforeCheckin:   Duration(24 * 3600e9),
		ClearCodeAfterCheckout: Duration(4 * 3600e9),
	}
	reg := BuildRegistry(nil)
	cal := Calendar{Slot: 2, Lock: LockSpec{Driver: "zwave_js", Entity: "lock.front_door"}}
	actions := plan(cfg, r, cal, reg)
	if len(actions) != 2 {
		t.Fatalf("want 2 actions, got %d", len(actions))
	}

	set := actions[0]
	if got := set.Target.Format("2006-01-02 15:04"); got != "2026-06-19 00:00" {
		t.Errorf("set target = %q", got)
	}
	if set.Call.Service != "set_lock_usercode" || set.Call.Data["usercode"] != "3341" || set.Call.Data["code_slot"] != 2 {
		t.Errorf("set call = %v", set.Call)
	}

	clr := actions[1]
	if got := clr.Target.Format("2006-01-02 15:04"); got != "2026-07-01 04:00" {
		t.Errorf("clear target = %q", got)
	}
	if clr.Call.Service != "clear_lock_usercode" || clr.Call.Data["entity_id"] != "lock.front_door" {
		t.Errorf("clear call = %v", clr.Call)
	}
}

func TestDriverCalls(t *testing.T) {
	reg := BuildRegistry(nil)
	set := reg["zha"].SetCode("lock.zb", 3, "9999")
	if set.Domain != "zha" || set.Service != "set_lock_user_code" || set.Data["user_code"] != "9999" {
		t.Errorf("zha set = %v", set)
	}
	if set.Data["code_slot"] != 3 {
		t.Errorf("code_slot should be int 3, got %v (%T)", set.Data["code_slot"], set.Data["code_slot"])
	}
}

func TestCustomDriver(t *testing.T) {
	// A lock model with a different command structure, defined as data.
	reg := BuildRegistry(map[string]DriverSpec{
		"acme": {
			Set:   CommandSpec{"acme.program", map[string]any{"lock": "__ENTITY__", "pin": "__CODE__", "index": "__SLOT__"}},
			Clear: CommandSpec{"acme.erase", map[string]any{"lock": "__ENTITY__", "index": "__SLOT__"}},
		},
	})
	c := reg["acme"].SetCode("lock.acme", 5, "1234")
	if c.Domain != "acme" || c.Service != "program" || c.Data["pin"] != "1234" || c.Data["index"] != 5 {
		t.Errorf("acme set = %v", c)
	}
}

func TestState(t *testing.T) {
	path := t.TempDir() + "/airlock.state"
	now := parseDateMust("20260620")

	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Has("k1") {
		t.Fatal("fresh state should be empty")
	}
	if err := s.Mark("k1", now); err != nil {
		t.Fatal(err)
	}

	// reload from disk: k1 persisted
	s2, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has("k1") {
		t.Error("k1 not persisted")
	}
	if s2.Has("k2") {
		t.Error("unexpected k2")
	}
}

func parseDateMust(v string) (t time.Time) {
	t, _ = parseDate(v)
	return t
}
