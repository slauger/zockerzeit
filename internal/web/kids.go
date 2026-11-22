package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/slauger/zockerzeit/internal/store"
)

const kidsPath = "/kinder"

// kidView is one kid on the kids page.
type kidView struct {
	store.Profile
	Consoles []store.NintendoDevice
	// Games are the most played games of the last 30 days.
	Games   gamesView
	Minutes int
}

type kidsData struct {
	Kids  []kidView
	Plans []store.Plan
}

// kidsGames is how many games the kids page shows per kid.
const kidsGames = 6

func (s *Server) kidsPage(w http.ResponseWriter, r *http.Request) {
	kids, err := s.store.Profiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	plans, _ := s.store.Plans()
	consoles, _ := s.store.NintendoDevices()
	now := s.engine.Now()
	data := kidsData{Plans: plans}
	for _, k := range kids {
		v := kidView{Profile: k, Games: s.nintendoGames(k.ID, now.AddDate(0, 0, -29), now)}
		if len(v.Games.Top) > kidsGames {
			v.Games.Top = v.Games.Top[:kidsGames]
		}
		for _, c := range consoles {
			if c.ProfileID == k.ID {
				v.Consoles = append(v.Consoles, c)
			}
		}
		data.Kids = append(data.Kids, v)
	}
	s.render(w, r, "kinder", "Kinder", data)
}

func (s *Server) saveKid(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectErr(w, r, kidsPath, errors.New("Name fehlt"))
		return
	}
	id := r.FormValue("id")
	if id == "" {
		id = slugify(name)
		if _, err := s.store.Profile(id); err == nil {
			redirectErr(w, r, kidsPath, fmt.Errorf("ein Kind %q gibt es schon", name))
			return
		}
	}
	plan, err := s.store.Plan(r.FormValue("plan"))
	if err != nil || plan.BuiltIn() {
		redirectErr(w, r, kidsPath, errors.New("bitte einen Plan als Standardplan wählen, zuerst unter Pläne einen anlegen"))
		return
	}
	p := store.Profile{ID: id, Name: name, PlanID: plan.ID}
	if err := s.store.SaveProfile(p); err != nil {
		redirectErr(w, r, kidsPath, err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), ProfileID: p.ID, Kind: "config", Message: fmt.Sprintf("%s: Standardplan %s", p.Name, plan.Name)})
	s.applyPlans(r)
	redirect(w, r, kidsPath, p.Name+" gespeichert")
}

func (s *Server) deleteKid(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteProfile(r.PathValue("id")); err != nil {
		redirectErr(w, r, kidsPath, err)
		return
	}
	redirect(w, r, kidsPath, "Kind gelöscht")
}
