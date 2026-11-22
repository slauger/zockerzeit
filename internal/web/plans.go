package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
)

const plansPath = "/plaene"

// planDayView is one weekday of a plan in the form.
type planDayView struct {
	Day   time.Weekday
	From  string
	To    string
	Limit string
	Slot  []slot
}

type planView struct {
	store.Plan
	New  bool
	Days []planDayView
	// Kids have this plan as standard plan.
	Kids []string
}

type plansData struct {
	Plans []planView
}

func formatClock(m int) string {
	if m == 24*60 {
		return "24:00"
	}
	return rules.FormatClock(m)
}

func newPlanView(p store.Plan, isNew bool, kids []store.Profile) planView {
	v := planView{Plan: p, New: isNew}
	for _, k := range kids {
		if k.PlanID == p.ID {
			v.Kids = append(v.Kids, k.Name)
		}
	}
	for _, d := range rules.Weekdays {
		day := p.Day(d)
		dv := planDayView{Day: d}
		if w, ok := day.Window(); ok {
			dv.From, dv.To = formatClock(w.Start), formatClock(w.End)
		}
		switch {
		case day.Limit > 0:
			dv.Limit = strconv.Itoa(day.Limit)
		case day.Limit == 0:
			dv.Limit = "0"
		}
		if day.Limit != 0 {
			w, _ := day.Window()
			dv.Slot = slots([]rules.Window{w})
		}
		v.Days = append(v.Days, dv)
	}
	return v
}

// newPlanTemplate is the starting point offered for a new plan.
func newPlanTemplate() store.Plan {
	p := store.Plan{Days: map[time.Weekday]store.PlanDay{}}
	for _, d := range rules.Weekdays {
		p.Days[d] = store.PlanDay{From: 9 * 60, To: 19 * 60, Limit: 60}
		if d == time.Saturday || d == time.Sunday {
			p.Days[d] = store.PlanDay{From: 9 * 60, To: 20 * 60, Limit: 90}
		}
	}
	return p
}

func (s *Server) plansPage(w http.ResponseWriter, r *http.Request) {
	kids, err := s.store.Profiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	plans, err := s.store.Plans()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var data plansData
	for _, p := range plans {
		data.Plans = append(data.Plans, newPlanView(p, false, kids))
	}
	data.Plans = append(data.Plans, newPlanView(newPlanTemplate(), true, nil))
	s.render(w, r, "plaene", "Pläne", data)
}

// parsePlanDay reads one weekday of the plan form. Times must fit what
// the Nintendo parental controls can store.
func parsePlanDay(r *http.Request, d time.Weekday) (store.PlanDay, error) {
	key := strconv.Itoa(int(d))
	name := rules.WeekdayNames[d]
	var day store.PlanDay
	limit := strings.TrimSpace(r.FormValue("limit_" + key))
	switch limit {
	case "":
		day.Limit = -1
	default:
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 || n > 360 {
			return day, fmt.Errorf("%s: das Limit muss zwischen 0 und 360 Minuten liegen", name)
		}
		day.Limit = n
	}
	from, to := strings.TrimSpace(r.FormValue("from_"+key)), strings.TrimSpace(r.FormValue("to_"+key))
	if from == "" && to == "" {
		return day, nil
	}
	if to == "24:00" {
		to = "23:59"
	}
	ws, err := rules.ParseWindows(from + "-" + to)
	if err != nil || len(ws) != 1 {
		return day, fmt.Errorf("%s: bitte Beginn und Ende als HH:MM angeben", name)
	}
	day.From, day.To = ws[0].Start, ws[0].End
	if day.To == 23*60+59 {
		day.To = 24 * 60
	}
	if day.From < 5*60 || day.From > 9*60 {
		return day, fmt.Errorf("%s: Nintendo erlaubt den Beginn nur zwischen 05:00 und 09:00", name)
	}
	if day.To < 16*60 || (day.To > 23*60 && day.To != 24*60) {
		return day, fmt.Errorf("%s: Nintendo erlaubt das Ende nur zwischen 16:00 und 23:00 oder um 24:00", name)
	}
	return day, nil
}

func (s *Server) savePlan(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectErr(w, r, plansPath, errors.New("der Plan braucht einen Namen"))
		return
	}
	p := store.Plan{ID: r.FormValue("id"), Name: name, Days: map[time.Weekday]store.PlanDay{}}
	if p.ID == "" {
		p.ID = slugify(name)
		if _, err := s.store.Plan(p.ID); err == nil {
			redirectErr(w, r, plansPath, fmt.Errorf("einen Plan %q gibt es schon", name))
			return
		}
	}
	for _, d := range rules.Weekdays {
		day, err := parsePlanDay(r, d)
		if err != nil {
			redirectErr(w, r, plansPath, err)
			return
		}
		p.Days[d] = day
	}
	if err := s.store.SavePlan(p); err != nil {
		redirectErr(w, r, plansPath, err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), Kind: "config", Message: "Plan " + p.Name + " gespeichert"})
	s.applyPlans(r)
	redirect(w, r, plansPath+"#plan-"+p.ID, "Plan "+p.Name+" gespeichert")
}

func (s *Server) deletePlan(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeletePlan(r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrPlanInUse):
		err = errors.New("der Plan ist noch Standardplan eines Kindes")
	case errors.Is(err, store.ErrNotFound):
		err = errors.New("Plan nicht gefunden")
	}
	if err != nil {
		redirectErr(w, r, plansPath, err)
		return
	}
	s.applyPlans(r)
	redirect(w, r, plansPath, "Plan gelöscht")
}
