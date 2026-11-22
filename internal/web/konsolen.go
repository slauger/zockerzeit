package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/slauger/zockerzeit/internal/nintendo"
	"github.com/slauger/zockerzeit/internal/store"
)

// consoleView is one console on the consoles page.
type consoleView struct {
	store.NintendoDevice
	Owner string // name of the kid the console belongs to
	// Plan is the plan in force for the owner, Synced whether the console
	// has it.
	Plan   string
	Synced bool
	Today  int
	Games  []string
}

type consolesData struct {
	Consoles []consoleView
	Profiles []store.Profile
	Nintendo bool
}

func (s *Server) consolesPage(w http.ResponseWriter, r *http.Request) {
	consoles, err := s.engine.Consoles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := s.engine.Now()
	profiles, _ := s.store.Profiles()
	ovs, _ := s.store.ActiveOverrides(now, s.engine.Location())
	usage, _ := s.store.NintendoUsageOn(now.Format("2006-01-02"))
	titles, _ := s.store.NintendoTitles()
	data := consolesData{Profiles: profiles, Nintendo: s.opts.Nintendo != nil}
	for _, c := range consoles {
		v := consoleView{NintendoDevice: c}
		for _, p := range profiles {
			if p.ID != c.ProfileID {
				continue
			}
			v.Owner = p.Name
			if plan, _, err := s.store.ActivePlan(p, ovs); err == nil {
				v.Plan = plan.Name
				v.Synced = c.Applied == nintendo.Fingerprint(plan)
			}
		}
		perGame := map[string]int{}
		var order []string
		for _, u := range usage {
			if u.DeviceID != c.DeviceID {
				continue
			}
			v.Today += u.Minutes
			for _, g := range u.Games {
				name := titles[strings.ToUpper(g.ApplicationID)].Name
				if name == "" {
					name = "Unbekanntes Spiel"
				}
				if _, ok := perGame[name]; !ok {
					order = append(order, name)
				}
				perGame[name] += g.Minutes
			}
		}
		for _, name := range order {
			v.Games = append(v.Games, fmt.Sprintf("%s %s", name, formatDuration(time.Duration(perGame[name])*time.Minute)))
		}
		data.Consoles = append(data.Consoles, v)
	}
	s.render(w, r, "konsolen", "Konsolen", data)
}

// saveConsole renames a console and sets its owner. A new owner gets the
// play time of the console attributed again, including the past, and the
// console gets the owner's plan.
func (s *Server) saveConsole(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.NintendoDevice(r.PathValue("id"))
	if err != nil {
		redirectErr(w, r, "/konsolen", err)
		return
	}
	owner := r.FormValue("profile")
	if owner != "" {
		if _, err := s.store.Profile(owner); err != nil {
			redirectErr(w, r, "/konsolen", fmt.Errorf("unbekanntes Kind %q", owner))
			return
		}
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := s.store.SetNintendoDevice(c.DeviceID, name, owner); err != nil {
		redirectErr(w, r, "/konsolen", err)
		return
	}
	if owner != c.ProfileID {
		if err := s.reattribute(s.store.ResetNintendoPlay(c.DeviceID)); err != nil {
			redirectErr(w, r, "/konsolen", err)
			return
		}
	}
	if name == "" {
		name = c.Label
	}
	msg := name + " gespeichert"
	if owner != c.ProfileID {
		msg = fmt.Sprintf("%s gehört jetzt %s", name, s.profileName(owner))
		if owner == "" {
			msg = name + " gehört jetzt niemandem"
		}
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), ProfileID: owner, DeviceID: c.DeviceID, Kind: "config", Message: msg})
	s.applyPlans(r)
	redirect(w, r, "/konsolen", msg)
}
