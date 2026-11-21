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
	Unowned []engine.Console
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
		if c.HeldBy == "" {
			data.Unowned = append(data.Unowned, c)
		}
	}
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
	profileID := r.FormValue("profile")
	minutes, _ := strconv.Atoi(r.FormValue("minutes"))
	tomorrow := rules.StartOfDay(now).AddDate(0, 0, 1)

	if r.FormValue("action") == "free" {
		s.free(w, r, now)
		return
	}
	o := store.Override{StartsAt: now, ProfileID: profileID, Minutes: minutes}
	var msg string
	switch r.FormValue("action") {
	case "bonus":
		o.Kind = store.KindBonus
		o.Until = tomorrow
		msg = fmt.Sprintf("%s bekommt heute %d Min extra", s.profileName(profileID), minutes)
	case "release":
		c, err := s.store.NintendoDevice(r.FormValue("device"))
		if err != nil {
			redirectErr(w, r, back, errors.New("bitte eine Konsole wählen"))
			return
		}
		o.Kind = store.KindRelease
		o.DeviceID = c.DeviceID
		o.Until = now.Add(time.Duration(minutes) * time.Minute)
		if minutes <= 0 || o.Until.After(tomorrow) {
			o.Until = tomorrow
			o.Minutes = int(tomorrow.Sub(now).Minutes())
		}
		msg = fmt.Sprintf("%s ist bis %s bei %s", c.Name, o.Until.Format("15:04"), s.profileName(profileID))
	default:
		redirectErr(w, r, back, errors.New("unbekannte Aktion"))
		return
	}
	if _, err := s.store.Profile(profileID); err != nil || o.Minutes == 0 || (o.Kind == store.KindRelease && o.Minutes < 0) {
		redirectErr(w, r, back, errors.New("bitte Kind und Minuten angeben"))
		return
	}
	o.Note = msg
	if _, err := s.store.AddOverride(o); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: now, ProfileID: o.ProfileID, DeviceID: o.DeviceID, Kind: "parent", Message: msg})
	s.applyNow(r)
	redirect(w, r, back, msg)
}

// free switches the rules of one kid or all kids off until the next
// morning, or including a date (bis=YYYY-MM-DD). A morning is the start of
// the first time window of a day; after midnight the next morning is today's.
func (s *Server) free(w http.ResponseWriter, r *http.Request, now time.Time) {
	back := backTarget(r)
	var day time.Time // the day whose morning ends the free time, zero for the next morning
	if v := r.FormValue("bis"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, s.engine.Location())
		if err != nil || d.Before(rules.StartOfDay(now)) || d.After(now.AddDate(1, 0, 0)) {
			redirectErr(w, r, back, errors.New("bitte ein Datum ab heute und höchstens ein Jahr voraus wählen"))
			return
		}
		day = d.AddDate(0, 0, 1)
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
	var names []string
	var until time.Time
	for _, p := range kids {
		if day.IsZero() {
			until = morningOf(p.Rules.Schedule, now)
			if !until.After(now) {
				until = morningOf(p.Rules.Schedule, rules.StartOfDay(now).AddDate(0, 0, 1))
			}
		} else {
			until = morningOf(p.Rules.Schedule, day)
		}
		msg := fmt.Sprintf("%s hat frei bis %s Uhr", p.Name, until.Format("02.01. 15:04"))
		o := store.Override{Kind: store.KindFree, ProfileID: p.ID, StartsAt: now, Until: until, Minutes: int(until.Sub(now).Minutes()), Note: msg}
		if _, err := s.store.AddOverride(o); err != nil {
			redirectErr(w, r, back, err)
			return
		}
		_ = s.store.AddEvent(store.Event{TS: now, ProfileID: p.ID, Kind: "parent", Message: msg})
		names = append(names, p.Name)
	}
	s.applyNow(r)
	redirect(w, r, back, fmt.Sprintf("%s: frei bis %s Uhr", strings.Join(names, " und "), until.Format("02.01. 15:04")))
}

// morningOf is when the first time window of day starts, or midnight if
// there is none.
func morningOf(s rules.Schedule, day time.Time) time.Time {
	start := rules.StartOfDay(day)
	ws := s[start.Weekday()]
	if len(ws) == 0 {
		return start
	}
	m := ws[0].Start
	for _, w := range ws {
		m = min(m, w.Start)
	}
	return start.Add(time.Duration(m) * time.Minute)
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
	s.applyNow(r)
	redirect(w, r, back, "Ausnahme beendet")
}

// backTarget returns the local page a form wants to return to.
func backTarget(r *http.Request) string {
	if b := r.FormValue("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/"
}

// applyNow runs a poll right away so that parent decisions take effect
// without waiting for the next interval.
func (s *Server) applyNow(r *http.Request) {
	if err := s.engine.Tick(r.Context()); err != nil {
		s.log.Warn("immediate poll failed", "err", err)
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

// --- rules --------------------------------------------------------------

type rulesData struct {
	Profiles []store.Profile
	Weekdays []time.Weekday
}

// newProfileTemplate is the starting point offered for a new profile.
func newProfileTemplate() store.Profile {
	weekday := []rules.Window{{Start: 15 * 60, End: 18*60 + 30}}
	weekend := []rules.Window{{Start: 10 * 60, End: 12 * 60}, {Start: 15 * 60, End: 19 * 60}}
	p := store.Profile{Rules: store.RuleSet{DailyMinutes: 60, Schedule: rules.Schedule{}}}
	for _, d := range rules.Weekdays {
		p.Rules.Schedule[d] = weekday
		if d == time.Saturday || d == time.Sunday {
			p.Rules.Schedule[d] = weekend
		}
	}
	return p
}

func (s *Server) rulesPage(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.store.Profiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	profiles = append(profiles, newProfileTemplate())
	s.render(w, r, "regeln", "Regeln", rulesData{Profiles: profiles, Weekdays: rules.Weekdays})
}

// parseRuleSet reads the limits and windows from the form.
func parseRuleSet(r *http.Request) (store.RuleSet, error) {
	rs := store.RuleSet{Schedule: rules.Schedule{}}
	var err error
	if rs.DailyMinutes, err = atoiEmpty(r.FormValue("daily")); err != nil || rs.DailyMinutes < 0 {
		return rs, errors.New("Tageslimit muss eine Zahl ab 0 sein")
	}
	for _, d := range rules.Weekdays {
		ws, err := rules.ParseWindows(r.FormValue("win_" + strconv.Itoa(int(d))))
		if err != nil {
			return rs, fmt.Errorf("%s: %w", rules.WeekdayNames[d], err)
		}
		if len(ws) > 0 {
			rs.Schedule[d] = ws
		}
	}
	return rs, nil
}

func atoiEmpty(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectErr(w, r, "/regeln", errors.New("Name fehlt"))
		return
	}
	id := r.FormValue("id")
	if id == "" {
		id = slugify(name)
		if _, err := s.store.Profile(id); err == nil {
			redirectErr(w, r, "/regeln", fmt.Errorf("Profil %q gibt es schon", name))
			return
		}
	}
	p := store.Profile{ID: id, Name: name}
	var err error
	if p.Rules, err = parseRuleSet(r); err != nil {
		redirectErr(w, r, "/regeln", err)
		return
	}
	if err := s.store.SaveProfile(p); err != nil {
		redirectErr(w, r, "/regeln", err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), ProfileID: p.ID, Kind: "config", Message: "Regeln für " + p.Name + " gespeichert"})
	s.applyNow(r)
	redirect(w, r, "/regeln", "Regeln für "+p.Name+" gespeichert")
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteProfile(id); err != nil {
		redirectErr(w, r, "/regeln", err)
		return
	}
	redirect(w, r, "/regeln", "Profil gelöscht")
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
		limit = p.Rules.DailyMinutes
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
	s.render(w, r, "kind", v.Profile.Name, kidData{View: v, Base: base, Window: kidWindow(v.Profile, s.engine.Now()), Linked: len(consoles) > 0})
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
		Stats: s.kidStats(v.Profile, r.URL.Query().Get("zeitraum"), s.engine.Now(), base),
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
