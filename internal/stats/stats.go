// Package stats aggregates usage samples into chart data.
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

// Slice is a named share, e.g. play time per access point.
type Slice struct {
	Name    string  `json:"name"`
	Minutes float64 `json:"minutes"`
}

// Session is a contiguous period of activity on one device.
type Session struct {
	Device string    `json:"device"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	AP     string    `json:"ap"`
}

func minutes(secs int) float64 { return float64(secs) / 60 }

func dayIndex(t, from time.Time) int {
	a := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	return int(a.Sub(b).Hours() / 24)
}

// DailyMinutes returns active minutes per device and day for the days
// starting at from (local midnight). Total counts parallel use only once.
func DailyMinutes(samples []store.Sample, from time.Time, days int, names map[string]string) Daily {
	d := Daily{Labels: make([]string, days), Total: make([]float64, days)}
	for i := range days {
		d.Labels[i] = from.AddDate(0, 0, i).Format("Mon 02.01.")
	}
	perDevice := map[string][]float64{}
	totalTS := map[int64]int{}
	totalDay := map[int64]int{}
	for _, s := range samples {
		if !s.Active {
			continue
		}
		idx := dayIndex(s.TS, from)
		if idx < 0 || idx >= days {
			continue
		}
		if perDevice[s.MAC] == nil {
			perDevice[s.MAC] = make([]float64, days)
		}
		perDevice[s.MAC][idx] += minutes(s.Seconds)
		ts := s.TS.Unix()
		if s.Seconds > totalTS[ts] {
			totalTS[ts] = s.Seconds
			totalDay[ts] = idx
		}
	}
	for ts, secs := range totalTS {
		d.Total[totalDay[ts]] += minutes(secs)
	}
	for mac, data := range perDevice {
		d.Series = append(d.Series, Series{Key: mac, Name: nameOf(names, mac), Data: data})
	}
	sort.Slice(d.Series, func(i, j int) bool { return d.Series[i].Name < d.Series[j].Name })
	for i, l := range d.Labels {
		d.Labels[i] = germanWeekday(l)
	}
	return d
}

// Heatmap returns average active minutes per weekday (Monday first) and hour.
func Heatmap(samples []store.Sample, weeks float64) [7][24]float64 {
	var out [7][24]float64
	seen := map[int64]bool{}
	for _, s := range samples {
		if !s.Active || seen[s.TS.Unix()] {
			continue
		}
		seen[s.TS.Unix()] = true
		wd := (int(s.TS.Weekday()) + 6) % 7
		out[wd][s.TS.Hour()] += minutes(s.Seconds)
	}
	if weeks > 1 {
		for i := range out {
			for j := range out[i] {
				out[i][j] /= weeks
			}
		}
	}
	return out
}

// Locations returns active minutes per access point, largest first.
func Locations(samples []store.Sample) []Slice {
	m := map[string]int{}
	for _, s := range samples {
		if !s.Active {
			continue
		}
		name := s.APName
		if name == "" {
			name = "unbekannt"
		}
		m[name] += s.Seconds
	}
	out := make([]Slice, 0, len(m))
	for name, secs := range m {
		out = append(out, Slice{Name: name, Minutes: minutes(secs)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Minutes > out[j].Minutes })
	return out
}

// Devices returns active minutes per device, largest first.
func Devices(samples []store.Sample, names map[string]string) []Slice {
	m := map[string]int{}
	for _, s := range samples {
		if s.Active {
			m[s.MAC] += s.Seconds
		}
	}
	out := make([]Slice, 0, len(m))
	for mac, secs := range m {
		out = append(out, Slice{Name: nameOf(names, mac), Minutes: minutes(secs)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Minutes > out[j].Minutes })
	return out
}

// Sessions merges consecutive active samples per device into sessions.
// Gaps longer than maxGap start a new session.
func Sessions(samples []store.Sample, names map[string]string, maxGap time.Duration) []Session {
	byMAC := map[string][]store.Sample{}
	for _, s := range samples {
		if s.Active {
			byMAC[s.MAC] = append(byMAC[s.MAC], s)
		}
	}
	var out []Session
	for mac, list := range byMAC {
		sort.Slice(list, func(i, j int) bool { return list[i].TS.Before(list[j].TS) })
		var cur *Session
		for _, s := range list {
			start := s.TS.Add(-time.Duration(s.Seconds) * time.Second)
			if cur != nil && start.Sub(cur.End) <= maxGap {
				cur.End = s.TS
				continue
			}
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &Session{Device: nameOf(names, mac), Start: start, End: s.TS, AP: s.APName}
		}
		if cur != nil {
			out = append(out, *cur)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

func nameOf(names map[string]string, mac string) string {
	if n, ok := names[mac]; ok && n != "" {
		return n
	}
	return mac
}

var weekdayDE = map[string]string{
	"Mon": "Mo", "Tue": "Di", "Wed": "Mi", "Thu": "Do", "Fri": "Fr", "Sat": "Sa", "Sun": "So",
}

func germanWeekday(label string) string {
	if len(label) < 3 {
		return label
	}
	if de, ok := weekdayDE[label[:3]]; ok {
		return de + label[3:]
	}
	return label
}
