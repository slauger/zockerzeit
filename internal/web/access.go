package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slauger/zockerzeit/internal/engine"
	"github.com/slauger/zockerzeit/internal/store"
)

// accessCookie holds the token of a parent device link. With it the device
// stays signed in without a password, until the link is removed.
const accessCookie = "zockerzeit_zugang"

// linkCookieAge is the longest lifetime browsers accept for a cookie. The
// installed app starts with its link and so renews it on every start.
const linkCookieAge = 400 * 24 * time.Hour

// newToken returns a random URL safe token with 192 bits.
func newToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// parentLink returns the parent device link the request is signed in with.
func (s *Server) parentLink(r *http.Request) (string, bool) {
	c, err := r.Cookie(accessCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	l, err := s.store.AccessLinkByToken(c.Value, s.engine.Now())
	if err != nil || l.Kind != store.LinkParent {
		return "", false
	}
	return c.Value, true
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// parentAccess signs a device in with its link and opens the dashboard.
func (s *Server) parentAccess(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	l, err := s.store.AccessLinkByToken(token, s.engine.Now())
	if err != nil || l.Kind != store.LinkParent {
		s.loginFailed(r, "Zugangslink ungültig")
		http.Redirect(w, r, "/login?link=1", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure only behind HTTPS so that plain HTTP on the LAN keeps working
		Name: accessCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: secureRequest(r), MaxAge: int(linkCookieAge.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// kidTarget resolves the kid of a kid app URL: /k/{token} with a kid link,
// or /kind/{id} for signed in parents. base is the URL prefix of the app.
func (s *Server) kidTarget(r *http.Request) (engine.ProfileView, string, bool) {
	if token := r.PathValue("token"); token != "" {
		l, err := s.store.AccessLinkByToken(token, s.engine.Now())
		if err != nil || l.Kind != store.LinkKid {
			return engine.ProfileView{}, "", false
		}
		v, ok := s.kidView(l.ProfileID)
		return v, "/k/" + token, ok
	}
	id := r.PathValue("id")
	v, ok := s.kidView(id)
	return v, "/kind/" + id, ok
}

// --- login throttling ---------------------------------------------------

const (
	loginWindow   = 15 * time.Minute
	loginMaxFails = 5
)

// loginLimiter counts failed logins per client address.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func (l *loginLimiter) recent(ip string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.fails[ip] {
		if now.Sub(t) < loginWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = keep
	}
	return keep
}

// blocked reports whether ip had too many failed logins recently.
func (l *loginLimiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip, now)) >= loginMaxFails
}

func (l *loginLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string][]time.Time{}
	}
	l.fails[ip] = append(l.recent(ip, now), now)
}

// clientIP is the address of the client. Behind the ingress it is the last
// entry of X-Forwarded-For, the one the proxy added.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) loginFailed(r *http.Request, what string) {
	ip := clientIP(r)
	s.limiter.fail(ip, time.Now())
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), Kind: "login", Message: what + " von " + ip})
}

// sameOrigin rejects form posts from other sites. Browsers mark every
// request with Sec-Fetch-Site; older ones send Origin, which is "null"
// here because pages use no-referrer. Requests with neither (scripts,
// tests) are let through, the session cookie is SameSite=Lax anyway.
func sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// --- settings -------------------------------------------------------------

// newLink is a link that was just created; its token is shown only once.
type newLink struct {
	Label string
	Kid   string
	URL   string
}

// accessData lists the links on the settings page.
type accessData struct {
	Parents []linkView
	Kids    []linkView
	New     *newLink
}

type linkView struct {
	store.AccessLink
	Kid string
}

func (s *Server) accessData() accessData {
	var d accessData
	links, _ := s.store.AccessLinks()
	names := s.profileNames()
	for _, l := range links {
		if l.Kind == store.LinkParent {
			d.Parents = append(d.Parents, linkView{AccessLink: l})
		} else {
			d.Kids = append(d.Kids, linkView{AccessLink: l, Kid: names[l.ProfileID]})
		}
	}
	return d
}

// baseURL is the address the request was made to, for links to share.
func baseURL(r *http.Request) string {
	scheme := "http"
	if secureRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) addAccessLink(w http.ResponseWriter, r *http.Request) {
	label := strings.TrimSpace(r.FormValue("label"))
	l := store.AccessLink{Kind: r.FormValue("kind"), ProfileID: r.FormValue("profile"), Label: label, Created: s.engine.Now()}
	var kid string
	switch l.Kind {
	case store.LinkParent:
		l.ProfileID = ""
	case store.LinkKid:
		p, err := s.store.Profile(l.ProfileID)
		if err != nil {
			redirectErr(w, r, "/einstellungen#zugang", errors.New("bitte ein Kind wählen"))
			return
		}
		kid = p.Name
	default:
		redirectErr(w, r, "/einstellungen#zugang", errors.New("unbekannte Art von Link"))
		return
	}
	if label == "" {
		redirectErr(w, r, "/einstellungen#zugang", errors.New("bitte einen Namen für das Gerät angeben"))
		return
	}
	token, err := newToken()
	if err == nil {
		_, err = s.store.AddAccessLink(l, token)
	}
	if err != nil {
		redirectErr(w, r, "/einstellungen#zugang", err)
		return
	}
	path := "/zugang/" + token
	msg := "Elternzugang für " + label + " erstellt"
	if l.Kind == store.LinkKid {
		path = "/k/" + token
		msg = fmt.Sprintf("Kinder-App für %s auf %s erstellt", kid, label)
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), ProfileID: l.ProfileID, Kind: "config", Message: msg})
	created := &newLink{Label: label, Kid: kid, URL: baseURL(r) + path}
	s.settingsWith(w, r, created)
}

func (s *Server) removeAccessLink(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.store.DeleteAccessLink(id)
	}
	if err != nil {
		redirectErr(w, r, "/einstellungen#zugang", errors.New("Link nicht gefunden"))
		return
	}
	_ = s.store.AddEvent(store.Event{TS: s.engine.Now(), Kind: "config", Message: "Zugangslink gesperrt"})
	redirect(w, r, "/einstellungen#zugang", "Link gesperrt, das Gerät hat keinen Zugang mehr")
}
