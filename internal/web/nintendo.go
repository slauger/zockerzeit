package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slauger/zockerzeit/internal/store"
	"github.com/slauger/zockerzeit/internal/unifi"
)

type nintendoPlayerView struct {
	store.NintendoPlayer
	Today int
	Games []string
}

type nintendoDeviceView struct {
	store.NintendoDevice
	Today   int
	Players []nintendoPlayerView
}

type nintendoAccountView struct {
	store.NintendoAccount
	Devices []nintendoDeviceView
}

type nintendoData struct {
	Enabled  bool
	Accounts []nintendoAccountView
	Consoles []store.Device
	Profiles []store.Profile
	// LoginKey and LoginURL are set while a login is in progress.
	LoginKey string
	LoginURL string
}

func (s *Server) nintendoPage(w http.ResponseWriter, r *http.Request) {
	data := nintendoData{Enabled: s.opts.Nintendo != nil}
	if key := r.URL.Query().Get("login"); key != "" {
		s.loginMu.Lock()
		if p, ok := s.logins[key]; ok {
			data.LoginKey, data.LoginURL = key, p.url
		}
		s.loginMu.Unlock()
	}
	accounts, _ := s.store.NintendoAccounts()
	devices, _ := s.store.NintendoDevices()
	players, _ := s.store.NintendoPlayers()
	usage, _ := s.store.NintendoUsageOn(s.engine.Now().Format("2006-01-02"))
	titles, _ := s.store.NintendoTitles()
	images, _ := s.store.ImageKeys()
	data.Profiles, _ = s.store.Profiles()
	managed, _ := s.store.Devices()
	for _, d := range managed {
		if d.Type() == store.TypeConsole {
			data.Consoles = append(data.Consoles, d)
		}
	}
	for _, a := range accounts {
		av := nintendoAccountView{NintendoAccount: a}
		for _, d := range devices {
			if d.Account != a.ID {
				continue
			}
			dv := nintendoDeviceView{NintendoDevice: d}
			for _, p := range players {
				if p.DeviceID != d.DeviceID {
					continue
				}
				pv := nintendoPlayerView{NintendoPlayer: p}
				pv.ImageURI = localImage(images, p.ImageURI)
				for _, u := range usage {
					if u.DeviceID == d.DeviceID && u.PlayerID == p.PlayerID {
						pv.Today = u.Minutes
						for _, g := range u.Games {
							name := titles[strings.ToUpper(g.ApplicationID)].Name
							if name == "" {
								name = "Unbekanntes Spiel"
							}
							pv.Games = append(pv.Games, fmt.Sprintf("%s %s", name, formatDuration(time.Duration(g.Minutes)*time.Minute)))
						}
					}
				}
				dv.Today += pv.Today
				dv.Players = append(dv.Players, pv)
			}
			av.Devices = append(av.Devices, dv)
		}
		data.Accounts = append(data.Accounts, av)
	}
	s.render(w, r, "nintendo", "Nintendo", data)
}

func (s *Server) nintendoLogin(w http.ResponseWriter, r *http.Request) {
	if s.opts.Nintendo == nil {
		redirectErr(w, r, "/einstellungen/nintendo", errors.New("Nintendo-Anbindung ist nicht eingerichtet"))
		return
	}
	loginURL, verifier, err := s.opts.Nintendo.LoginURL()
	if err != nil {
		redirectErr(w, r, "/einstellungen/nintendo", err)
		return
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		redirectErr(w, r, "/einstellungen/nintendo", err)
		return
	}
	key := hex.EncodeToString(raw)
	s.loginMu.Lock()
	for k, p := range s.logins {
		if time.Since(p.created) > time.Hour {
			delete(s.logins, k)
		}
	}
	s.logins[key] = pendingLogin{url: loginURL, verifier: verifier, created: time.Now()}
	s.loginMu.Unlock()
	http.Redirect(w, r, "/einstellungen/nintendo?login="+key+"#verbinden", http.StatusSeeOther)
}

func (s *Server) nintendoConnect(w http.ResponseWriter, r *http.Request) {
	key := r.FormValue("key")
	s.loginMu.Lock()
	p, ok := s.logins[key]
	s.loginMu.Unlock()
	if !ok || s.opts.Nintendo == nil {
		redirectErr(w, r, "/einstellungen/nintendo", errors.New("Anmeldung abgelaufen, bitte neu starten"))
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if _, err := s.opts.Nintendo.Connect(r.Context(), label, r.FormValue("redirect"), p.verifier); err != nil {
		http.Redirect(w, r, "/einstellungen/nintendo?login="+key+"&fehler="+urlQuery(err.Error())+"#verbinden", http.StatusSeeOther)
		return
	}
	s.loginMu.Lock()
	delete(s.logins, key)
	s.loginMu.Unlock()
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), Kind: "config", Message: "Nintendo-Account " + label + " verbunden"})
	redirect(w, r, "/einstellungen/nintendo", "Nintendo-Account "+label+" verbunden")
}

func (s *Server) nintendoSync(w http.ResponseWriter, r *http.Request) {
	if s.opts.Nintendo != nil {
		s.opts.Nintendo.SyncAll(r.Context())
	}
	redirect(w, r, "/einstellungen/nintendo", "Daten von Nintendo abgerufen")
}

func (s *Server) nintendoRemove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.opts.Nintendo == nil {
		redirectErr(w, r, "/einstellungen/nintendo", errors.New("unbekannter Account"))
		return
	}
	if err := s.opts.Nintendo.Remove(id); err != nil {
		redirectErr(w, r, "/einstellungen/nintendo", err)
		return
	}
	redirect(w, r, "/einstellungen/nintendo", "Nintendo-Account entfernt")
}

func (s *Server) nintendoMapDevice(w http.ResponseWriter, r *http.Request) {
	mac := r.FormValue("mac")
	if mac != "" {
		mac = unifi.NormalizeMAC(mac)
	}
	if err := s.store.SetNintendoDeviceMAC(r.FormValue("device_id"), mac); err != nil {
		redirectErr(w, r, "/einstellungen/nintendo", err)
		return
	}
	redirect(w, r, "/einstellungen/nintendo", "Konsole zugeordnet")
}

func (s *Server) nintendoMapPlayer(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetNintendoPlayerProfile(r.FormValue("player_id"), r.FormValue("profile")); err != nil {
		redirectErr(w, r, "/einstellungen/nintendo", err)
		return
	}
	redirect(w, r, "/einstellungen/nintendo", "Spieler zugeordnet")
}

// nintendoToday is the play time a profile had today according to Nintendo.
type nintendoToday struct {
	Minutes int
	Parts   []nintendoPart
}

type nintendoPart struct {
	Console string
	Minutes int
}

// nintendoByProfile sums today's Nintendo play time of mapped players.
func (s *Server) nintendoByProfile() map[string]nintendoToday {
	out := map[string]nintendoToday{}
	usage, err := s.store.NintendoUsageOn(s.engine.Now().Format("2006-01-02"))
	if err != nil || len(usage) == 0 {
		return out
	}
	players, _ := s.store.NintendoPlayers()
	label := s.nintendoConsoleNames()
	profileOf := map[string]string{}
	for _, p := range players {
		profileOf[p.DeviceID+"/"+p.PlayerID] = p.ProfileID
	}
	for _, u := range usage {
		pid := profileOf[u.DeviceID+"/"+u.PlayerID]
		if pid == "" || u.Minutes == 0 {
			continue
		}
		t := out[pid]
		t.Minutes += u.Minutes
		t.Parts = append(t.Parts, nintendoPart{Console: label[u.DeviceID], Minutes: u.Minutes})
		out[pid] = t
	}
	for k, t := range out {
		sort.Slice(t.Parts, func(i, j int) bool { return t.Parts[i].Minutes > t.Parts[j].Minutes })
		out[k] = t
	}
	return out
}

// gamesView is what a profile played according to Nintendo in a period.
type gamesView struct {
	Linked  bool // at least one Nintendo user is mapped to the profile
	Minutes int
	Top     []gameRow
	Days    []gameDay
}

type gameRow struct {
	Name     string
	ImageURI string
	Minutes  int
	Percent  int // share of the most played game, for the bar width
	Consoles []string
}

type gameDay struct {
	Date    time.Time
	Minutes int
	Games   []gameChip
}

type gameChip struct {
	Name    string
	Console string
	Minutes int
}

// nintendoGames collects the games a profile played from..to (inclusive).
func (s *Server) nintendoGames(profileID string, from, to time.Time) gamesView {
	var v gamesView
	players, _ := s.store.NintendoPlayers()
	mine := map[string]bool{}
	for _, p := range players {
		if p.ProfileID == profileID {
			mine[p.DeviceID+"/"+p.PlayerID] = true
			v.Linked = true
		}
	}
	if !v.Linked {
		return v
	}
	usage, err := s.store.NintendoUsageBetween(from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return v
	}
	titles, _ := s.store.NintendoTitles()
	images, _ := s.store.ImageKeys()
	consoles := s.nintendoConsoleNames()
	name := func(id string) (string, string) {
		if t, ok := titles[strings.ToUpper(id)]; ok && t.Name != "" {
			return t.Name, localImage(images, t.ImageURI)
		}
		return "Unbekanntes Spiel", ""
	}

	top := map[string]*gameRow{}
	days := map[string]*gameDay{}
	for _, u := range usage {
		if !mine[u.DeviceID+"/"+u.PlayerID] || u.Minutes == 0 {
			continue
		}
		v.Minutes += u.Minutes
		day := days[u.Date]
		if day == nil {
			d, _ := time.ParseInLocation("2006-01-02", u.Date, s.engine.Location())
			day = &gameDay{Date: d}
			days[u.Date] = day
		}
		day.Minutes += u.Minutes
		console := consoles[u.DeviceID]
		for _, g := range u.Games {
			if g.Minutes == 0 {
				continue
			}
			n, img := name(g.ApplicationID)
			day.Games = append(day.Games, gameChip{Name: n, Console: console, Minutes: g.Minutes})
			key := strings.ToUpper(g.ApplicationID)
			row := top[key]
			if row == nil {
				row = &gameRow{Name: n, ImageURI: img}
				top[key] = row
			}
			row.Minutes += g.Minutes
			if !slices.Contains(row.Consoles, console) {
				row.Consoles = append(row.Consoles, console)
			}
		}
	}
	for _, r := range top {
		v.Top = append(v.Top, *r)
	}
	sort.Slice(v.Top, func(i, j int) bool {
		if v.Top[i].Minutes != v.Top[j].Minutes {
			return v.Top[i].Minutes > v.Top[j].Minutes
		}
		return v.Top[i].Name < v.Top[j].Name
	})
	for i := range v.Top {
		v.Top[i].Percent = max(v.Top[i].Minutes*100/v.Top[0].Minutes, 2)
	}
	for _, d := range days {
		sort.SliceStable(d.Games, func(i, j int) bool { return d.Games[i].Minutes > d.Games[j].Minutes })
		v.Days = append(v.Days, *d)
	}
	sort.Slice(v.Days, func(i, j int) bool { return v.Days[i].Date.After(v.Days[j].Date) })
	return v
}

// nintendoConsoleNames maps Nintendo device ids to the managed device name,
// or the Nintendo label when the console is not mapped.
func (s *Server) nintendoConsoleNames() map[string]string {
	devices, _ := s.store.NintendoDevices()
	managed, _ := s.store.Devices()
	out := map[string]string{}
	for _, d := range devices {
		out[d.DeviceID] = d.Label
		for _, m := range managed {
			if m.MAC == d.DeviceMAC {
				out[d.DeviceID] = m.Name
			}
		}
	}
	return out
}

// localImage returns the path of the stored copy of an external image, or ""
// when there is none yet. Pages never link to the original host.
func localImage(have map[string]bool, src string) string {
	if src == "" {
		return ""
	}
	key := store.ImageKey(src)
	if !have[key] {
		return ""
	}
	return "/bild/" + key
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	img, err := s.store.Image(r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", img.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=604800")
	_, _ = w.Write(img.Data)
}

// gameStats is one game on the games page.
type gameStats struct {
	Name       string
	ImageURI   string
	Minutes    int
	Percent    int
	LastPlayed time.Time
	Days       int
	PerKid     []kidMinutes
	Consoles   []string
}

type kidMinutes struct {
	Name    string
	Minutes int
}

type gamesData struct {
	Range   string // "7", "30" or "alle"
	Minutes int
	Played  []gameStats
	// Installed are known games without play time in the range.
	Installed []gameStats
}

// othersLabel groups Nintendo users that are not mapped to a kid.
const othersLabel = "Andere"

func (s *Server) gamesPage(w http.ResponseWriter, r *http.Request) {
	data := gamesData{Range: r.URL.Query().Get("zeitraum")}
	now := s.engine.Now()
	from := "0000-00-00"
	switch data.Range {
	case "7", "30":
		days, _ := strconv.Atoi(data.Range)
		from = now.AddDate(0, 0, 1-days).Format("2006-01-02")
	default:
		data.Range = "alle"
	}
	usage, err := s.store.NintendoUsageBetween(from, now.Format("2006-01-02"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	titles, _ := s.store.NintendoTitles()
	images, _ := s.store.ImageKeys()
	players, _ := s.store.NintendoPlayers()
	profiles, _ := s.store.Profiles()
	consoles := s.nintendoConsoleNames()
	profileName := map[string]string{}
	for _, p := range profiles {
		profileName[p.ID] = p.Name
	}
	kidOf := map[string]string{}
	for _, p := range players {
		name := profileName[p.ProfileID]
		if name == "" {
			name = othersLabel
		}
		kidOf[p.DeviceID+"/"+p.PlayerID] = name
	}

	type acc struct {
		gameStats
		kids map[string]int
		days map[string]bool
	}
	games := map[string]*acc{}
	get := func(id string) *acc {
		key := strings.ToUpper(id)
		g := games[key]
		if g == nil {
			t := titles[key]
			g = &acc{Name: t.Name, ImageURI: localImage(images, t.ImageURI), kids: map[string]int{}, days: map[string]bool{}}
			if g.Name == "" {
				g.Name = "Unbekanntes Spiel"
			}
			games[key] = g
		}
		return g
	}
	for _, u := range usage {
		kid := kidOf[u.DeviceID+"/"+u.PlayerID]
		if kid == "" {
			kid = othersLabel
		}
		day, _ := time.ParseInLocation("2006-01-02", u.Date, s.engine.Location())
		for _, gm := range u.Games {
			if gm.Minutes == 0 {
				continue
			}
			g := get(gm.ApplicationID)
			g.Minutes += gm.Minutes
			g.kids[kid] += gm.Minutes
			g.days[u.Date] = true
			if day.After(g.LastPlayed) {
				g.LastPlayed = day
			}
			if c := consoles[u.DeviceID]; !slices.Contains(g.Consoles, c) {
				g.Consoles = append(g.Consoles, c)
			}
		}
	}
	for key, t := range titles {
		if _, ok := games[key]; !ok && t.Name != "" {
			data.Installed = append(data.Installed, gameStats{Name: t.Name, ImageURI: localImage(images, t.ImageURI)})
		}
	}
	for _, g := range games {
		g.Days = len(g.days)
		for name, m := range g.kids {
			g.PerKid = append(g.PerKid, kidMinutes{Name: name, Minutes: m})
		}
		sort.Slice(g.PerKid, func(i, j int) bool { return g.PerKid[i].Minutes > g.PerKid[j].Minutes })
		sort.Strings(g.Consoles)
		data.Minutes += g.Minutes
		data.Played = append(data.Played, g.gameStats)
	}
	sort.Slice(data.Played, func(i, j int) bool {
		if data.Played[i].Minutes != data.Played[j].Minutes {
			return data.Played[i].Minutes > data.Played[j].Minutes
		}
		return data.Played[i].Name < data.Played[j].Name
	})
	for i := range data.Played {
		data.Played[i].Percent = max(data.Played[i].Minutes*100/data.Played[0].Minutes, 2)
	}
	sort.Slice(data.Installed, func(i, j int) bool { return data.Installed[i].Name < data.Installed[j].Name })
	s.render(w, r, "spiele", "Spiele", data)
}
