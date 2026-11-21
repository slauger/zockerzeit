// Package stats aggregates Nintendo play time into chart data.
package stats

import (
	"sort"
	"time"

	"github.com/slauger/zockerzeit/internal/store"
)

// Series is one named data series.
type Series struct {
	Key  string    `json:"key"`
	Name string    `json:"name"`
	Data []float64 `json:"data"`
}

// Daily is play time per day.
type Daily struct {
	Labels []string  `json:"labels"`
	Series []Series  `json:"series"`
	Total  []float64 `json:"total"`
}

// Slice is a named share, e.g. play time per console.
type Slice struct {
	Name    string  `json:"name"`
	Minutes float64 `json:"minutes"`
}

const dateLayout = "2006-01-02"

// DailyMinutes returns the play time per console and day for the days
// starting at from (local midnight).
func DailyMinutes(plays []store.NintendoPlay, from time.Time, days int, names map[string]string) Daily {
	d := Daily{Labels: make([]string, days), Total: make([]float64, days)}
	index := make(map[string]int, days)
	for i := range days {
		day := from.AddDate(0, 0, i)
		d.Labels[i] = weekdayDE[day.Weekday()] + day.Format(" 02.01.")
		index[day.Format(dateLayout)] = i
	}
	perConsole := map[string][]float64{}
	for _, p := range plays {
		i, ok := index[p.Date]
		if !ok {
			continue
		}
		if perConsole[p.DeviceID] == nil {
			perConsole[p.DeviceID] = make([]float64, days)
		}
		perConsole[p.DeviceID][i] += float64(p.Minutes)
		d.Total[i] += float64(p.Minutes)
	}
	for id, data := range perConsole {
		d.Series = append(d.Series, Series{Key: id, Name: nameOf(names, id), Data: data})
	}
	sort.Slice(d.Series, func(i, j int) bool { return d.Series[i].Name < d.Series[j].Name })
	return d
}

// Heatmap returns the average play time per weekday (Monday first) and
// hour. Nintendo reports minutes per day only, so the hour is when the
// minutes arrived, at most one sync interval after they were played.
func Heatmap(plays []store.NintendoPlay, loc *time.Location, weeks float64) [7][24]float64 {
	var out [7][24]float64
	for _, p := range plays {
		t := p.TS.In(loc)
		out[(int(t.Weekday())+6)%7][t.Hour()] += float64(p.Minutes)
	}
	for i := range out {
		for j := range out[i] {
			out[i][j] = max(out[i][j], 0)
			if weeks > 1 {
				out[i][j] /= weeks
			}
		}
	}
	return out
}

// Consoles returns the play time per console, largest first.
func Consoles(plays []store.NintendoPlay, names map[string]string) []Slice {
	m := map[string]int{}
	for _, p := range plays {
		m[p.DeviceID] += p.Minutes
	}
	out := make([]Slice, 0, len(m))
	for id, mins := range m {
		if mins > 0 {
			out = append(out, Slice{Name: nameOf(names, id), Minutes: float64(mins)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Minutes > out[j].Minutes })
	return out
}

func nameOf(names map[string]string, id string) string {
	if n, ok := names[id]; ok && n != "" {
		return n
	}
	return id
}

var weekdayDE = map[time.Weekday]string{
	time.Monday: "Mo", time.Tuesday: "Di", time.Wednesday: "Mi", time.Thursday: "Do",
	time.Friday: "Fr", time.Saturday: "Sa", time.Sunday: "So",
}
