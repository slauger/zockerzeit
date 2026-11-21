package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
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
	Games int
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
				for _, u := range usage {
					if u.DeviceID == d.DeviceID && u.PlayerID == p.PlayerID {
						pv.Today, pv.Games = u.Minutes, len(u.Games)
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
	devices, _ := s.store.NintendoDevices()
	managed, _ := s.store.Devices()
	label := map[string]string{}
	for _, d := range devices {
		label[d.DeviceID] = d.Label
		for _, m := range managed {
			if m.MAC == d.DeviceMAC {
				label[d.DeviceID] = m.Name
			}
		}
	}
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
