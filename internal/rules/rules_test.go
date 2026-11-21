package rules

import (
	"strings"
	"testing"
	"time"
)

var berlin = mustLoc("Europe/Berlin")

func mustLoc(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// at returns a time on Wednesday 2026-11-18 in Berlin.
func at(hh, mm int) time.Time {
	return time.Date(2026, 11, 18, hh, mm, 0, 0, berlin)
}

func TestParseWindows(t *testing.T) {
	ws, err := ParseWindows(" 19:00-20:00, 15:00-18:30 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Start != 15*60 || ws[0].End != 18*60+30 || ws[1].Start != 19*60 {
		t.Fatalf("unexpected windows: %+v", ws)
	}
	if got := FormatWindows(ws); got != "15:00-18:30, 19:00-20:00" {
		t.Fatalf("FormatWindows = %q", got)
	}

	ws, err = ParseWindows("20:00-24:00")
	if err != nil || ws[0].End != 1440 {
		t.Fatalf("24:00 not accepted: %+v %v", ws, err)
	}

	if ws, err := ParseWindows(""); err != nil || len(ws) != 0 {
		t.Fatalf("empty string: %+v %v", ws, err)
	}

	for _, bad := range []string{"15:00", "18:00-15:00", "25:00-26:00", "aa:00-10:00", "10:xx-11:00", "1000-1100", "10:00-11:61"} {
		if _, err := ParseWindows(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func testSchedule() Schedule {
	weekday := []Window{{Start: 15 * 60, End: 18 * 60}}
	return Schedule{
		time.Monday:    weekday,
		time.Tuesday:   weekday,
		time.Wednesday: weekday,
		time.Thursday:  weekday,
		time.Friday:    weekday,
		time.Saturday:  {{Start: 10 * 60, End: 19 * 60}},
	}
}

func TestScheduleCurrentAndNext(t *testing.T) {
	s := testSchedule()
	if _, ok := s.Current(at(14, 59)); ok {
		t.Error("14:59 should be outside")
	}
	if _, ok := s.Current(at(15, 0)); !ok {
		t.Error("15:00 should be inside")
	}
	if _, ok := s.Current(at(18, 0)); ok {
		t.Error("18:00 should be outside (end exclusive)")
	}
	if got := s.MinutesLeftInWindow(at(17, 15)); got != 45 {
		t.Errorf("MinutesLeftInWindow = %d", got)
	}
	if got := s.MinutesLeftInWindow(at(12, 0)); got != 0 {
		t.Errorf("MinutesLeftInWindow outside = %d", got)
	}

	next, ok := s.Next(at(12, 0))
	if !ok || !next.Equal(at(15, 0)) {
		t.Errorf("Next from noon = %v", next)
	}
	next, _ = s.Next(at(16, 0))
	if want := time.Date(2026, 11, 19, 15, 0, 0, 0, berlin); !next.Equal(want) {
		t.Errorf("Next from inside window = %v, want %v", next, want)
	}
	// Saturday evening -> Sunday has nothing -> Monday 15:00
	next, _ = s.Next(time.Date(2026, 11, 21, 20, 0, 0, 0, berlin))
	if want := time.Date(2026, 11, 23, 15, 0, 0, 0, berlin); !next.Equal(want) {
		t.Errorf("Next over weekend = %v, want %v", next, want)
	}

	if _, ok := (Schedule{}).Next(at(12, 0)); ok {
		t.Error("empty schedule should have no next window")
	}
}

func TestEvaluate(t *testing.T) {
	limits := Limits{DailyMinutes: 90, WeeklyMinutes: 300, Schedule: testSchedule()}

	tests := []struct {
		name      string
		now       time.Time
		usage     Usage
		ov        Overrides
		allowed   bool
		reason    string
		remaining time.Duration
	}{
		{
			name: "outside window", now: at(12, 0),
			allowed: false, reason: "Außerhalb der Spielzeit (nächste: 15:00)",
		},
		{
			name: "inside window, plenty left, window is the bound", now: at(17, 0),
			usage:   Usage{Today: 10 * time.Minute, Week: time.Hour},
			allowed: true, reason: "Spielzeit", remaining: 60 * time.Minute,
		},
		{
			name: "daily quota is the bound", now: at(15, 0),
			usage:   Usage{Today: 60 * time.Minute, Week: time.Hour},
			allowed: true, reason: "Spielzeit", remaining: 30 * time.Minute,
		},
		{
			name: "daily quota exhausted", now: at(16, 0),
			usage:   Usage{Today: 90 * time.Minute, Week: 2 * time.Hour},
			allowed: false, reason: "Tageslimit erreicht",
		},
		{
			name: "bonus extends daily quota", now: at(16, 0),
			usage: Usage{Today: 90 * time.Minute, Week: 2 * time.Hour},
			ov:    Overrides{BonusMinutes: 30}, allowed: true, reason: "Spielzeit", remaining: 30 * time.Minute,
		},
		{
			name: "weekly quota exhausted", now: at(16, 0),
			usage:   Usage{Today: 0, Week: 300 * time.Minute},
			allowed: false, reason: "Wochenlimit erreicht",
		},
		{
			name: "weekly quota is the bound", now: at(15, 0),
			usage:   Usage{Today: 0, Week: 290 * time.Minute},
			allowed: true, reason: "Spielzeit", remaining: 10 * time.Minute,
		},
		{
			name: "lock wins over window", now: at(16, 0),
			ov:      Overrides{LockUntil: time.Date(2026, 11, 19, 0, 0, 0, 0, berlin)},
			allowed: false, reason: "Gesperrt bis Do 00:00",
		},
		{
			name: "allow wins over window and quota", now: at(20, 0),
			usage:   Usage{Today: 200 * time.Minute, Week: 400 * time.Minute},
			ov:      Overrides{AllowUntil: at(20, 30)},
			allowed: true, reason: "Freigegeben bis 20:30", remaining: 30 * time.Minute,
		},
		{
			name: "expired overrides are ignored", now: at(16, 0),
			ov:      Overrides{LockUntil: at(15, 0), AllowUntil: at(15, 30)},
			allowed: true, reason: "Spielzeit", remaining: 90 * time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := Evaluate(limits, tc.usage, tc.ov, tc.now)
			if st.Allowed != tc.allowed || st.Reason != tc.reason {
				t.Fatalf("got allowed=%v reason=%q", st.Allowed, st.Reason)
			}
			if st.Remaining != tc.remaining {
				t.Fatalf("remaining = %v, want %v", st.Remaining, tc.remaining)
			}
		})
	}
}

func TestEvaluateUnlimited(t *testing.T) {
	everyDay := Schedule{}
	for _, d := range Weekdays {
		everyDay[d] = []Window{{Start: 0, End: 1440}}
	}
	st := Evaluate(Limits{Schedule: everyDay}, Usage{Today: 10 * time.Hour}, Overrides{}, at(12, 0))
	if !st.Allowed || st.LimitToday != 0 || st.LimitWeek != 0 {
		t.Fatalf("unexpected status: %+v", st)
	}
	if st.Remaining != 12*time.Hour {
		t.Fatalf("remaining = %v", st.Remaining)
	}
}

func TestEvaluateNoSchedule(t *testing.T) {
	st := Evaluate(Limits{DailyMinutes: 60}, Usage{}, Overrides{}, at(12, 0))
	if st.Allowed || st.Reason != "Außerhalb der Spielzeit" {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestFormatUntil(t *testing.T) {
	now := at(12, 0)
	if got := formatUntil(time.Date(2026, 12, 24, 18, 0, 0, 0, berlin), now); !strings.HasPrefix(got, "24.12.") {
		t.Errorf("far future = %q", got)
	}
}

func TestStartOfWeek(t *testing.T) {
	cases := map[time.Time]time.Time{
		at(12, 0): time.Date(2026, 11, 16, 0, 0, 0, 0, berlin),
		time.Date(2026, 11, 22, 23, 0, 0, 0, berlin): time.Date(2026, 11, 16, 0, 0, 0, 0, berlin),
		time.Date(2026, 11, 16, 0, 0, 0, 0, berlin):  time.Date(2026, 11, 16, 0, 0, 0, 0, berlin),
	}
	for in, want := range cases {
		if got := StartOfWeek(in); !got.Equal(want) {
			t.Errorf("StartOfWeek(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestNextChange(t *testing.T) {
	s := testSchedule()
	if got, ok := s.NextChange(at(16, 0)); !ok || !got.Equal(at(18, 0)) {
		t.Errorf("inside window: %v", got)
	}
	if got, _ := s.NextChange(at(12, 0)); !got.Equal(at(15, 0)) {
		t.Errorf("outside window: %v", got)
	}
	night := Schedule{
		time.Wednesday: {{Start: 20 * 60, End: 1440}},
		time.Thursday:  {{Start: 0, End: 7 * 60}},
	}
	if got, _ := night.NextChange(at(21, 0)); !got.Equal(time.Date(2026, 11, 19, 7, 0, 0, 0, berlin)) {
		t.Errorf("window over midnight: %v", got)
	}
	always := Schedule{}
	for _, d := range Weekdays {
		always[d] = []Window{{Start: 0, End: 1440}}
	}
	if _, ok := always.NextChange(at(12, 0)); ok {
		t.Error("a schedule without gaps never changes")
	}
	if _, ok := (Schedule{}).NextChange(at(12, 0)); ok {
		t.Error("an empty schedule never changes")
	}
}
