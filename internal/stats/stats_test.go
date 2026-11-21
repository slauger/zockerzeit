package stats

import (
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/store"
)

var berlin, _ = time.LoadLocation("Europe/Berlin")

func plays() []store.NintendoPlay {
	at := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, berlin) }
	return []store.NintendoPlay{
		{TS: at(16, 15), Date: "2026-11-16", DeviceID: "D1", Minutes: 30},
		{TS: at(16, 16), Date: "2026-11-16", DeviceID: "D2", Minutes: 10},
		{TS: at(17, 15), Date: "2026-11-17", DeviceID: "D1", Minutes: 20},
		// Nintendo corrected the day downwards.
		{TS: at(17, 18), Date: "2026-11-17", DeviceID: "D1", Minutes: -5},
		{TS: at(1, 12), Date: "2026-11-01", DeviceID: "D1", Minutes: 99},
	}
}

func TestDailyMinutes(t *testing.T) {
	from := time.Date(2026, 11, 16, 0, 0, 0, 0, berlin)
	d := DailyMinutes(plays(), from, 3, map[string]string{"D1": "Switch Max"})
	if len(d.Labels) != 3 || d.Labels[0] != "Mo 16.11." || d.Labels[2] != "Mi 18.11." {
		t.Fatalf("labels = %v", d.Labels)
	}
	if d.Total[0] != 40 || d.Total[1] != 15 || d.Total[2] != 0 {
		t.Fatalf("total = %v", d.Total)
	}
	if len(d.Series) != 2 || d.Series[0].Name != "D2" || d.Series[1].Name != "Switch Max" || d.Series[1].Data[1] != 15 {
		t.Fatalf("series = %+v", d.Series)
	}
}

func TestHeatmap(t *testing.T) {
	hm := Heatmap(plays(), berlin, 2)
	if hm[0][15] != 15 || hm[0][16] != 5 || hm[1][15] != 10 || hm[6][12] != 49.5 {
		t.Fatalf("heatmap = %v %v %v %v", hm[0][15], hm[0][16], hm[1][15], hm[6][12])
	}
	if hm[1][18] != 0 {
		t.Fatalf("negative cell = %v", hm[1][18])
	}
}

func TestConsoles(t *testing.T) {
	c := Consoles(plays(), map[string]string{"D2": "Switch Mia"})
	if len(c) != 2 || c[0].Name != "D1" || c[0].Minutes != 144 || c[1].Name != "Switch Mia" {
		t.Fatalf("consoles = %+v", c)
	}
	if c := Consoles([]store.NintendoPlay{{DeviceID: "D1", Minutes: -3}}, nil); len(c) != 0 {
		t.Fatalf("negative console = %+v", c)
	}
}
