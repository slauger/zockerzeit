package rules

import (
	"fmt"
	"time"
)

// Limits holds the play time budget of a profile. A value of 0 means
// "no limit" for the respective period.
type Limits struct {
	DailyMinutes  int
	WeeklyMinutes int
	Schedule      Schedule
}

// Usage is the play time already consumed by a profile.
type Usage struct {
	Today time.Duration
	Week  time.Duration
}

// Overrides are manual parent decisions that take precedence over the
// regular rules.
type Overrides struct {
	// LockUntil blocks the profile until the given time.
	LockUntil time.Time
	// AllowUntil allows the profile until the given time, ignoring windows
	// and quotas.
	AllowUntil time.Time
	// BonusMinutes extends today's (and this week's) quota.
	BonusMinutes int
}

// Status is the outcome of evaluating a profile at a point in time.
type Status struct {
	Allowed    bool          `json:"allowed"`
	Reason     string        `json:"reason"`
	UsedToday  time.Duration `json:"usedToday"`
	UsedWeek   time.Duration `json:"usedWeek"`
	LimitToday int           `json:"limitToday"`
	LimitWeek  int           `json:"limitWeek"`
	// Remaining is the play time left right now. Negative means unlimited.
	Remaining  time.Duration `json:"remaining"`
	InWindow   bool          `json:"inWindow"`
	NextWindow time.Time     `json:"nextWindow"`
}

// Unlimited marks a Status.Remaining without an upper bound.
const Unlimited = time.Duration(-1)

// Evaluate decides whether a profile may play at now.
func Evaluate(l Limits, u Usage, o Overrides, now time.Time) Status {
	st := Status{
		UsedToday: u.Today,
		UsedWeek:  u.Week,
		Remaining: Unlimited,
	}
	if l.DailyMinutes > 0 {
		st.LimitToday = l.DailyMinutes + o.BonusMinutes
	}
	if l.WeeklyMinutes > 0 {
		st.LimitWeek = l.WeeklyMinutes + o.BonusMinutes
	}
	_, st.InWindow = l.Schedule.Current(now)
	if next, ok := l.Schedule.Next(now); ok {
		st.NextWindow = next
	}

	if o.LockUntil.After(now) {
		st.Reason = "Gesperrt bis " + formatUntil(o.LockUntil, now)
		st.Remaining = 0
		return st
	}
	if o.AllowUntil.After(now) {
		st.Allowed = true
		st.Reason = "Freigegeben bis " + formatUntil(o.AllowUntil, now)
		st.Remaining = o.AllowUntil.Sub(now)
		return st
	}
	if !st.InWindow {
		st.Reason = "Außerhalb der Spielzeit"
		if !st.NextWindow.IsZero() {
			st.Reason += " (nächste: " + formatUntil(st.NextWindow, now) + ")"
		}
		st.Remaining = 0
		return st
	}

	remaining := time.Duration(l.Schedule.MinutesLeftInWindow(now)) * time.Minute
	if st.LimitToday > 0 {
		left := time.Duration(st.LimitToday)*time.Minute - u.Today
		if left <= 0 {
			st.Reason = "Tageslimit erreicht"
			st.Remaining = 0
			return st
		}
		remaining = min(remaining, left)
	}
	if st.LimitWeek > 0 {
		left := time.Duration(st.LimitWeek)*time.Minute - u.Week
		if left <= 0 {
			st.Reason = "Wochenlimit erreicht"
			st.Remaining = 0
			return st
		}
		remaining = min(remaining, left)
	}

	st.Allowed = true
	st.Reason = "Spielzeit"
	st.Remaining = remaining
	return st
}

// formatUntil renders t relative to now: only the clock for today, otherwise
// prefixed with the weekday.
func formatUntil(t, now time.Time) string {
	clock := t.Format("15:04")
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return clock
	}
	if t.Sub(now) < 7*24*time.Hour {
		return fmt.Sprintf("%s %s", WeekdayNames[t.Weekday()], clock)
	}
	return t.Format("02.01. 15:04")
}

// StartOfDay returns local midnight of t.
func StartOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// StartOfWeek returns local midnight of the Monday of t's week.
func StartOfWeek(t time.Time) time.Time {
	d := StartOfDay(t)
	offset := (int(d.Weekday()) + 6) % 7
	return d.AddDate(0, 0, -offset)
}
