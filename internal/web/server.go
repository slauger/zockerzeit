// Package web serves the parent dashboard, the kid app and the JSON API used
// by the charts.
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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/engine"
	"github.com/slauger/zockerzeit/internal/notify"
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
	Consoles() ([]engine.Console, error)
	Now() time.Time
	Location() *time.Location
}

// Nintendo connects Nintendo parental controls accounts. Optional.
type Nintendo interface {
	LoginURL() (string, string, error)
	Connect(ctx context.Context, label, redirect, verifier string) (int64, error)
	Remove(id int64) error
	SyncAll(ctx context.Context)
}

// Notify manages notification channels and events. Optional.
type Notify interface {
	CanStore() bool
	HasStatic() bool
	Enabled(event string) bool
	SetEnabled(event string, on bool) error
	Add(kind, label string, c notify.ChannelConfig) error
	Test(ctx context.Context, id int64) error
}

// Options configure the web server.
type Options struct {
	AdminPassword string
	SessionSecret string
	Version       string
	Nintendo      Nintendo
	Notify        Notify
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
	limiter loginLimiter
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
	for _, page := range []string{"dashboard", "verlauf", "regeln", "konsolen", "protokoll", "einstellungen", "nintendo", "spiele", "login", "kind"} {
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
	m.HandleFunc("GET /manifest.webmanifest", s.appManifest)
	m.HandleFunc("GET /zugang/{token}", s.parentAccess)
	// Kid apps: public with a kid link, by name only for parents.
	m.HandleFunc("GET /k/{token}", s.kidPage)
	m.HandleFunc("GET /k/{token}/manifest.webmanifest", s.kidManifest)
	m.HandleFunc("GET /k/{token}/statistik", s.kidStatsPage)
	m.HandleFunc("GET /k/{token}/bild/{key}", s.kidImage)
	m.HandleFunc("GET /k/{token}/status", s.kidAPI)
	m.HandleFunc("GET /k/sw.js", s.kidServiceWorker)
	m.HandleFunc("GET /kind/sw.js", s.kidServiceWorker)
	m.Handle("GET /kind/{id}", s.auth(s.kidPage))
	m.Handle("GET /kind/{id}/manifest.webmanifest", s.auth(s.kidManifest))
	m.Handle("GET /kind/{id}/statistik", s.auth(s.kidStatsPage))
	m.Handle("GET /kind/{id}/bild/{key}", s.auth(s.kidImage))
	m.Handle("GET /api/kind/{id}", s.auth(s.kidAPI))

	m.Handle("GET /{$}", s.auth(s.dashboard))
	m.Handle("GET /verlauf", s.auth(s.history))
	m.Handle("GET /regeln", s.auth(s.rulesPage))
	m.Handle("POST /regeln", s.auth(s.saveProfile))
	m.Handle("POST /regeln/{id}/loeschen", s.auth(s.deleteProfile))
	m.Handle("GET /konsolen", s.auth(s.consolesPage))
	m.Handle("POST /konsolen/{id}", s.auth(s.saveConsole))
	m.Handle("GET /protokoll", s.auth(s.eventsPage))
	m.Handle("GET /einstellungen", s.auth(s.settingsPage))
	m.Handle("GET /bild/{key}", s.auth(s.image))
	m.Handle("GET /spiele", s.auth(s.gamesPage))
	m.Handle("GET /einstellungen/nintendo", s.auth(s.nintendoPage))
	m.Handle("POST /einstellungen/nintendo/login", s.auth(s.nintendoLogin))
	m.Handle("POST /einstellungen/nintendo/verbinden", s.auth(s.nintendoConnect))
	m.Handle("POST /einstellungen/nintendo/sync", s.auth(s.nintendoSync))
	m.Handle("POST /einstellungen/nintendo/{id}/entfernen", s.auth(s.nintendoRemove))
	m.Handle("POST /einstellungen/zugang", s.auth(s.addAccessLink))
	m.Handle("POST /einstellungen/zugang/{id}/entfernen", s.auth(s.removeAccessLink))
	m.Handle("POST /einstellungen/benachrichtigungen", s.auth(s.saveNotifyEvents))
	m.Handle("POST /einstellungen/kanal", s.auth(s.addNotifyChannel))
	m.Handle("POST /einstellungen/kanal/{id}/test", s.auth(s.testNotifyChannel))
	m.Handle("POST /einstellungen/kanal/{id}/entfernen", s.auth(s.removeNotifyChannel))
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
	if secureRequest(r) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
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
	if _, ok := s.parentLink(r); ok {
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
	q := r.URL.Query()
	s.render(w, r, "login", "Anmelden", map[string]any{"Failed": q.Has("fehler"), "Link": q.Has("link"), "Blocked": q.Has("gesperrt")})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.limiter.blocked(clientIP(r), time.Now()) {
		http.Redirect(w, r, "/login?gesperrt=1", http.StatusSeeOther)
		return
	}
	pw := r.FormValue("password")
	if s.opts.AdminPassword == "" || subtle.ConstantTimeCompare([]byte(pw), []byte(s.opts.AdminPassword)) != 1 {
		s.loginFailed(r, "Falsches Passwort")
		time.Sleep(time.Second)
		http.Redirect(w, r, "/login?fehler=1", http.StatusSeeOther)
		return
	}
	expiry := time.Now().Add(30 * 24 * time.Hour).Unix()
	// Secure is only set behind HTTPS so that plain HTTP on the LAN keeps working.
	// Lax, so that links from notifications open signed in; posts from other
	// sites are rejected by sameOrigin.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name: sessionCookie, Value: s.sign(expiry), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
		Expires: time.Unix(expiry, 0),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{sessionCookie, accessCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- rendering ----------------------------------------------------------

type page struct {
	Title string
	Nav   string
	// Manifest is the address of the parent app manifest for this device.
	Manifest string
	Auth     bool
	Version  string
	Flash    string
	Error    string
	Data     any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	p := page{
		Title: title, Nav: name, Auth: s.opts.AdminPassword != "", Manifest: "/manifest.webmanifest",
		Version: s.opts.Version, Flash: r.URL.Query().Get("ok"), Error: r.URL.Query().Get("fehler"), Data: data,
	}
	if token, ok := s.parentLink(r); ok {
		p.Manifest += "?zugang=" + token
	}
	if snap := s.engine.Snapshot(); snap.Error != "" && p.Error == "" && name != "kind" && name != "login" {
		p.Error = "Letzte Auswertung fehlgeschlagen: " + snap.Error
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		s.log.Error("render failed", "page", name, "err", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, to, flash string) {
	http.Redirect(w, r, withQuery(to, "ok", flash), http.StatusSeeOther)
}

// withQuery adds a query parameter to a local URL, before its fragment.
func withQuery(to, key, value string) string {
	path, frag, hasFrag := strings.Cut(to, "#")
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	out := path + sep + key + "=" + url.QueryEscape(value)
	if hasFrag {
		out += "#" + frag
	}
	return out
}

func urlQuery(v string) string { return url.QueryEscape(v) }

func redirectErr(w http.ResponseWriter, r *http.Request, to string, err error) {
	http.Redirect(w, r, withQuery(to, "fehler", err.Error()), http.StatusSeeOther)
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
		"daysAgo": func(t time.Time) string {
			days := int(rules.StartOfDay(time.Now().In(loc)).Sub(rules.StartOfDay(t.In(loc))).Hours()/24 + 0.5)
			switch {
			case days <= 0:
				return "heute"
			case days == 1:
				return "gestern"
			case days < 14:
				return fmt.Sprintf("vor %d Tagen", days)
			}
			return t.In(loc).Format("02.01.2006")
		},
		"remaining": func(st rules.Status) string {
			if st.Remaining < 0 {
				return "unbegrenzt"
			}
			return formatDuration(st.Remaining)
		},
		"windows": rules.FormatWindows,
		"cells":   energyCells,
		"slots":   slots,
		"list":    func(v ...int) []int { return v },
		"eventLabel": func(kind string) string {
			if l, ok := eventLabels[kind]; ok {
				return l
			}
			return kind
		},
		"minutes": func(m int) time.Duration { return time.Duration(m) * time.Minute },
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
		"weekday": func(d time.Weekday) string { return rules.WeekdayNames[d] },
	}
}

// eventLabels also names kinds that only older entries have.
var eventLabels = map[string]string{
	"over": "Zeit um", "parent": "Eltern", "config": "Einstellung", "error": "Fehler", "login": "Anmeldung",
	"block": "gesperrt", "unblock": "entsperrt", "dryrun": "Testmodus", "online": "online", "offline": "offline", "roam": "AP-Wechsel",
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
