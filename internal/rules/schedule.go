// Package rules contains the time window and quota logic that decides
// whether a profile is currently allowed to play.
package rules

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Window is a time range within a single day, in minutes since midnight.
// End is exclusive. End may be 1440 to express "until midnight".
type Window struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Schedule maps each weekday to the windows in which playing is allowed.
// A weekday without windows means no playing at all on that day.
type Schedule map[time.Weekday][]Window

// Weekdays lists the weekdays in German display order (Monday first).
var Weekdays = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

// WeekdayNames are the German short names used in the UI.
var WeekdayNames = map[time.Weekday]string{
	time.Monday:    "Mo",
	time.Tuesday:   "Di",
	time.Wednesday: "Mi",
	time.Thursday:  "Do",
	time.Friday:    "Fr",
	time.Saturday:  "Sa",
	time.Sunday:    "So",
}

// ParseWindows parses a comma separated list like "15:00-18:00, 19:00-20:00".
// An empty string yields no windows.
func ParseWindows(s string) ([]Window, error) {
	var out []Window
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "-")
		if !ok {
			return nil, fmt.Errorf("ungültiges Zeitfenster %q (erwartet HH:MM-HH:MM)", part)
		}
		start, err := parseClock(strings.TrimSpace(from))
		if err != nil {
			return nil, err
		}
		end, err := parseClock(strings.TrimSpace(to))
		if err != nil {
			return nil, err
		}
		if end <= start {
			return nil, fmt.Errorf("Zeitfenster %q endet vor dem Beginn", part)
		}
		out = append(out, Window{Start: start, End: end})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("ungültige Uhrzeit %q", s)
	}
	hh, err := strconv.Atoi(h)
	if err != nil {
		return 0, fmt.Errorf("ungültige Uhrzeit %q", s)
	}
	mm, err := strconv.Atoi(m)
	if err != nil {
		return 0, fmt.Errorf("ungültige Uhrzeit %q", s)
	}
	if hh == 24 && mm == 0 {
		return 1440, nil
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("ungültige Uhrzeit %q", s)
	}
	return hh*60 + mm, nil
}

// FormatWindows renders windows back into the "HH:MM-HH:MM, ..." form.
func FormatWindows(ws []Window) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		parts = append(parts, FormatClock(w.Start)+"-"+FormatClock(w.End))
	}
	return strings.Join(parts, ", ")
}

// FormatClock renders minutes since midnight as HH:MM.
func FormatClock(m int) string {
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// minuteOfDay returns the minutes since local midnight for t.
func minuteOfDay(t time.Time) int {
	return t.Hour()*60 + t.Minute()
}

// Current returns the window that contains t, if any.
func (s Schedule) Current(t time.Time) (Window, bool) {
	m := minuteOfDay(t)
	for _, w := range s[t.Weekday()] {
		if m >= w.Start && m < w.End {
			return w, true
		}
	}
	return Window{}, false
}

// MinutesLeftInWindow returns how many minutes remain in the current window,
// or 0 if t is outside any window.
func (s Schedule) MinutesLeftInWindow(t time.Time) int {
	w, ok := s.Current(t)
	if !ok {
		return 0
	}
	return w.End - minuteOfDay(t)
}

// Next returns the start of the next window strictly after t, searching up to
// one week ahead. ok is false if the schedule has no windows at all.
func (s Schedule) Next(t time.Time) (time.Time, bool) {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	m := minuteOfDay(t)
	for i := range 8 {
		d := day.AddDate(0, 0, i)
		for _, w := range s[d.Weekday()] {
			if i == 0 && w.Start <= m {
				continue
			}
			return time.Date(d.Year(), d.Month(), d.Day(), w.Start/60, w.Start%60, 0, 0, t.Location()), true
		}
	}
	return time.Time{}, false
}
