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
	Nintendo map[string]nintendoToday
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
		Nintendo: s.nintendoByProfile(),
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
	back := backTarget(r)
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
	case "lockdevice-next", "unlockdevice-next":
		// Until the device's schedule flips next, e.g. the end of the current
		// window or the start of the next one.
		o.ProfileID = s.deviceProfile(mac)
		o.DeviceMAC = mac
		o.Until = s.nextChange(mac, now)
		o.Minutes = int(o.Until.Sub(now).Minutes())
		if kind == "lockdevice-next" {
			o.Kind = store.KindLock
			msg = fmt.Sprintf("%s gesperrt bis %s", s.deviceName(mac), o.Until.Format("02.01. 15:04"))
		} else {
			o.Kind = store.KindRelease
			msg = fmt.Sprintf("%s freigegeben bis %s", s.deviceName(mac), o.Until.Format("02.01. 15:04"))
		}
	default:
		redirectErr(w, r, back, errors.New("unbekannte Aktion"))
		return
	}
	if o.ProfileID == "" || (o.Kind != store.KindLock && o.Kind != store.KindBonus && o.Minutes <= 0) || (o.Kind == store.KindBonus && o.Minutes == 0) {
		redirectErr(w, r, back, errors.New("bitte Profil und Minuten angeben"))
		return
	}
	if (kind == "release" || kind == "lockdevice") && mac == "" {
		redirectErr(w, r, back, errors.New("bitte ein Gerät wählen"))
		return
	}
	o.Note = msg
	if _, err := s.store.AddOverride(o); err != nil {
		redirectErr(w, r, back, err)
		return
	}
	_ = s.store.AddEvent(store.Event{TS: now, ProfileID: o.ProfileID, DeviceMAC: o.DeviceMAC, Kind: "parent", Message: msg})
	s.applyNow(r)
	redirect(w, r, back, msg)
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

// nextChange returns when the schedule of a device's owner flips next, or
// tomorrow morning if it never does.
func (s *Server) nextChange(mac string, now time.Time) time.Time {
	devs, _ := s.store.Devices()
	for _, d := range devs {
		if d.MAC != mac {
			continue
		}
		p, err := s.store.Profile(d.ProfileID)
		if err != nil {
			break
		}
		if t, ok := p.Rules(d.Type()).Schedule.NextChange(now); ok {
			return t
		}
	}
	return rules.StartOfDay(now).AddDate(0, 0, 1)
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

type deviceTypeInfo struct {
	Key   string
	Label string
	Hint  string
}

var deviceTypeInfos = []deviceTypeInfo{
	{Key: store.TypeConsole, Label: "Konsole", Hint: "Zählt, solange die Konsole im WLAN ist."},
	{Key: store.TypePhone, Label: "Handy", Hint: "Zählt nur bei Datenverkehr. Anrufe über das Mobilfunknetz gehen immer."},
}

type rulesData struct {
	Profiles []store.Profile
	Types    []deviceTypeInfo
	Weekdays []time.Weekday
	// HasPhone tells which profiles have a phone assigned.
	HasPhone map[string]bool
}

// newProfileTemplate is the starting point offered for a new profile.
func newProfileTemplate() store.Profile {
	weekday := []rules.Window{{Start: 15 * 60, End: 18*60 + 30}}
	weekend := []rules.Window{{Start: 10 * 60, End: 12 * 60}, {Start: 15 * 60, End: 19 * 60}}
	phone := []rules.Window{{Start: 7 * 60, End: 20*60 + 30}}
	p := store.Profile{
		Console: store.RuleSet{DailyMinutes: 90, WeeklyMinutes: 480, Schedule: rules.Schedule{}},
		Phone:   store.RuleSet{Schedule: rules.Schedule{}},
	}
	for _, d := range rules.Weekdays {
		p.Console.Schedule[d] = weekday
		if d == time.Saturday || d == time.Sunday {
			p.Console.Schedule[d] = weekend
		}
		p.Phone.Schedule[d] = phone
	}
	return p
}

func (s *Server) rulesPage(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.store.Profiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hasPhone := map[string]bool{}
	devices, _ := s.store.Devices()
	for _, d := range devices {
		if d.Type() == store.TypePhone {
			hasPhone[d.ProfileID] = true
		}
	}
	profiles = append(profiles, newProfileTemplate())
	s.render(w, r, "regeln", "Regeln", rulesData{Profiles: profiles, Types: deviceTypeInfos, Weekdays: rules.Weekdays, HasPhone: hasPhone})
}

// parseRuleSet reads the limits and windows of one device type from the form.
func parseRuleSet(r *http.Request, deviceType string) (store.RuleSet, error) {
	label := engine.TypeLabels[deviceType]
	rs := store.RuleSet{Schedule: rules.Schedule{}}
	var err error
	if rs.DailyMinutes, err = atoiEmpty(r.FormValue(deviceType + "_daily")); err != nil || rs.DailyMinutes < 0 {
		return rs, fmt.Errorf("%s: Tageslimit muss eine Zahl ab 0 sein", label)
	}
	if rs.WeeklyMinutes, err = atoiEmpty(r.FormValue(deviceType + "_weekly")); err != nil || rs.WeeklyMinutes < 0 {
		return rs, fmt.Errorf("%s: Wochenlimit muss eine Zahl ab 0 sein", label)
	}
	for _, d := range rules.Weekdays {
		ws, err := rules.ParseWindows(r.FormValue(deviceType + "_win_" + strconv.Itoa(int(d))))
		if err != nil {
			return rs, fmt.Errorf("%s, %s: %w", label, rules.WeekdayNames[d], err)
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
	if p.Console, err = parseRuleSet(r, store.TypeConsole); err != nil {
		redirectErr(w, r, "/regeln", err)
		return
	}
	if p.Phone, err = parseRuleSet(r, store.TypePhone); err != nil {
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

type deviceGroup struct {
	Index   int
	Profile store.Profile
	Devices []store.Device
}

type seenDevice struct {
	engine.SeenClient
	Type string
}

type devicesData struct {
	Groups   []deviceGroup
	Profiles []store.Profile
	Seen     []seenDevice
	States   map[string]engine.DeviceState
	// Internet is true when devices can be blocked via the FRITZ!Box.
	Internet bool
	// DefaultBlocker is preselected for devices found in the WLAN.
	DefaultBlocker string
}

// guessType suggests a device type from the hostname reported by UniFi.
func guessType(name string) string {
	n := strings.ToLower(name)
	for _, hint := range []string{"iphone", "ipad", "android", "pixel", "galaxy", "phone", "xiaomi", "redmi"} {
		if strings.Contains(n, hint) {
			return store.TypePhone
		}
	}
	return store.TypeConsole
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
	var seen []seenDevice
	for _, c := range snap.Seen {
		if !c.Managed {
			seen = append(seen, seenDevice{SeenClient: c, Type: guessType(c.Name)})
		}
	}
	groups := make([]deviceGroup, 0, len(profiles))
	for i, p := range profiles {
		g := deviceGroup{Index: i, Profile: p}
		for _, d := range devices {
			if d.ProfileID == p.ID {
				g.Devices = append(g.Devices, d)
			}
		}
		groups = append(groups, g)
	}
	s.render(w, r, "geraete", "Geräte", devicesData{Groups: groups, Profiles: profiles, Seen: seen, States: states, Internet: s.engine.InternetEnabled(), DefaultBlocker: s.engine.Settings().DefaultBlocker})
}

func (s *Server) saveDevice(w http.ResponseWriter, r *http.Request) {
	d := store.Device{
		MAC:       unifi.NormalizeMAC(r.FormValue("mac")),
		Name:      strings.TrimSpace(r.FormValue("name")),
		ProfileID: r.FormValue("profile"),
		Shared:    r.FormValue("shared") == "on",
		Mode:      modeForType(r.FormValue("type"), r.FormValue("mode")),
		Blocker:   r.FormValue("blocker"),

		ScheduleBlock: r.FormValue("schedule_block") == "on",
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
	// Switching the way of blocking: lift a block set the old way first,
	// otherwise it would stay forever.
	devs, _ := s.store.Devices()
	for _, old := range devs {
		if old.MAC == d.MAC && old.Blocker != d.Blocker && (d.Blocker == store.BlockInternet || old.Blocker == store.BlockInternet) {
			if err := s.engine.Release(r.Context(), d.MAC); err != nil {
				s.log.Warn("lifting the previous block failed", "mac", d.MAC, "err", err)
			}
		}
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

// eventFilters groups event kinds for the filter on the log page.
var eventFilters = map[string][]string{
	"sperren": {"block", "unblock", "dryrun", "error"},
	"online":  {"online", "offline", "roam"},
	"eltern":  {"parent", "config"},
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
	Date     string
	Type     string
	Games    gamesView
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
	t := r.URL.Query().Get("typ")
	if t != store.TypePhone && t != "alle" {
		t = store.TypeConsole
	}
	data := historyData{Profiles: profiles, Selected: sel, Days: days, Date: date, Type: t}
	if t != store.TypePhone {
		today := s.engine.Now().In(s.engine.Location())
		data.Games = s.nintendoGames(sel, today.AddDate(0, 0, 1-days), today)
	}
	s.render(w, r, "verlauf", "Verlauf", data)
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
	// Without a type filter console and phone time are mixed, so there is no
	// single limit to draw. Console limits apply to Nintendo play time, not
	// to the WLAN time shown here.
	deviceType := r.URL.Query().Get("type")
	limit := 0
	if p, err := s.store.Profile(profileID); err == nil && deviceType == store.TypePhone {
		limit = p.Rules(deviceType).DailyMinutes
	}

	from := rules.StartOfDay(now).AddDate(0, 0, -(days - 1))
	samples, err := s.store.Samples(profileID, from, now.Add(time.Minute), loc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	samples = filterType(samples, deviceType)

	day, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("day"), loc)
	if err != nil {
		day = rules.StartOfDay(now)
	}
	daySamples, err := s.store.Samples(profileID, day, day.AddDate(0, 0, 1), loc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	daySamples = filterType(daySamples, deviceType)

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
		"name":    v.Profile.Name,
		"console": kidStatus(v.Console),
		"phone":   kidStatus(v.Phone),
	})
}

func kidStatus(st rules.Status) map[string]any {
	return map[string]any{
		"allowed":   st.Allowed,
		"reason":    st.Reason,
		"usedToday": int(st.UsedToday.Minutes()),
		"limit":     st.LimitToday,
		"remaining": int(st.Remaining.Minutes()),
	}
}

// modeForType maps the device type chosen in the UI to its tracking mode.
func modeForType(deviceType, mode string) string {
	switch deviceType {
	case store.TypePhone:
		return store.ModeTraffic
	case store.TypeConsole:
		return store.ModeOnline
	}
	return mode
}

// --- device detail -----------------------------------------------------

type deviceDetailData struct {
	Device    store.Device
	State     engine.DeviceState
	Owner     store.Profile
	Others    []store.Profile
	Overrides []store.Override
	Sessions  []stats.Session
	Today     time.Duration
	// NextChange is when the schedule of the device flips next.
	NextChange time.Time
	DryRun     bool
}

func (s *Server) deviceDetail(w http.ResponseWriter, r *http.Request) {
	mac := unifi.NormalizeMAC(r.PathValue("mac"))
	devices, err := s.store.Devices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var data deviceDetailData
	found := false
	for _, d := range devices {
		if d.MAC == mac {
			data.Device, found = d, true
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	profiles, _ := s.store.Profiles()
	for _, p := range profiles {
		if p.ID == data.Device.ProfileID {
			data.Owner = p
		} else {
			data.Others = append(data.Others, p)
		}
	}
	for _, v := range s.engine.Snapshot().Profiles {
		for _, ds := range v.Devices {
			if ds.Device.MAC == mac {
				data.State = ds
			}
		}
	}
	now := s.engine.Now()
	ovs, _ := s.store.ActiveOverrides(now, s.engine.Location())
	for _, o := range ovs {
		if o.DeviceMAC == mac {
			data.Overrides = append(data.Overrides, o)
		}
	}
	samples, err := s.store.Samples("", rules.StartOfDay(now), now.Add(time.Minute), s.engine.Location())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var mine []store.Sample
	for _, sm := range samples {
		if sm.MAC == mac {
			mine = append(mine, sm)
			if sm.Active {
				data.Today += time.Duration(sm.Seconds) * time.Second
			}
		}
	}
	data.Sessions = stats.Sessions(mine, map[string]string{mac: data.Device.Name}, 3*time.Minute)
	data.NextChange = s.nextChange(mac, now)
	data.DryRun = s.engine.DryRun()
	s.render(w, r, "geraet", data.Device.Name, data)
}

// filterType keeps the samples of one device type; any other value keeps all.
func filterType(samples []store.Sample, deviceType string) []store.Sample {
	if deviceType != store.TypeConsole && deviceType != store.TypePhone {
		return samples
	}
	out := samples[:0]
	for _, sm := range samples {
		if sm.Type == deviceType {
			out = append(out, sm)
		}
	}
	return out
}

// kidManifest makes the kid view installable as an app of its own per kid.
func (s *Server) kidManifest(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Profile(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	start := "/kind/" + p.ID
	w.Header().Set("Content-Type", "application/manifest+json")
	writeJSONBody(w, map[string]any{
		"id":               start,
		"name":             "Zockerzeit " + p.Name,
		"short_name":       "Zockerzeit",
		"description":      "Wie viel Zockerzeit " + p.Name + " heute noch hat",
		"lang":             "de",
		"start_url":        start,
		"scope":            start,
		"display":          "standalone",
		"orientation":      "portrait",
		"background_color": "#17152b",
		"theme_color":      "#17152b",
		"icons": []map[string]string{
			{"src": "/static/icons/icon-192.png", "sizes": "192x192", "type": "image/png"},
			{"src": "/static/icons/icon-512.png", "sizes": "512x512", "type": "image/png"},
			{"src": "/static/icons/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
		},
	})
}

// kidServiceWorker serves the service worker from /kind/ so it may control
// all kid apps.
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
	Settings         engine.Settings
	Internet         bool
	Nintendo         bool
	NintendoAccounts []store.NintendoAccount
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	data := settingsData{Settings: s.engine.Settings(), Internet: s.engine.InternetEnabled(), Nintendo: s.opts.Nintendo != nil}
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

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	set := engine.Settings{
		DryRun:           r.FormValue("dry_run") == "on",
		WLANBlocking:     r.FormValue("wlan_blocking") == "on",
		InternetBlocking: r.FormValue("internet_blocking") == "on",
		DefaultBlocker:   r.FormValue("default_blocker"),
	}
	before := s.engine.Settings()
	if err := s.engine.SaveSettings(set); err != nil {
		redirectErr(w, r, "/einstellungen", err)
		return
	}
	msg := "Einstellungen gespeichert"
	switch {
	case before.DryRun && !set.DryRun:
		msg = "Testmodus aus: Zockerzeit sperrt und entsperrt jetzt selbst"
	case !before.DryRun && set.DryRun:
		msg = "Testmodus an: Zockerzeit simuliert nur noch"
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), Kind: "config", Message: msg})
	s.applyNow(r)
	redirect(w, r, "/einstellungen", msg)
}
