package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
)

// Built-in plans. They are not stored and can't be edited.
const (
	PlanFree   = "frei"     // no time window and no limit
	PlanLocked = "gesperrt" // no play time at all
)

// PlanDay is the rule of one weekday, the way Nintendo stores it per day.
type PlanDay struct {
	// From and To bound the play time in minutes after midnight. Both 0
	// means the whole day.
	From int `json:"from"`
	To   int `json:"to"`
	// Limit is the play time in minutes: 0 none at all, -1 without a limit.
	Limit int `json:"limit"`
}

// Window reports the time window of the day; ok is false for the whole day.
func (d PlanDay) Window() (rules.Window, bool) {
	if d.From == 0 && d.To == 0 {
		return rules.Window{Start: 0, End: 1440}, false
	}
	return rules.Window{Start: d.From, End: d.To}, true
}

// Plan is a named set of rules per weekday. Kids have one as their
// standard plan and can be switched to another one for a while.
type Plan struct {
	ID   string                   `json:"id"`
	Name string                   `json:"name"`
	Days map[time.Weekday]PlanDay `json:"days"`
}

// Day returns the rule of a weekday. A day that was never set has no play
// time.
func (p Plan) Day(d time.Weekday) PlanDay {
	if day, ok := p.Days[d]; ok {
		return day
	}
	return PlanDay{}
}

// BuiltIn reports whether the plan is Frei or Gesperrt.
func (p Plan) BuiltIn() bool { return p.ID == PlanFree || p.ID == PlanLocked }

// Limits converts the plan into rule input for the day of now.
func (p Plan) Limits(now time.Time) rules.Limits {
	l := rules.Limits{Schedule: rules.Schedule{}}
	for _, d := range rules.Weekdays {
		day := p.Day(d)
		if day.Limit == 0 {
			continue
		}
		w, _ := day.Window()
		l.Schedule[d] = []rules.Window{w}
	}
	if limit := p.Day(now.Weekday()).Limit; limit > 0 {
		l.DailyMinutes = limit
	}
	return l
}

// builtInPlans returns Frei and Gesperrt.
func builtInPlans() []Plan {
	free := Plan{ID: PlanFree, Name: "Frei", Days: map[time.Weekday]PlanDay{}}
	locked := Plan{ID: PlanLocked, Name: "Gesperrt", Days: map[time.Weekday]PlanDay{}}
	for _, d := range rules.Weekdays {
		free.Days[d] = PlanDay{Limit: -1}
		locked.Days[d] = PlanDay{Limit: 0}
	}
	return []Plan{free, locked}
}

// Plans returns the stored plans ordered by name, without the built-in ones.
func (s *Store) Plans() ([]Plan, error) {
	rows, err := s.db.Query(`SELECT id, name, days FROM plans ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Plan returns a stored or built-in plan.
func (s *Store) Plan(id string) (Plan, error) {
	for _, p := range builtInPlans() {
		if p.ID == id {
			return p, nil
		}
	}
	p, err := scanPlan(s.db.QueryRow(`SELECT id, name, days FROM plans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func scanPlan(r scanner) (Plan, error) {
	var p Plan
	var days string
	if err := r.Scan(&p.ID, &p.Name, &days); err != nil {
		return p, err
	}
	p.Days = map[time.Weekday]PlanDay{}
	if err := json.Unmarshal([]byte(days), &p.Days); err != nil {
		return p, fmt.Errorf("plan %s: %w", p.ID, err)
	}
	return p, nil
}

// SavePlan inserts or updates a plan. Built-in plans can't be saved.
func (s *Store) SavePlan(p Plan) error {
	if p.BuiltIn() {
		return errors.New("Frei und Gesperrt lassen sich nicht ändern")
	}
	b, err := json.Marshal(p.Days)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO plans (id, name, days) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, days = excluded.days`, p.ID, p.Name, string(b))
	return err
}

// ErrPlanInUse is returned when a plan to delete is a kid's standard plan.
var ErrPlanInUse = errors.New("plan in use")

// DeletePlan removes a plan that no kid has as standard plan. Switches to
// it end.
func (s *Store) DeletePlan(id string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM profiles WHERE plan_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrPlanInUse
	}
	if _, err := s.db.Exec(`DELETE FROM overrides WHERE kind = ? AND plan_id = ?`, KindPlan, id); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM plans WHERE id = ?`, id)
	return affected(res, err)
}

// ActivePlan returns the plan of a kid at now from its active overrides:
// the newest switch, otherwise the standard plan. until is when a switch
// ends, zero for the standard plan.
func (s *Store) ActivePlan(p Profile, ovs []Override) (Plan, time.Time, error) {
	var sw *Override
	for i, o := range ovs {
		if o.Kind == KindPlan && o.ProfileID == p.ID && (sw == nil || !o.CreatedAt.Before(sw.CreatedAt)) {
			sw = &ovs[i]
		}
	}
	if sw != nil {
		plan, err := s.Plan(sw.PlanID)
		if err == nil {
			return plan, sw.Until, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return plan, time.Time{}, err
		}
	}
	if p.PlanID == "" {
		return Plan{}, time.Time{}, ErrNotFound
	}
	plan, err := s.Plan(p.PlanID)
	return plan, time.Time{}, err
}
