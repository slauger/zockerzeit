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
	"github.com/slauger/zockerzeit/internal/unifi"
)

// --- dashboard ----------------------------------------------------------

type dashboardData struct {
	Views    []engine.ProfileView
	Profiles []store.Profile
	Devices  []store.Device
	Shared   []store.Device
	PolledAt time.Time
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	views, err := s.engine.Evaluate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	profiles, _ := s.store.Profiles()
	devices, _ := s.store.Devices()
	var shared []store.Device
	for _, d := range devices {
		if d.Shared {
			shared = append(shared, d)
		}
	}
	s.render(w, r, "dashboard", "Übersicht", dashboardData{
		Views: views, Profiles: profiles, Devices: devices, Shared: shared, PolledAt: s.engine.Snapshot().At,
	})
}

func (s *Server) statusAPI(w http.ResponseWriter, _ *http.Request) {
	views, err := s.engine.Evaluate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	snap := s.engine.Snapshot()
	writeJSON(w, map[string]any{"at": snap.At, "dryRun": snap.DryRun, "error": snap.Error, "profiles": views})
}

// --- parent actions -----------------------------------------------------

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	now := s.engine.Now()
	kind := r.FormValue("action")
	profileID := r.FormValue("profile")
	mac := r.FormValue("device")
	minutes, _ := strconv.Atoi(r.FormValue("minutes"))
	tomorrow := rules.StartOfDay(now).AddDate(0, 0, 1)

	o := store.Override{StartsAt: now, ProfileID: profileID, Minutes: minutes}
	var msg string
	switch kind {
	case "lock":
		o.Kind = store.KindLock
		o.Until = now.Add(time.Duration(minutes) * time.Minute)
		if minutes <= 0 {
			o.Until = tomorrow
		}
		msg = fmt.Sprintf("%s gesperrt bis %s", s.profileName(profileID), o.Until.Format("02.01. 15:04"))
	case "allow":
		o.Kind = store.KindAllow
		o.Until = now.Add(time.Duration(minutes) * time.Minute)
		msg = fmt.Sprintf("%s freigegeben für %d Min", s.profileName(profileID), minutes)
	case "bonus":
		o.Kind = store.KindBonus
		o.Until = tomorrow
		msg = fmt.Sprintf("%s bekommt heute %d Min extra", s.profileName(profileID), minutes)
	case "release":
		o.Kind = store.KindRelease
		o.DeviceMAC = mac
		o.Until = now.Add(time.Duration(minutes) * time.Minute)
		msg = fmt.Sprintf("%s für %s freigegeben (%d Min)", s.deviceName(mac), s.profileName(profileID), minutes)
	case "lockdevice":
		o.Kind = store.KindLock
		o.DeviceMAC = mac
		o.ProfileID = s.deviceProfile(mac)
		o.Until = now.Add(time.Duration(minutes) * time.Minute)
		if minutes <= 0 {
			o.Until = tomorrow
		}
		msg = fmt.Sprintf("%s gesperrt bis %s", s.deviceName(mac), o.Until.Format("02.01. 15:04"))
	default:
		redirectErr(w, r, "/", errors.New("unbekannte Aktion"))
		return
	}
	if o.ProfileID == "" || (o.Kind != store.KindLock && o.Kind != store.KindBonus && minutes <= 0) || (o.Kind == store.KindBonus && minutes == 0) {
		redirectErr(w, r, "/", errors.New("bitte Profil und Minuten angeben"))
		return
	}
	if (kind == "release" || kind == "lockdevice") && mac == "" {
		redirectErr(w, r, "/", errors.New("bitte ein Gerät wählen"))
		return
	}
	o.Note = msg
	if _, err := s.store.AddOverride(o); err != nil {
		redirectErr(w, r, "/", err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: now, ProfileID: o.ProfileID, DeviceMAC: o.DeviceMAC, Kind: "parent", Message: msg})
	s.applyNow(r)
	redirect(w, r, "/", msg)
}

func (s *Server) endOverride(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		redirectErr(w, r, "/", err)
		return
	}
	now := s.engine.Now()
	if err := s.store.EndOverride(id, now); err != nil {
		redirectErr(w, r, "/", errors.New("Ausnahme nicht gefunden oder schon abgelaufen"))
		return
	}
	_ = s.store.AddEvent(store.Event{TS: now, Kind: "parent", Message: "Ausnahme vorzeitig beendet"})
	s.applyNow(r)
	redirect(w, r, "/", "Ausnahme beendet")
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

func (s *Server) deviceName(mac string) string {
	devs, _ := s.store.Devices()
	for _, d := range devs {
		if d.MAC == mac {
			return d.Name
		}
	}
	return mac
}

func (s *Server) deviceProfile(mac string) string {
	devs, _ := s.store.Devices()
	for _, d := range devs {
		if d.MAC == mac {
			return d.ProfileID
		}
	}
	return ""
}

// --- rules --------------------------------------------------------------

type rulesData struct {
	Profiles []store.Profile
	Weekdays []time.Weekday
}

func (s *Server) rulesPage(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.store.Profiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "regeln", "Regeln", rulesData{Profiles: profiles, Weekdays: rules.Weekdays})
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
	p := store.Profile{ID: id, Name: name, Schedule: rules.Schedule{}}
	p.DailyMinutes, _ = strconv.Atoi(r.FormValue("daily"))
	p.WeeklyMinutes, _ = strconv.Atoi(r.FormValue("weekly"))
	if p.DailyMinutes < 0 || p.WeeklyMinutes < 0 {
		redirectErr(w, r, "/regeln", errors.New("Minuten dürfen nicht negativ sein"))
		return
	}
	for _, d := range rules.Weekdays {
		ws, err := rules.ParseWindows(r.FormValue("win_" + strconv.Itoa(int(d))))
		if err != nil {
			redirectErr(w, r, "/regeln", fmt.Errorf("%s: %w", rules.WeekdayNames[d], err))
			return
		}
		if len(ws) > 0 {
			p.Schedule[d] = ws
		}
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
	devs, _ := s.store.Devices()
	for _, d := range devs {
		if d.ProfileID == id {
			if err := s.engine.Release(r.Context(), d.MAC); err != nil {
				s.log.Warn("unblocking device failed", "mac", d.MAC, "err", err)
			}
		}
	}
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

// --- devices ------------------------------------------------------------

type devicesData struct {
	Devices  []store.Device
	Profiles []store.Profile
	Seen     []engine.SeenClient
	States   map[string]engine.DeviceState
}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.Devices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	profiles, _ := s.store.Profiles()
	snap := s.engine.Snapshot()
	states := map[string]engine.DeviceState{}
	for _, v := range snap.Profiles {
		for _, d := range v.Devices {
			states[d.Device.MAC] = d
		}
	}
	var seen []engine.SeenClient
	for _, c := range snap.Seen {
		if !c.Managed {
			seen = append(seen, c)
		}
	}
	s.render(w, r, "geraete", "Geräte", devicesData{Devices: devices, Profiles: profiles, Seen: seen, States: states})
}

func (s *Server) saveDevice(w http.ResponseWriter, r *http.Request) {
	d := store.Device{
		MAC:       unifi.NormalizeMAC(r.FormValue("mac")),
		Name:      strings.TrimSpace(r.FormValue("name")),
		ProfileID: r.FormValue("profile"),
		Shared:    r.FormValue("shared") == "on",
		Mode:      r.FormValue("mode"),
	}
	if !validMAC(d.MAC) {
		redirectErr(w, r, "/geraete", fmt.Errorf("ungültige MAC-Adresse %q", d.MAC))
		return
	}
	if d.Name == "" {
		d.Name = d.MAC
	}
	if _, err := s.store.Profile(d.ProfileID); err != nil {
		redirectErr(w, r, "/geraete", errors.New("bitte zuerst ein Profil anlegen und auswählen"))
		return
	}
	if err := s.store.SaveDevice(d); err != nil {
		redirectErr(w, r, "/geraete", err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), ProfileID: d.ProfileID, DeviceMAC: d.MAC, Kind: "config", Message: d.Name + " gespeichert"})
	s.applyNow(r)
	redirect(w, r, "/geraete", d.Name+" gespeichert")
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	mac := unifi.NormalizeMAC(r.PathValue("mac"))
	if err := s.store.DeleteDevice(mac); err != nil {
		redirectErr(w, r, "/geraete", err)
		return
	}
	if err := s.engine.Release(r.Context(), mac); err != nil {
		s.log.Warn("unblocking device failed", "mac", mac, "err", err)
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), DeviceMAC: mac, Kind: "config", Message: mac + " nicht mehr verwaltet"})
	redirect(w, r, "/geraete", "Gerät entfernt und entsperrt")
}

func validMAC(mac string) bool {
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return false
	}
	for _, p := range parts {
		if len(p) != 2 {
			return false
		}
		if _, err := strconv.ParseUint(p, 16, 8); err != nil {
			return false
		}
	}
	return true
}

// --- events -------------------------------------------------------------

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	ev, err := s.store.Events(300, s.engine.Location())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "protokoll", "Protokoll", ev)
}

// --- history & stats ----------------------------------------------------

type historyData struct {
	Profiles []store.Profile
	Selected string
	Days     int
	Date     string
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	profiles, _ := s.store.Profiles()
	sel := r.URL.Query().Get("profil")
	if sel == "" && len(profiles) > 0 {
		sel = profiles[0].ID
	}
	days := atoiDefault(r.URL.Query().Get("tage"), 14)
	date := r.URL.Query().Get("tag")
	if date == "" {
		date = s.engine.Now().Format("2006-01-02")
	}
	s.render(w, r, "verlauf", "Verlauf", historyData{Profiles: profiles, Selected: sel, Days: days, Date: date})
}

type statsResponse struct {
	Profile   string          `json:"profile"`
	Limit     int             `json:"limit"`
	Daily     stats.Daily     `json:"daily"`
	Heatmap   [7][24]float64  `json:"heatmap"`
	Locations []stats.Slice   `json:"locations"`
	Devices   []stats.Slice   `json:"devices"`
	Sessions  []stats.Session `json:"sessions"`
	Day       string          `json:"day"`
}

func (s *Server) statsAPI(w http.ResponseWriter, r *http.Request) {
	loc := s.engine.Location()
	now := s.engine.Now()
	profileID := r.URL.Query().Get("profile")
	days := min(max(atoiDefault(r.URL.Query().Get("days"), 14), 1), 366)

	names := map[string]string{}
	devs, _ := s.store.Devices()
	for _, d := range devs {
		names[d.MAC] = d.Name
	}
	limit := 0
	if p, err := s.store.Profile(profileID); err == nil {
		limit = p.DailyMinutes
	}

	from := rules.StartOfDay(now).AddDate(0, 0, -(days - 1))
	samples, err := s.store.Samples(profileID, from, now.Add(time.Minute), loc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	day, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("day"), loc)
	if err != nil {
		day = rules.StartOfDay(now)
	}
	daySamples, err := s.store.Samples(profileID, day, day.AddDate(0, 0, 1), loc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, statsResponse{
		Profile:   profileID,
		Limit:     limit,
		Daily:     stats.DailyMinutes(samples, from, days, names),
		Heatmap:   stats.Heatmap(samples, float64(days)/7),
		Locations: stats.Locations(samples),
		Devices:   stats.Devices(samples, names),
		Sessions:  stats.Sessions(daySamples, names, 3*time.Minute),
		Day:       day.Format("2006-01-02"),
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
	v, ok := s.kidView(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "kind", v.Profile.Name, kidData{View: v})
}

func (s *Server) kidAPI(w http.ResponseWriter, r *http.Request) {
	v, ok := s.kidView(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]any{
		"name":      v.Profile.Name,
		"allowed":   v.Status.Allowed,
		"reason":    v.Status.Reason,
		"usedToday": int(v.Status.UsedToday.Minutes()),
		"limit":     v.Status.LimitToday,
		"remaining": int(v.Status.Remaining.Minutes()),
	})
}
