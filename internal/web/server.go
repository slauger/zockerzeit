// Package web serves the parent dashboard, the kid view and the JSON API
// used by the charts.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/engine"
	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const sessionCookie = "zockerzeit_session"

// Engine is what the web layer needs from the control loop.
type Engine interface {
	Tick(ctx context.Context) error
	Snapshot() engine.Snapshot
	Evaluate() ([]engine.ProfileView, error)
	Release(ctx context.Context, mac string) error
	Now() time.Time
	Location() *time.Location
	DryRun() bool
	InternetEnabled() bool
	Settings() engine.Settings
	SaveSettings(engine.Settings) error
}

// Nintendo connects Nintendo parental controls accounts. Optional.
type Nintendo interface {
	LoginURL() (string, string, error)
	Connect(ctx context.Context, label, redirect, verifier string) (int64, error)
	Remove(id int64) error
	SyncAll(ctx context.Context)
}

// Options configure the web server.
type Options struct {
	AdminPassword string
	SessionSecret string
	Version       string
	Nintendo      Nintendo
}

// Server is the HTTP handler.
type Server struct {
	store  *store.Store
	engine Engine
	opts   Options
	log    *slog.Logger
	secret []byte
	pages  map[string]*template.Template
	mux    *http.ServeMux

	loginMu sync.Mutex
	logins  map[string]pendingLogin
}

// pendingLogin keeps the PKCE verifier of a started Nintendo login.
type pendingLogin struct {
	url      string
	verifier string
	created  time.Time
}

// New creates the HTTP handler.
func New(st *store.Store, eng Engine, opts Options, log *slog.Logger) (*Server, error) {
	s := &Server{store: st, engine: eng, opts: opts, log: log, pages: map[string]*template.Template{}, logins: map[string]pendingLogin{}}
	if opts.SessionSecret != "" {
		s.secret = []byte(opts.SessionSecret)
	} else {
		s.secret = make([]byte, 32)
		if _, err := rand.Read(s.secret); err != nil {
			return nil, err
		}
	}
	for _, page := range []string{"dashboard", "verlauf", "regeln", "geraete", "geraet", "protokoll", "einstellungen", "nintendo", "login", "kind"} {
		t, err := template.New(page).Funcs(funcs(eng.Location())).ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	m.HandleFunc("GET /readyz", s.readyz)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.login)
	m.HandleFunc("POST /logout", s.logout)
	m.HandleFunc("GET /kind/{id}", s.kidPage)
	m.HandleFunc("GET /kind/{id}/manifest.webmanifest", s.kidManifest)
	m.HandleFunc("GET /kind/sw.js", s.kidServiceWorker)
	m.HandleFunc("GET /api/kind/{id}", s.kidAPI)

	m.Handle("GET /{$}", s.auth(s.dashboard))
	m.Handle("GET /verlauf", s.auth(s.history))
	m.Handle("GET /regeln", s.auth(s.rulesPage))
	m.Handle("POST /regeln", s.auth(s.saveProfile))
	m.Handle("POST /regeln/{id}/loeschen", s.auth(s.deleteProfile))
	m.Handle("GET /geraete", s.auth(s.devicesPage))
	m.Handle("POST /geraete", s.auth(s.saveDevice))
	m.Handle("POST /geraete/{mac}/loeschen", s.auth(s.deleteDevice))
	m.Handle("GET /geraet/{mac}", s.auth(s.deviceDetail))
	m.Handle("GET /protokoll", s.auth(s.eventsPage))
	m.Handle("GET /einstellungen", s.auth(s.settingsPage))
	m.Handle("GET /bild/{key}", s.auth(s.image))
	m.Handle("GET /einstellungen/nintendo", s.auth(s.nintendoPage))
	m.Handle("POST /einstellungen/nintendo/login", s.auth(s.nintendoLogin))
	m.Handle("POST /einstellungen/nintendo/verbinden", s.auth(s.nintendoConnect))
	m.Handle("POST /einstellungen/nintendo/sync", s.auth(s.nintendoSync))
	m.Handle("POST /einstellungen/nintendo/konsole", s.auth(s.nintendoMapDevice))
	m.Handle("POST /einstellungen/nintendo/spieler", s.auth(s.nintendoMapPlayer))
	m.Handle("POST /einstellungen/nintendo/{id}/entfernen", s.auth(s.nintendoRemove))
	m.Handle("POST /einstellungen", s.auth(s.saveSettings))
	m.Handle("POST /aktion", s.auth(s.action))
	m.Handle("POST /aktion/{id}/beenden", s.auth(s.endOverride))
	m.Handle("GET /api/status", s.auth(s.statusAPI))
	m.Handle("GET /api/stats", s.auth(s.statsAPI))
	s.mux = m
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Images only from Zockerzeit itself; covers and avatars are stored copies.
	w.Header().Set("Content-Security-Policy", "img-src 'self' data:")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// --- auth ---------------------------------------------------------------

func (s *Server) sign(expiry int64) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = fmt.Fprintf(mac, "admin|%d", expiry)
	return strconv.FormatInt(expiry, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) validSession(r *http.Request) bool {
	if s.opts.AdminPassword == "" {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	expiry, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.sign(expiry)))
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validSession(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", "Anmelden", map[string]any{"Failed": r.URL.Query().Has("fehler")})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	pw := r.FormValue("password")
	if s.opts.AdminPassword == "" || subtle.ConstantTimeCompare([]byte(pw), []byte(s.opts.AdminPassword)) != 1 {
		time.Sleep(time.Second)
		http.Redirect(w, r, "/login?fehler=1", http.StatusSeeOther)
		return
	}
	expiry := time.Now().Add(30 * 24 * time.Hour).Unix()
	// Secure is only set behind HTTPS so that plain HTTP on the LAN keeps working.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name: sessionCookie, Value: s.sign(expiry), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		Expires: time.Unix(expiry, 0),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- rendering ----------------------------------------------------------

type page struct {
	Title   string
	Nav     string
	DryRun  bool
	Auth    bool
	Version string
	Flash   string
	Error   string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	p := page{
		Title: title, Nav: name, DryRun: s.engine.DryRun(), Auth: s.opts.AdminPassword != "",
		Version: s.opts.Version, Flash: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("fehler"), Data: data,
	}
	if snap := s.engine.Snapshot(); snap.Error != "" && p.Error == "" && name != "kind" && name != "login" {
		p.Error = "Letzte Abfrage fehlgeschlagen: " + snap.Error
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		s.log.Error("render failed", "page", name, "err", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, to, flash string) {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	http.Redirect(w, r, to+sep+"ok="+url.QueryEscape(flash), http.StatusSeeOther)
}

func urlQuery(v string) string { return url.QueryEscape(v) }

func redirectErr(w http.ResponseWriter, r *http.Request, to string, err error) {
	http.Redirect(w, r, to+"?fehler="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	writeJSONBody(w, v)
}

// writeJSONBody encodes v without touching the content type.
func writeJSONBody(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func funcs(loc *time.Location) template.FuncMap {
	return template.FuncMap{
		"dur": formatDuration,
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.In(loc).Format("15:04")
		},
		"datetime": func(t time.Time) string { return t.In(loc).Format("02.01. 15:04") },
		"pct": func(used time.Duration, limit int) int {
			if limit <= 0 {
				return 0
			}
			p := int(math.Round(used.Minutes() / float64(limit) * 100))
			return min(p, 100)
		},
		"remaining": func(st rules.Status) string {
			if st.Remaining < 0 {
				return "unbegrenzt"
			}
			return formatDuration(st.Remaining)
		},
		"windows":     rules.FormatWindows,
		"cells":       energyCells,
		"slots":       slots,
		"deviceTypes": func() []string { return store.DeviceTypes },
		"list":        func(v ...int) []int { return v },
		"sub":         func(a, b time.Time) time.Duration { return a.Sub(b) },
		"dayPct": func(t time.Time) float64 {
			t = t.In(loc)
			return float64(t.Hour()*60+t.Minute()) / 1440 * 100
		},
		"spanPct": func(a, b time.Time) float64 {
			return max(b.Sub(a).Minutes()/1440*100, 0.4)
		},
		"eventLabel": func(kind string) string {
			if l, ok := eventLabels[kind]; ok {
				return l
			}
			return kind
		},
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"typeLabel": func(t string) string { return engine.TypeLabels[t] },
		"minutes":   func(m int) time.Duration { return time.Duration(m) * time.Minute },
		"initial": func(name string) string {
			for _, r := range name {
				return strings.ToUpper(string(r))
			}
			return "?"
		},
		"germanDate": func(t time.Time) string {
			t = t.In(loc)
			return fmt.Sprintf("%s, %d. %s", weekdayLong[t.Weekday()], t.Day(), monthLong[t.Month()-1])
		},
		"quotaLeft": func(st rules.Status) time.Duration {
			if st.LimitToday <= 0 {
				return 0
			}
			return max(time.Duration(st.LimitToday)*time.Minute-st.UsedToday, 0)
		},
		"weekday": func(d time.Weekday) string { return rules.WeekdayNames[d] },
		"kindLabel": func(k string) string {
			return map[string]string{
				store.KindLock: "Sperre", store.KindAllow: "Freigabe", store.KindBonus: "Bonus", store.KindRelease: "Gerätefreigabe",
			}[k]
		},
	}
}

var eventLabels = map[string]string{
	"block": "gesperrt", "unblock": "entsperrt", "dryrun": "Testmodus", "error": "Fehler",
	"online": "online", "offline": "offline", "roam": "AP-Wechsel", "parent": "Eltern", "config": "Einstellung",
}

var weekdayLong = map[time.Weekday]string{
	time.Monday: "Montag", time.Tuesday: "Dienstag", time.Wednesday: "Mittwoch", time.Thursday: "Donnerstag",
	time.Friday: "Freitag", time.Saturday: "Samstag", time.Sunday: "Sonntag",
}

var monthLong = []string{
	"Januar", "Februar", "März", "April", "Mai", "Juni",
	"Juli", "August", "September", "Oktober", "November", "Dezember",
}

// slot is a time window positioned on a 24 hour track, in percent.
type slot struct {
	Left  float64
	Width float64
	Label string
}

func slots(ws []rules.Window) []slot {
	out := make([]slot, 0, len(ws))
	for _, w := range ws {
		out = append(out, slot{
			Left:  float64(w.Start) / 1440 * 100,
			Width: float64(w.End-w.Start) / 1440 * 100,
			Label: rules.FormatClock(w.Start) + "-" + rules.FormatClock(w.End),
		})
	}
	return out
}

// cellMinutes is the play time one segment of the energy bar stands for.
const cellMinutes = 15

// energyCells splits today's budget into segments of cellMinutes. Each cell
// is "used", "left" (still playable), "low" (last segment while playing) or
// "paused" (budget left, but currently not allowed).
func energyCells(st rules.Status) []string {
	if st.LimitToday <= 0 {
		return nil
	}
	n := (st.LimitToday + cellMinutes - 1) / cellMinutes
	leftMin := st.LimitToday - int(st.UsedToday.Minutes())
	left := 0
	if leftMin > 0 {
		left = (leftMin + cellMinutes - 1) / cellMinutes
	}
	state := "left"
	switch {
	case !st.Allowed:
		state = "paused"
	case left <= 1:
		state = "low"
	}
	cells := make([]string, n)
	for i := range cells {
		if i < n-left {
			cells[i] = "used"
		} else {
			cells[i] = state
		}
	}
	return cells
}

// formatDuration renders a duration as "35 Min" or "1:05 Std".
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return fmt.Sprintf("%d Min", m)
	}
	return fmt.Sprintf("%d:%02d Std", m/60, m%60)
}
