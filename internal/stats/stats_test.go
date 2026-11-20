package stats

import (
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/store"
)

var loc, _ = time.LoadLocation("Europe/Berlin")

func sample(day, hh, mm int, mac, ap string, active bool) store.Sample {
	return store.Sample{
		TS: time.Date(2026, 11, day, hh, mm, 0, 0, loc), MAC: mac, ProfileID: "max",
		Seconds: 60, Active: active, APName: ap,
	}
}

func fixture() []store.Sample {
	var out []store.Sample
	// Wed 18.11.: switch 15:00-15:09 in the living room, iPhone 15:05-15:09 in parallel
	for m := 1; m <= 10; m++ {
		out = append(out, sample(18, 15, m, "sw", "AP Wohnzimmer", true))
		out = append(out, sample(18, 15, m, "ip", "AP Kinderzimmer", m > 5))
	}
	// Thu 19.11.: switch 16:00-16:04 and again 16:30-16:31
	for m := 1; m <= 5; m++ {
		out = append(out, sample(19, 16, m, "sw", "", true))
	}
	out = append(out, sample(19, 16, 31, "sw", "AP Wohnzimmer", true))
	return out
}

var names = map[string]string{"sw": "Switch", "ip": "iPhone"}

func TestDailyMinutes(t *testing.T) {
	from := time.Date(2026, 11, 17, 0, 0, 0, 0, loc)
	d := DailyMinutes(fixture(), from, 3, names)
	if len(d.Labels) != 3 || d.Labels[0] != "Di 17.11." || d.Labels[1] != "Mi 18.11." {
		t.Fatalf("labels = %v", d.Labels)
	}
	if len(d.Series) != 2 || d.Series[0].Name != "Switch" || d.Series[1].Name != "iPhone" {
		t.Fatalf("series = %+v", d.Series)
	}
	if d.Series[0].Data[1] != 10 || d.Series[0].Data[2] != 6 || d.Series[1].Data[1] != 5 {
		t.Fatalf("data = %+v", d.Series)
	}
	if d.Total[1] != 10 || d.Total[2] != 6 || d.Total[0] != 0 {
		t.Fatalf("total = %v", d.Total)
	}
	// Samples outside the range are ignored.
	d = DailyMinutes(fixture(), from.AddDate(0, 0, 5), 2, names)
	if len(d.Series) != 0 {
		t.Fatalf("expected no series, got %+v", d.Series)
	}
}

func TestHeatmap(t *testing.T) {
	h := Heatmap(fixture(), 1)
	if h[2][15] != 10 || h[3][16] != 6 {
		t.Fatalf("unexpected heatmap: wed15=%v thu16=%v", h[2][15], h[3][16])
	}
	h = Heatmap(fixture(), 2)
	if h[2][15] != 5 {
		t.Fatalf("average over 2 weeks = %v", h[2][15])
	}
}

func TestLocationsAndDevices(t *testing.T) {
	l := Locations(fixture())
	if len(l) != 3 || l[0].Name != "AP Wohnzimmer" || l[0].Minutes != 11 {
		t.Fatalf("locations = %+v", l)
	}
	d := Devices(fixture(), names)
	if len(d) != 2 || d[0].Name != "Switch" || d[0].Minutes != 16 || d[1].Minutes != 5 {
		t.Fatalf("devices = %+v", d)
	}
}

func TestSessions(t *testing.T) {
	s := Sessions(fixture(), map[string]string{}, 2*time.Minute)
	if len(s) != 4 {
		t.Fatalf("expected 4 sessions, got %+v", s)
	}
	first := s[0]
	if first.Device != "sw" || !first.Start.Equal(time.Date(2026, 11, 18, 15, 0, 0, 0, loc)) || !first.End.Equal(time.Date(2026, 11, 18, 15, 10, 0, 0, loc)) {
		t.Fatalf("first session = %+v", first)
	}
}

func TestGermanWeekday(t *testing.T) {
	if germanWeekday("x") != "x" || germanWeekday("Foo 1") != "Foo 1" {
		t.Fatal("unexpected passthrough")
	}
}
