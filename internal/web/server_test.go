package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/slauger/zockerzeit/internal/demo"
	"github.com/slauger/zockerzeit/internal/engine"
	"github.com/slauger/zockerzeit/internal/store"
)

var berlin, _ = time.LoadLocation("Europe/Berlin")

type env struct {
	t      *testing.T
	store  *store.Store
	srv    *httptest.Server
	client *http.Client
}

func newEnv(t *testing.T, password string) *env {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := demo.Seed(st, time.Now().In(berlin)); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(st, demo.NewController(), nil, engine.Options{Location: berlin, MinBytesPerMinute: 1}, log)
	h, err := New(st, eng, Options{AdminPassword: password, SessionSecret: "s3cret", Version: "test"}, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, store: st, srv: srv, client: &http.Client{Jar: jar}}
}

func (e *env) get(path string) (int, string) {
	e.t.Helper()
	resp, err := e.client.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) post(path string, form url.Values) (int, string) {
	e.t.Helper()
	resp, err := e.client.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPagesRender(t *testing.T) {
	e := newEnv(t, "")
	for path, want := range map[string]string{
		"/":          "Gemeinsames Gerät freigeben",
		"/verlauf":   "Spielzeit pro Tag",
		"/regeln":    "Neues Profil",
		"/geraete":   "Switch Kinderzimmer",
		"/protokoll": "Protokoll",
		"/kind/max":  "Hallo Max!",
		"/login":     "Passwort",
		"/healthz":   "ok",
		"/readyz":    "ok",
	} {
		code, body := e.get(path)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: status %d, missing %q", path, code, want)
		}
	}
	if code, _ := e.get("/kind/niemand"); code != http.StatusNotFound {
		t.Errorf("unknown kid: %d", code)
	}
	if code, _ := e.get("/static/app.css"); code != http.StatusOK {
		t.Errorf("static asset: %d", code)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t, "geheim")
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := e.client.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("expected redirect to login, got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _ := e.get("/api/status"); code != http.StatusUnauthorized {
		t.Fatalf("api without session: %d", code)
	}
	// The kid view stays public.
	if code, _ := e.get("/kind/max"); code != http.StatusOK {
		t.Fatalf("kid view: %d", code)
	}

	resp, err = e.client.PostForm(e.srv.URL+"/login", url.Values{"password": {"geheim"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("login failed: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if code, _ := e.get("/api/status"); code != http.StatusOK {
		t.Fatalf("api with session: %d", code)
	}

	resp, err = e.client.PostForm(e.srv.URL+"/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if code, _ := e.get("/api/status"); code != http.StatusUnauthorized {
		t.Fatalf("api after logout: %d", code)
	}
}

func TestWrongPasswordAndForgedCookie(t *testing.T) {
	e := newEnv(t, "geheim")
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := e.client.PostForm(e.srv.URL+"/login", url.Values{"password": {"falsch"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "fehler") {
		t.Fatalf("wrong password accepted: %s", resp.Header.Get("Location"))
	}
	u, _ := url.Parse(e.srv.URL)
	future := time.Now().Add(time.Hour).Unix()
	for _, v := range []string{"garbage", "abc.def", strings.Repeat("9", 5) + ".00", "1.00", "9999999999.00", strconv.FormatInt(future, 10) + ".00"} {
		e.client.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: v}})
		if code, _ := e.get("/api/status"); code != http.StatusUnauthorized {
			t.Fatalf("forged cookie %q accepted", v)
		}
	}
}

func TestActions(t *testing.T) {
	e := newEnv(t, "")
	cases := []url.Values{
		{"action": {"bonus"}, "profile": {"max"}, "minutes": {"15"}},
		{"action": {"allow"}, "profile": {"max"}, "minutes": {"30"}},
		{"action": {"lock"}, "profile": {"lea"}, "minutes": {"0"}},
		{"action": {"release"}, "profile": {"lea"}, "device": {"98:b6:e9:00:00:01"}, "minutes": {"30"}},
		{"action": {"lockdevice"}, "device": {"3a:10:5f:00:00:03"}, "minutes": {"60"}},
	}
	for _, form := range cases {
		code, body := e.post("/aktion", form)
		if code != http.StatusOK || strings.Contains(body, `class="banner err"`) {
			t.Fatalf("action %v failed: %d", form, code)
		}
	}
	ovs, _ := e.store.ActiveOverrides(time.Now(), berlin)
	if len(ovs) != 5 {
		t.Fatalf("expected 5 overrides, got %d", len(ovs))
	}

	var status struct {
		Profiles []engine.ProfileView `json:"profiles"`
	}
	_, body := e.get("/api/status")
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	for _, p := range status.Profiles {
		if p.Profile.ID == "lea" && p.Status.Allowed {
			t.Fatal("lea should be locked")
		}
		if p.Profile.ID == "max" && !p.Status.Allowed {
			t.Fatal("max should be allowed")
		}
	}

	code, _ := e.post("/aktion/"+strconv.FormatInt(ovs[0].ID, 10)+"/beenden", nil)
	if code != http.StatusOK {
		t.Fatalf("end override: %d", code)
	}
	if _, body := e.post("/aktion/"+strconv.FormatInt(ovs[0].ID, 10)+"/beenden", nil); !strings.Contains(body, "nicht gefunden") {
		t.Fatal("ending twice should report an error")
	}
	if _, body := e.post("/aktion/abc/beenden", nil); !strings.Contains(body, "banner err") {
		t.Fatal("invalid id should report an error")
	}

	for _, bad := range []url.Values{
		{"action": {"explode"}},
		{"action": {"allow"}, "profile": {"max"}, "minutes": {"0"}},
		{"action": {"release"}, "profile": {"lea"}, "minutes": {"10"}},
		{"action": {"bonus"}, "minutes": {"10"}},
	} {
		if _, body := e.post("/aktion", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestProfilesAndDevices(t *testing.T) {
	e := newEnv(t, "")
	form := url.Values{"name": {"Großer Bruder"}, "daily": {"60"}, "weekly": {"300"}, "win_1": {"15:00-17:00"}, "win_6": {"10:00-12:00, 14:00-18:00"}}
	if _, body := e.post("/regeln", form); !strings.Contains(body, "gespeichert") {
		t.Fatal("saving profile failed")
	}
	p, err := e.store.Profile("grosser-bruder")
	if err != nil {
		t.Fatal(err)
	}
	if p.DailyMinutes != 60 || len(p.Schedule[time.Saturday]) != 2 || len(p.Schedule[time.Sunday]) != 0 {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if _, body := e.post("/regeln", form); !strings.Contains(body, "gibt es schon") {
		t.Fatal("duplicate profile accepted")
	}
	for _, bad := range []url.Values{
		{"name": {""}},
		{"name": {"X"}, "win_1": {"18:00-15:00"}},
		{"name": {"Y"}, "daily": {"-5"}},
	} {
		if _, body := e.post("/regeln", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}

	dev := url.Values{"mac": {"AA-BB-CC-DD-EE-FF"}, "name": {"Switch Oma"}, "profile": {"grosser-bruder"}, "mode": {"online"}, "shared": {"on"}}
	if _, body := e.post("/geraete", dev); !strings.Contains(body, "gespeichert") {
		t.Fatal("saving device failed")
	}
	devs, _ := e.store.Devices()
	found := false
	for _, d := range devs {
		if d.MAC == "aa:bb:cc:dd:ee:ff" && d.Shared && d.ProfileID == "grosser-bruder" {
			found = true
		}
	}
	if !found {
		t.Fatalf("device not stored: %+v", devs)
	}
	for _, bad := range []url.Values{
		{"mac": {"nope"}, "profile": {"max"}},
		{"mac": {"aa:bb:cc:dd:ee:zz"}, "profile": {"max"}},
		{"mac": {"aa:bb:cc:dd:ee:f"}, "profile": {"max"}},
		{"mac": {"aa:bb:cc:dd:ee:01"}, "profile": {"nobody"}},
	} {
		if _, body := e.post("/geraete", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}
	if _, body := e.post("/geraete/aa:bb:cc:dd:ee:ff/loeschen", nil); !strings.Contains(body, "entfernt") {
		t.Fatal("deleting device failed")
	}
	if _, body := e.post("/regeln/grosser-bruder/loeschen", nil); !strings.Contains(body, "gelöscht") {
		t.Fatal("deleting profile failed")
	}
	if _, body := e.post("/regeln/max/loeschen", nil); !strings.Contains(body, "gelöscht") {
		t.Fatal("deleting profile with devices failed")
	}
}

func TestStatsAndKidAPI(t *testing.T) {
	e := newEnv(t, "")
	code, body := e.get("/api/stats?profile=max&days=7&day=" + time.Now().In(berlin).AddDate(0, 0, -1).Format("2006-01-02"))
	if code != http.StatusOK {
		t.Fatalf("stats: %d", code)
	}
	var st statsResponse
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Limit != 90 || len(st.Daily.Labels) != 7 || len(st.Devices) == 0 || len(st.Locations) == 0 {
		t.Fatalf("unexpected stats: limit=%d labels=%d devices=%d", st.Limit, len(st.Daily.Labels), len(st.Devices))
	}
	if code, _ := e.get("/api/stats?profile=max&days=abc&day=kaputt"); code != http.StatusOK {
		t.Fatalf("stats with bad params: %d", code)
	}

	code, body = e.get("/api/kind/max")
	if code != http.StatusOK || !strings.Contains(body, `"name":"Max"`) {
		t.Fatalf("kid api: %d %s", code, body)
	}
	if code, _ := e.get("/api/kind/niemand"); code != http.StatusNotFound {
		t.Fatalf("unknown kid api: %d", code)
	}
}

func TestHelpers(t *testing.T) {
	if slugify("  Max & Moritz  ") != "max-moritz" || slugify("Jürgen") != "juergen" || slugify("!!!") != "profil" {
		t.Fatal("slugify")
	}
	if formatDuration(-time.Minute) != "0 Min" || formatDuration(65*time.Minute) != "1:05 Std" {
		t.Fatal("formatDuration")
	}
	f := funcs(berlin)
	if f["clock"].(func(time.Time) string)(time.Time{}) != "-" {
		t.Fatal("clock zero")
	}
	pct := f["pct"].(func(time.Duration, int) int)
	if pct(time.Hour, 0) != 0 || pct(2*time.Hour, 60) != 100 || pct(30*time.Minute, 60) != 50 {
		t.Fatal("pct")
	}
}
