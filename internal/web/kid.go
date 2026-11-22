package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
)

// kidStats is the play time of a kid in the period chosen on the kid page.
type kidStats struct {
	Range      string // "heute", "7", "30" or "alle"
	Linked     bool
	Minutes    int
	PerDay     int // average per calendar day of the period
	DaysPlayed int
	Bars       []kidBar
	// Limit is the daily limit drawn into daily bars, LimitPct its height.
	Limit    int
	LimitPct int
	Games    []gameRow
}

// kidBar is one column of the kid chart, a day or a month.
type kidBar struct {
	Label   string
	Title   string
	Minutes int
	Percent int
	Current bool
	Over    bool
}

// kidRanges are the periods offered on the kid page.
var kidRanges = []struct{ Value, Label string }{
	{"heute", "Heute"}, {"7", "7 Tage"}, {"30", "30 Tage"}, {"alle", "Alles"},
}

var monthNames = [...]string{"Jan", "Feb", "Mär", "Apr", "Mai", "Jun", "Jul", "Aug", "Sep", "Okt", "Nov", "Dez"}

// maxMonthBars caps the "alle" chart to the most recent months.
const maxMonthBars = 12

func (s *Server) kidStats(p store.Profile, plan store.Plan, rng string, now time.Time, base string) kidStats {
	st := kidStats{Range: rng}
	today := rules.StartOfDay(now)
	from := today
	days := 1
	switch rng {
	case "7", "30":
		days, _ = strconv.Atoi(rng)
		from = today.AddDate(0, 0, 1-days)
	case "alle":
		from = time.Time{}
	default:
		st.Range = "heute"
	}
	v := s.nintendoGames(p.ID, from, today)
	st.Linked, st.Minutes = v.Linked, v.Minutes
	prefix := base + "/bild/"
	for _, g := range v.Top {
		g.ImageURI = strings.Replace(g.ImageURI, "/bild/", prefix, 1)
		st.Games = append(st.Games, g)
	}
	perDay := map[string]int{}
	first := today
	for _, d := range v.Days {
		if d.Minutes > 0 {
			st.DaysPlayed++
			perDay[d.Date.Format("2006-01-02")] += d.Minutes
			if d.Date.Before(first) {
				first = d.Date
			}
		}
	}

	switch st.Range {
	case "7", "30":
		st.PerDay = st.Minutes / days
		st.Limit = max(plan.Day(now.Weekday()).Limit, 0)
		for d := from; !d.After(today); d = d.AddDate(0, 0, 1) {
			m := perDay[d.Format("2006-01-02")]
			b := kidBar{
				Title:   fmt.Sprintf("%s %s: %s", rules.WeekdayNames[d.Weekday()], d.Format("02.01."), formatDuration(time.Duration(m)*time.Minute)),
				Minutes: m, Current: d.Equal(today), Over: st.Limit > 0 && m > st.Limit,
			}
			switch {
			case days <= 7:
				b.Label = rules.WeekdayNames[d.Weekday()]
			case d.Weekday() == time.Monday:
				b.Label = d.Format("2.")
			}
			st.Bars = append(st.Bars, b)
		}
	case "alle":
		if st.Minutes > 0 {
			st.PerDay = st.Minutes / (int(today.Sub(first).Hours()/24+0.5) + 1)
		}
		if today.Sub(first) < weekBarsUpTo {
			st.Bars = weekBars(perDay, rules.StartOfWeek(first), today)
		} else {
			st.Bars = monthBars(perDay, first, today)
		}
	}

	top := st.Limit
	for _, b := range st.Bars {
		top = max(top, b.Minutes)
	}
	if top > 0 {
		for i := range st.Bars {
			if st.Bars[i].Minutes > 0 {
				st.Bars[i].Percent = max(st.Bars[i].Minutes*100/top, 2)
			}
		}
		if st.Limit > 0 {
			st.LimitPct = st.Limit * 100 / top
		}
	}
	return st
}

// kidWindow describes today's play time of a plan, "" when the whole day
// is open.
func kidWindow(plan store.Plan, now time.Time) string {
	day := plan.Day(now.Weekday())
	if day.Limit == 0 && !plan.BuiltIn() {
		return "keine Zockzeit"
	}
	if w, ok := day.Window(); ok {
		return formatClock(w.Start) + " bis " + formatClock(w.End) + " Uhr"
	}
	return ""
}

// weekBarsUpTo is how much history "alle" shows per week instead of per month.
const weekBarsUpTo = 12 * 7 * 24 * time.Hour

func weekBars(perDay map[string]int, from, today time.Time) []kidBar {
	var bars []kidBar
	current := rules.StartOfWeek(today)
	for w := from; !w.After(today); w = w.AddDate(0, 0, 7) {
		m := 0
		for d := w; d.Before(w.AddDate(0, 0, 7)); d = d.AddDate(0, 0, 1) {
			m += perDay[d.Format("2006-01-02")]
		}
		bars = append(bars, kidBar{
			Label: w.Format("2.1."), Title: fmt.Sprintf("Woche ab %s: %s", w.Format("02.01."), formatDuration(time.Duration(m)*time.Minute)),
			Minutes: m, Current: w.Equal(current),
		})
	}
	return bars
}

func monthBars(perDay map[string]int, first, today time.Time) []kidBar {
	perMonth := map[string]int{}
	for date, m := range perDay {
		perMonth[date[:7]] += m
	}
	start := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, today.Location())
	end := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
	if lim := end.AddDate(0, 1-maxMonthBars, 0); start.Before(lim) {
		start = lim
	}
	var bars []kidBar
	for d := start; !d.After(end); d = d.AddDate(0, 1, 0) {
		m := perMonth[d.Format("2006-01")]
		name := monthNames[d.Month()-1]
		bars = append(bars, kidBar{
			Label: name, Title: fmt.Sprintf("%s %d: %s", name, d.Year(), formatDuration(time.Duration(m)*time.Minute)),
			Minutes: m, Current: d.Equal(end),
		})
	}
	return bars
}
