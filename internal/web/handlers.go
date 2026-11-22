package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/slauger/zockerzeit/internal/engine"
	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/stats"
	"github.com/slauger/zockerzeit/internal/store"
)

// --- dashboard ----------------------------------------------------------

type dashboardData struct {
	Nintendo map[string]nintendoToday
	Views    []engine.ProfileView
	Names    map[string]string
	At       time.Time
	// Unowned are consoles that count for nobody yet.
	Unowned []store.NintendoDevice
	// Plans can be switched to, the built-in ones last.
	Plans []store.Plan
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	views, err := s.engine.Evaluate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := dashboardData{Views: views, Names: s.profileNames(), At: s.engine.Now(), Nintendo: s.nintendoByProfile()}
	consoles, _ := s.engine.Consoles()
	for _, c := range consoles {
		if c.ProfileID == "" {
			data.Unowned = append(data.Unowned, c)
		}
	}
	data.Plans = s.allPlans()
	s.render(w, r, "dashboard", "Übersicht", data)
}

func (s *Server) statusAPI(w http.ResponseWriter, _ *http.Request) {
	views, err := s.engine.Evaluate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	snap := s.engine.Snapshot()
	writeJSON(w, map[string]any{"at": snap.At, "error": snap.Error, "profiles": views})
}

// --- parent actions -----------------------------------------------------

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	now := s.engine.Now()
	back := backTarget(r)
	switch r.FormValue("action") {
	case "plan":
		s.switchPlan(w, r, now)
	case "sperren":
		r.Form.Set("plan", store.PlanLocked)
		r.Form.Set("dauer", "morgen")
		s.switchPlan(w, r, now)
	case "bonus":
		s.bonus(w, r, now)
	default:
		redirectErr(w, r, back, errors.New("unbekannte Aktion"))
	}
}

// bonus gives a kid extra minutes today, in Zockerzeit and as extra play
// time on the consoles.
func (s *Server) bonus(w http.ResponseWriter, r *http.Request, now time.Time) {
	back := backTarget(r)
	minutes, _ := strconv.Atoi(r.FormValue("minutes"))
	p, err := s.store.Profile(r.FormValue("profile"))
	if err != nil || minutes <= 0 {
		redirectErr(w, r, back, errors.New("bitte Kind und Minuten angeben"))
		return
	}
	msg := fmt.Sprintf("%s bekommt heute %d Min extra", p.Name, minutes)
	o := store.Override{Kind: store.KindBonus, ProfileID: p.ID, Minutes: minutes, StartsAt: now, Until: rules.StartOfDay(now).AddDate(0, 0, 1), Note: msg}
	if _, err := s.store.AddOverride(o); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: now, ProfileID: p.ID, Kind: "parent", Message: msg})
	if s.opts.Nintendo != nil {
		if err := s.opts.Nintendo.AddExtraTime(r.Context(), p.ID, minutes); err != nil {
			s.applyNow(r)
			redirectErr(w, r, back, fmt.Errorf("%s, aber nicht an der Switch: %w", msg, err))
			return
		}
	}
	s.applyNow(r)
	redirect(w, r, back, msg)
}

// switchPlan puts one kid or all kids on another plan: until the next
// morning (dauer=morgen), including a date (dauer=datum, bis=YYYY-MM-DD) or
// for good (dauer=dauerhaft), which makes it the standard plan.
func (s *Server) switchPlan(w http.ResponseWriter, r *http.Request, now time.Time) {
	back := backTarget(r)
	plan, err := s.store.Plan(r.FormValue("plan"))
	if err != nil {
		redirectErr(w, r, back, errors.New("bitte einen Plan wählen"))
		return
	}
	var kids []store.Profile
	if id := r.FormValue("profile"); id == "alle" {
		kids, _ = s.store.Profiles()
	} else if p, err := s.store.Profile(id); err == nil {
		kids = []store.Profile{p}
	}
	if len(kids) == 0 {
		redirectErr(w, r, back, errors.New("bitte ein Kind wählen"))
		return
	}
	var day time.Time // the day whose morning ends the switch, zero for the next morning
	switch r.FormValue("dauer") {
	case "morgen":
	case "datum":
		d, err := time.ParseInLocation("2006-01-02", r.FormValue("bis"), s.engine.Location())
		if err != nil || d.Before(rules.StartOfDay(now)) || d.After(now.AddDate(1, 0, 0)) {
			redirectErr(w, r, back, errors.New("bitte ein Datum ab heute und höchstens ein Jahr voraus wählen"))
			return
		}
		day = d.AddDate(0, 0, 1)
	case "dauerhaft":
		if plan.BuiltIn() {
			redirectErr(w, r, back, errors.New("Frei und Gesperrt gehen nur auf Zeit"))
			return
		}
	default:
		redirectErr(w, r, back, errors.New("bitte wählen, wie lange"))
		return
	}
	ovs, _ := s.store.ActiveOverrides(now, s.engine.Location())
	var names []string
	var until time.Time
	for _, p := range kids {
		// A new switch replaces the running one.
		for _, o := range ovs {
			if o.Kind == store.KindPlan && o.ProfileID == p.ID {
				_ = s.store.EndOverride(o.ID, now)
			}
		}
		names = append(names, p.Name)
		if r.FormValue("dauer") == "dauerhaft" {
			p.PlanID = plan.ID
			if err := s.store.SaveProfile(p); err != nil {
				redirectErr(w, r, back, err)
				return
			}
			_ = s.store.AddEvent(store.Event{TS: now, ProfileID: p.ID, Kind: "parent", Message: fmt.Sprintf("%s: Standardplan jetzt %s", p.Name, plan.Name)})
			continue
		}
		standard, err := s.store.Plan(p.PlanID)
		if err != nil {
			standard = plan
		}
		if day.IsZero() {
			until = nextMorning(standard, now)
		} else {
			until = morningOf(standard, day)
		}
		msg := fmt.Sprintf("%s: %s bis %s Uhr", p.Name, plan.Name, until.Format("02.01. 15:04"))
		o := store.Override{Kind: store.KindPlan, PlanID: plan.ID, ProfileID: p.ID, StartsAt: now, Until: until, Minutes: int(until.Sub(now).Minutes()), Note: msg}
		if _, err := s.store.AddOverride(o); err != nil {
			redirectErr(w, r, back, err)
			return
		}
		_ = s.store.AddEvent(store.Event{TS: now, ProfileID: p.ID, Kind: "parent", Message: msg})
	}
	s.applyPlans(r)
	msg := fmt.Sprintf("%s: %s", strings.Join(names, " und "), plan.Name)
	if !until.IsZero() {
		msg += " bis " + until.Format("02.01. 15:04") + " Uhr"
	} else {
		msg += " ist jetzt der Standardplan"
	}
	redirect(w, r, back, msg)
}

// morningOf is when play may start on day according to plan, or midnight
// if the day has no time window.
func morningOf(plan store.Plan, day time.Time) time.Time {
	start := rules.StartOfDay(day)
	if w, ok := plan.Day(start.Weekday()).Window(); ok {
		return start.Add(time.Duration(w.Start) * time.Minute)
	}
	return start
}

// nextMorning is the next morning after now: today's, if it is still
// ahead (after midnight), otherwise tomorrow's.
func nextMorning(plan store.Plan, now time.Time) time.Time {
	if t := morningOf(plan, now); t.After(now) {
		return t
	}
	return morningOf(plan, rules.StartOfDay(now).AddDate(0, 0, 1))
}

func (s *Server) endOverride(w http.ResponseWriter, r *http.Request) {
	back := backTarget(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	now := s.engine.Now()
	if err := s.store.EndOverride(id, now); err != nil {
		redirectErr(w, r, back, errors.New("Ausnahme nicht gefunden oder schon abgelaufen"))
		return
	}
	_ = s.store.AddEvent(store.Event{TS: now, Kind: "parent", Message: "Ausnahme vorzeitig beendet"})
	s.applyPlans(r)
	redirect(w, r, back, "Ausnahme beendet")
}

// backTarget returns the local page a form wants to return to.
func backTarget(r *http.Request) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/"
}

// applyNow evaluates the rules right away so that parent decisions show
// without waiting for the next interval.
func (s *Server) applyNow(r *http.Request) {
	if err := s.engine.Tick(r.Context()); err != nil {
		s.log.Warn("immediate evaluation failed", "err", err)
	}
}

// applyPlans evaluates and writes changed plans to the consoles right away.
func (s *Server) applyPlans(r *http.Request) {
	s.applyNow(r)
	if s.opts.Nintendo != nil {
		s.opts.Nintendo.Apply(r.Context(), false)
	}
}

func (s *Server) profileName(id string) string {
	if p, err := s.store.Profile(id); err == nil {
		return p.Name
	}
	return id
}

func (s *Server) profileNames() map[string]string {
	profiles, _ := s.store.Profiles()
	out := make(map[string]string, len(profiles))
	for _, p := range profiles {
		out[p.ID] = p.Name
	}
	return out
}

// allPlans returns the stored plans and then Frei and Gesperrt.
func (s *Server) allPlans() []store.Plan {
	plans, _ := s.store.Plans()
	for _, id := range []string{store.PlanFree, store.PlanLocked} {
		if p, err := s.store.Plan(id); err == nil {
			plans = append(plans, p)
		}
	}
	return plans
}

// slugify turns a display name into a URL friendly id.
func slugify(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss").Replace(name)
	var b strings.Builder
	dash := false
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if out == "" {
		out = "profil"
	}
	return out
}

// --- events -------------------------------------------------------------

// eventFilters groups event kinds for the filter on the log page.
var eventFilters = map[string][]string{
	"zeit":   {"over"},
	"eltern": {"parent", "config", "login"},
}

type eventsData struct {
	Events []store.Event
	Filter string
}

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	filter := r.URL.Query().Get("art")
	kinds, ok := eventFilters[filter]
	if !ok {
		filter = ""
	}
	ev, err := s.store.Events(500, s.engine.Location(), kinds...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "protokoll", "Protokoll", eventsData{Events: ev, Filter: filter})
}

// --- history & stats ----------------------------------------------------

type historyData struct {
	Profiles []store.Profile
	Selected string
	Days     int
	Games    gamesView
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	profiles, _ := s.store.Profiles()
	sel := r.URL.Query().Get("profil")
	if sel == "" && len(profiles) > 0 {
		sel = profiles[0].ID
	}
	days := atoiDefault(r.URL.Query().Get("tage"), 14)
	today := s.engine.Now()
	data := historyData{Profiles: profiles, Selected: sel, Days: days, Games: s.nintendoGames(sel, today.AddDate(0, 0, 1-days), today)}
	s.render(w, r, "verlauf", "Verlauf", data)
}

type statsResponse struct {
	Profile  string         `json:"profile"`
	Limit    int            `json:"limit"`
	Daily    stats.Daily    `json:"daily"`
	Heatmap  [7][24]float64 `json:"heatmap"`
	Consoles []stats.Slice  `json:"consoles"`
}

func (s *Server) statsAPI(w http.ResponseWriter, r *http.Request) {
	now := s.engine.Now()
	profileID := r.URL.Query().Get("profile")
	days := min(max(atoiDefault(r.URL.Query().Get("days"), 14), 1), 366)
	limit := 0
	if p, err := s.store.Profile(profileID); err == nil {
		if plan, err := s.store.Plan(p.PlanID); err == nil {
			limit = max(plan.Day(now.Weekday()).Limit, 0)
		}
	}
	from := rules.StartOfDay(now).AddDate(0, 0, -(days - 1))
	all, err := s.store.NintendoPlayBetween(from.Format("2006-01-02"), now.Format("2006-01-02"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var plays []store.NintendoPlay
	for _, p := range all {
		if p.ProfileID == profileID {
			plays = append(plays, p)
		}
	}
	names := s.nintendoConsoleNames()
	writeJSON(w, statsResponse{
		Profile:  profileID,
		Limit:    limit,
		Daily:    stats.DailyMinutes(plays, from, days, names),
		Heatmap:  stats.Heatmap(plays, s.engine.Location(), float64(days)/7),
		Consoles: stats.Consoles(plays, names),
	})
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}

// --- kid view -----------------------------------------------------------

type kidData struct {
	View engine.ProfileView
	// Base is the address of this kid app: /k/<token> or /kind/<id>.
	Base   string
	Window string
	// Linked is true when Nintendo consoles are known, so there are
	// statistics to link to.
	Linked bool
	// StatsPage renders the statistics instead of the remaining time.
	StatsPage bool
	Stats     kidStats
	Ranges    []struct{ Value, Label string }
}

func (s *Server) kidView(id string) (engine.ProfileView, bool) {
	views, err := s.engine.Evaluate()
	if err != nil {
		return engine.ProfileView{}, false
	}
	for _, v := range views {
		if v.Profile.ID == id {
			return v, true
		}
	}
	return engine.ProfileView{}, false
}

func (s *Server) kidPage(w http.ResponseWriter, r *http.Request) {
	v, base, ok := s.kidTarget(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	consoles, _ := s.store.NintendoDevices()
	s.render(w, r, "kind", v.Profile.Name, kidData{View: v, Base: base, Window: kidWindow(v.Plan, s.engine.Now()), Linked: len(consoles) > 0})
}

// kidStatsPage shows a kid its own play time and games. It lives below the
// kid app so that it opens inside the installed app.
func (s *Server) kidStatsPage(w http.ResponseWriter, r *http.Request) {
	v, base, ok := s.kidTarget(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "kind", v.Profile.Name, kidData{
		View: v, Base: base, StatsPage: true, Ranges: kidRanges,
		Stats: s.kidStats(v.Profile, v.Plan, r.URL.Query().Get("zeitraum"), s.engine.Now(), base),
	})
}

// kidImage serves covers to the kid app, which has no parent session.
func (s *Server) kidImage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.kidTarget(r); !ok {
		http.NotFound(w, r)
		return
	}
	s.image(w, r)
}

func (s *Server) kidAPI(w http.ResponseWriter, r *http.Request) {
	v, _, ok := s.kidTarget(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	st := v.Status
	writeJSON(w, map[string]any{
		"name":      v.Profile.Name,
		"allowed":   st.Allowed,
		"reason":    st.Reason,
		"usedToday": int(st.UsedToday.Minutes()),
		"limit":     st.LimitToday,
		"remaining": int(st.Remaining.Minutes()),
	})
}

// kidManifest makes the kid app installable as an app of its own per kid
// and link.
func (s *Server) kidManifest(w http.ResponseWriter, r *http.Request) {
	v, base, ok := s.kidTarget(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := v.Profile.Name
	w.Header().Set("Content-Type", "application/manifest+json")
	writeJSONBody(w, map[string]any{
		"id":               base,
		"name":             "Zockerzeit " + name,
		"short_name":       "Zockerzeit",
		"description":      "Wie viel Zockerzeit " + name + " heute noch hat",
		"lang":             "de",
		"start_url":        base,
		"scope":            base,
		"display":          "standalone",
		"orientation":      "portrait",
		"background_color": "#17152b",
		"theme_color":      "#17152b",
		"icons":            appIcons,
	})
}

// appManifest makes the parent UI installable as an app. A device signed in
// with a link gets the link as start address, so the installed app signs
// itself in, even with its own cookie storage (iOS).
func (s *Server) appManifest(w http.ResponseWriter, r *http.Request) {
	start := "/"
	if token := r.URL.Query().Get("zugang"); token != "" {
		if l, err := s.store.AccessLinkByToken(token, s.engine.Now()); err == nil && l.Kind == store.LinkParent {
			start = "/zugang/" + token
		}
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	writeJSONBody(w, map[string]any{
		"id":               "/",
		"name":             "Zockerzeit für Eltern",
		"short_name":       "Zockerzeit",
		"description":      "Spielzeit der Kinder auf der Nintendo Switch",
		"lang":             "de",
		"start_url":        start,
		"scope":            "/",
		"display":          "standalone",
		"background_color": "#f3f2f8",
		"theme_color":      "#6440f0",
		"icons":            appIcons,
	})
}

var appIcons = []map[string]string{
	{"src": "/static/icons/icon-192.png", "sizes": "192x192", "type": "image/png"},
	{"src": "/static/icons/icon-512.png", "sizes": "512x512", "type": "image/png"},
	{"src": "/static/icons/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
}

// kidServiceWorker serves the service worker from /k/ and /kind/ so it may
// control all kid apps below.
func (s *Server) kidServiceWorker(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/kid-sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// --- settings -----------------------------------------------------------

type settingsData struct {
	Profiles         []store.Profile
	Access           accessData
	Nintendo         bool
	NintendoAccounts []store.NintendoAccount
	Notify           *notifyData
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.settingsWith(w, r, nil)
}

// settingsWith renders the settings, with a link that was just created.
func (s *Server) settingsWith(w http.ResponseWriter, r *http.Request, created *newLink) {
	data := settingsData{Nintendo: s.opts.Nintendo != nil, Notify: s.notifyData(), Access: s.accessData()}
	data.Access.New = created
	data.Profiles, _ = s.store.Profiles()
	if data.Nintendo {
		accounts, err := s.store.NintendoAccounts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.NintendoAccounts = accounts
	}
	s.render(w, r, "einstellungen", "Einstellungen", data)
}
