package web

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/slauger/zockerzeit/internal/nintendo"
	"github.com/slauger/zockerzeit/internal/notify"
	"github.com/slauger/zockerzeit/internal/rules"
	"github.com/slauger/zockerzeit/internal/secret"
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
		"/":                              "Mehr Zeit",
		"/geraet/98:b6:e9:00:00:01":      "Ist jetzt bei Mia",
		"/verlauf":                       "Im WLAN pro Tag",
		"/regeln":                        "Neues Profil",
		"/geraete":                       "Switch Kinderzimmer",
		"/protokoll":                     "Protokoll",
		"/einstellungen":                 "Nur Internet (FRITZ!Box)",
		"/protokoll?art=online":          "Online und offline",
		"/kind/max":                      "Hallo Max!",
		"/kind/max/manifest.webmanifest": `"start_url":"/kind/max"`,
		"/kind/sw.js":                    "zockerzeit-kid-v1",
		"/login":                         "Passwort",
		"/healthz":                       "ok",
		"/readyz":                        "ok",
	} {
		code, body := e.get(path)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: status %d, missing %q", path, code, want)
		}
	}
	if code, _ := e.get("/geraet/00:00:00:00:00:00"); code != http.StatusNotFound {
		t.Errorf("unknown device: %d", code)
	}
	if code, _ := e.get("/kind/niemand/manifest.webmanifest"); code != http.StatusNotFound {
		t.Errorf("unknown kid manifest: %d", code)
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
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := e.client.PostForm(e.srv.URL+"/aktion", url.Values{"action": {"lockdevice"}, "device": {"3a:10:5f:00:00:04"}, "minutes": {"30"}, "back": {"/geraet/3a:10:5f:00:00:04"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/geraet/3a:10:5f:00:00:04?ok=") {
		t.Fatalf("expected redirect back to the device page, got %q", loc)
	}
	if err := e.store.EndOverride(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, back := range []string{"//evil.example", "https://evil.example"} {
		resp, err := e.client.PostForm(e.srv.URL+"/aktion", url.Values{"action": {"nope"}, "back": {back}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/?") {
			t.Fatalf("open redirect to %q", loc)
		}
	}
	for _, kind := range []string{"unlockdevice-next", "lockdevice-next"} {
		resp, err := e.client.PostForm(e.srv.URL+"/aktion", url.Values{"action": {kind}, "device": {"98:b6:e9:00:00:02"}, "back": {"/geraet/98:b6:e9:00:00:02"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if loc := resp.Header.Get("Location"); !strings.Contains(loc, "ok=") {
			t.Fatalf("%s failed: %q", kind, loc)
		}
	}
	ovs, _ := e.store.ActiveOverrides(time.Now(), berlin)
	for _, o := range ovs {
		if o.DeviceMAC == "98:b6:e9:00:00:02" {
			_ = e.store.EndOverride(o.ID, time.Now())
			if !o.Until.After(time.Now()) {
				t.Fatalf("override %s ends in the past", o.Kind)
			}
		}
	}
	e.client.CheckRedirect = nil
	cases := []url.Values{
		{"action": {"bonus"}, "profile": {"max"}, "minutes": {"15"}},
		{"action": {"allow"}, "profile": {"max"}, "minutes": {"30"}},
		{"action": {"lock"}, "profile": {"mia"}, "minutes": {"0"}},
		{"action": {"release"}, "profile": {"mia"}, "device": {"98:b6:e9:00:00:01"}, "minutes": {"30"}},
		{"action": {"release"}, "profile": {"mia"}, "device": {"98:b6:e9:00:00:02"}, "minutes": {"0"}},
		{"action": {"lockdevice"}, "device": {"3a:10:5f:00:00:03"}, "minutes": {"60"}},
	}
	for _, form := range cases {
		code, body := e.post("/aktion", form)
		if code != http.StatusOK || strings.Contains(body, `class="banner err"`) {
			t.Fatalf("action %v failed: %d", form, code)
		}
	}
	ovs, _ = e.store.ActiveOverrides(time.Now(), berlin)
	if len(ovs) != 6 {
		t.Fatalf("expected 6 overrides, got %d", len(ovs))
	}

	var status struct {
		Profiles []engine.ProfileView `json:"profiles"`
	}
	_, body := e.get("/api/status")
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	for _, p := range status.Profiles {
		if p.Profile.ID == "mia" && p.Console.Allowed {
			t.Fatal("mia should be locked")
		}
		if p.Profile.ID == "max" && !p.Console.Allowed {
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
		{"action": {"release"}, "profile": {"mia"}, "minutes": {"10"}},
		{"action": {"bonus"}, "minutes": {"10"}},
	} {
		if _, body := e.post("/aktion", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestProfilesAndDevices(t *testing.T) {
	e := newEnv(t, "")
	form := url.Values{
		"name": {"Großer Bruder"}, "console_daily": {"60"}, "console_weekly": {"300"},
		"console_win_1": {"15:00-17:00"}, "console_win_6": {"10:00-12:00, 14:00-18:00"},
		"phone_daily": {""}, "phone_win_0": {"07:00-20:30"},
	}
	if _, body := e.post("/regeln", form); !strings.Contains(body, "gespeichert") {
		t.Fatal("saving profile failed")
	}
	p, err := e.store.Profile("grosser-bruder")
	if err != nil {
		t.Fatal(err)
	}
	if p.Console.DailyMinutes != 60 || len(p.Console.Schedule[time.Saturday]) != 2 || len(p.Console.Schedule[time.Sunday]) != 0 ||
		p.Phone.DailyMinutes != 0 || len(p.Phone.Schedule[time.Sunday]) != 1 {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if _, body := e.post("/regeln", form); !strings.Contains(body, "gibt es schon") {
		t.Fatal("duplicate profile accepted")
	}
	for _, bad := range []url.Values{
		{"name": {""}},
		{"name": {"X"}, "console_win_1": {"18:00-15:00"}},
		{"name": {"Y"}, "console_daily": {"-5"}},
		{"name": {"Z"}, "phone_weekly": {"viel"}},
		{"name": {"W"}, "phone_win_3": {"abends"}},
	} {
		if _, body := e.post("/regeln", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}

	dev := url.Values{"mac": {"AA-BB-CC-DD-EE-FF"}, "name": {"Switch Oma"}, "profile": {"grosser-bruder"}, "mode": {"online"}, "shared": {"on"}, "blocker": {"internet"}, "schedule_block": {"on"}}
	if _, body := e.post("/geraete", dev); !strings.Contains(body, "gespeichert") {
		t.Fatal("saving device failed")
	}
	devs, _ := e.store.Devices()
	found := false
	for _, d := range devs {
		if d.MAC == "aa:bb:cc:dd:ee:ff" && d.Shared && d.ProfileID == "grosser-bruder" && d.Blocker == store.BlockInternet && d.ScheduleBlock {
			found = true
		}
	}
	if !found {
		t.Fatalf("device not stored: %+v", devs)
	}
	// Switching back to WLAN blocking lifts the internet block first.
	dev.Set("blocker", "wlan")
	if _, body := e.post("/geraete", dev); !strings.Contains(body, "gespeichert") {
		t.Fatal("switching the blocker failed")
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
	code, body := e.get("/api/stats?profile=max&type=console&days=7&day=" + time.Now().In(berlin).AddDate(0, 0, -1).Format("2006-01-02"))
	if code != http.StatusOK {
		t.Fatalf("stats: %d", code)
	}
	var st statsResponse
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Limit != 0 || len(st.Daily.Labels) != 7 || len(st.Devices) == 0 || len(st.Locations) == 0 {
		t.Fatalf("unexpected stats: limit=%d labels=%d devices=%d", st.Limit, len(st.Daily.Labels), len(st.Devices))
	}
	_, body = e.get("/api/stats?profile=max&days=7&type=phone")
	var phone statsResponse
	if err := json.Unmarshal([]byte(body), &phone); err != nil {
		t.Fatal(err)
	}
	if phone.Limit != 60 || len(phone.Devices) != 1 || phone.Devices[0].Name != "iPhone Max" {
		t.Fatalf("phone stats not filtered: limit=%d devices=%+v", phone.Limit, phone.Devices)
	}
	_, body = e.get("/api/stats?profile=max&days=7&type=alle")
	var all statsResponse
	_ = json.Unmarshal([]byte(body), &all)
	if all.Limit != 0 || len(all.Devices) != 3 {
		t.Fatalf("unfiltered stats: limit=%d devices=%d", all.Limit, len(all.Devices))
	}
	if code, _ := e.get("/verlauf?typ=phone"); code != http.StatusOK {
		t.Fatalf("history with type: %d", code)
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

func TestEnergyCells(t *testing.T) {
	cases := []struct {
		st   rules.Status
		want string
	}{
		{rules.Status{}, ""},
		{rules.Status{Allowed: true, LimitToday: 60, UsedToday: 20 * time.Minute}, "used,left,left,left"},
		{rules.Status{Allowed: true, LimitToday: 60, UsedToday: 50 * time.Minute}, "used,used,used,low"},
		{rules.Status{Allowed: false, LimitToday: 45, UsedToday: 0}, "paused,paused,paused"},
		{rules.Status{Allowed: false, LimitToday: 30, UsedToday: time.Hour}, "used,used"},
	}
	for _, c := range cases {
		if got := strings.Join(energyCells(c.st), ","); got != c.want {
			t.Errorf("energyCells(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
	f := funcs(berlin)
	if got := f["germanDate"].(func(time.Time) string)(time.Date(2026, 11, 20, 12, 0, 0, 0, berlin)); got != "Freitag, 20. November" {
		t.Errorf("germanDate = %q", got)
	}
	if f["initial"].(func(string) string)("mia") != "M" || f["initial"].(func(string) string)("") != "?" {
		t.Error("initial")
	}
	ql := f["quotaLeft"].(func(rules.Status) time.Duration)
	if ql(rules.Status{}) != 0 || ql(rules.Status{LimitToday: 30, UsedToday: time.Hour}) != 0 || ql(rules.Status{LimitToday: 30}) != 30*time.Minute {
		t.Error("quotaLeft")
	}
}

func TestSettingsPage(t *testing.T) {
	e := newEnv(t, "")
	if _, body := e.post("/einstellungen", url.Values{"dry_run": {"on"}, "wlan_blocking": {"on"}}); !strings.Contains(body, "Testmodus an") {
		t.Fatal("enabling test mode not confirmed")
	}
	_, body := e.get("/")
	if !strings.Contains(body, "banner warn") {
		t.Fatal("test mode banner missing")
	}
	if _, body := e.post("/einstellungen", url.Values{"wlan_blocking": {"on"}}); !strings.Contains(body, "Testmodus aus") {
		t.Fatal("disabling test mode not confirmed")
	}
	if _, body := e.post("/einstellungen", url.Values{}); !strings.Contains(body, "Einstellungen gespeichert") {
		t.Fatal("saving settings not confirmed")
	}
	ev, _ := e.store.Events(5, berlin, "config")
	if len(ev) < 3 {
		t.Fatalf("settings changes not logged: %+v", ev)
	}
}

type fakeNintendo struct {
	st      *store.Store
	synced  int
	removed int64
}

func (f *fakeNintendo) LoginURL() (string, string, error) {
	return "https://accounts.example/authorize?x=1", "VERIFIER", nil
}

func (f *fakeNintendo) Connect(_ context.Context, label, redirect, verifier string) (int64, error) {
	if !strings.Contains(redirect, "session_token_code=") || verifier != "VERIFIER" {
		return 0, errors.New("the URL contains no session_token_code")
	}
	id, err := f.st.AddNintendoAccount(store.NintendoAccount{Label: label, AccountID: "acc", SessionToken: "enc"})
	if err != nil {
		return 0, err
	}
	today := time.Now().In(berlin).Format("2006-01-02")
	_ = f.st.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "D1", Account: id, Label: "Switch 2", Model: "Switch 2"})
	_ = f.st.UpsertNintendoPlayer(store.NintendoPlayer{PlayerID: "P1", DeviceID: "D1", Nickname: "Spieler A"})
	_ = f.st.UpsertNintendoUsage(store.NintendoUsage{Date: today, DeviceID: "D1", PlayerID: "P1", Minutes: 42, Games: []store.NintendoGame{{ApplicationID: "A", Minutes: 42}}})
	return id, nil
}

func (f *fakeNintendo) Remove(id int64) error {
	f.removed = id
	return f.st.DeleteNintendoAccount(id)
}

func (f *fakeNintendo) SyncAll(context.Context) { f.synced++ }

func TestNintendoPages(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := demo.Seed(st, time.Now().In(berlin)); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(st, demo.NewController(), nil, engine.Options{Location: berlin}, log)
	fn := &fakeNintendo{st: st}
	h, err := New(st, eng, Options{Nintendo: fn}, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	e := &env{t: t, store: st, srv: srv, client: &http.Client{Jar: jar}}

	if _, body := e.get("/einstellungen/nintendo"); !strings.Contains(body, "Account verbinden") {
		t.Fatal("connect button missing")
	}
	if _, body := e.get("/einstellungen"); !strings.Contains(body, "Noch kein Account verbunden") || !strings.Contains(body, "/einstellungen/nintendo/login") {
		t.Fatal("Nintendo card missing on settings page")
	}
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := e.client.PostForm(srv.URL+"/einstellungen/nintendo/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	key := strings.TrimSuffix(strings.TrimPrefix(loc, "/einstellungen/nintendo?login="), "#verbinden")
	e.client.CheckRedirect = nil
	if _, body := e.get("/einstellungen/nintendo?login=" + key); !strings.Contains(body, "accounts.example") {
		t.Fatal("login link missing")
	}
	if _, body := e.post("/einstellungen/nintendo/verbinden", url.Values{"key": {key}, "label": {"Papa"}, "redirect": {"falsch"}}); !strings.Contains(body, "session_token_code") {
		t.Fatal("bad redirect should show the error and keep the login")
	}
	if _, body := e.post("/einstellungen/nintendo/verbinden", url.Values{"key": {"unknown"}}); !strings.Contains(body, "abgelaufen") {
		t.Fatal("unknown login key accepted")
	}
	if _, body := e.post("/einstellungen/nintendo/verbinden", url.Values{"key": {key}, "label": {"Papa"}, "redirect": {"npf://auth#session_token_code=C"}}); !strings.Contains(body, "Papa verbunden") {
		t.Fatal("connect failed")
	}
	if _, body := e.post("/einstellungen/nintendo/konsole", url.Values{"device_id": {"D1"}, "mac": {"98:B6:E9:00:00:01"}}); !strings.Contains(body, "Konsole zugeordnet") {
		t.Fatal("mapping console failed")
	}
	if _, body := e.post("/einstellungen/nintendo/spieler", url.Values{"player_id": {"P1"}, "profile": {"max"}}); !strings.Contains(body, "Spieler zugeordnet") {
		t.Fatal("mapping player failed")
	}
	if _, body := e.post("/einstellungen/nintendo/spieler", url.Values{"player_id": {"nope"}, "profile": {"max"}}); !strings.Contains(body, "banner err") {
		t.Fatal("unknown player accepted")
	}
	if _, body := e.post("/einstellungen/nintendo/konsole", url.Values{"device_id": {"nope"}}); !strings.Contains(body, "banner err") {
		t.Fatal("unknown console accepted")
	}
	_, body := e.get("/einstellungen/nintendo")
	if !strings.Contains(body, "Spieler A") || !strings.Contains(body, "42 Min") {
		t.Fatal("players or usage missing on the page")
	}
	if _, body := e.get("/"); !strings.Contains(body, "Laut Nintendo heute <strong>42 Min</strong>") || !strings.Contains(body, "Switch Kinderzimmer 42 Min") {
		t.Fatal("dashboard Nintendo line missing")
	}
	if _, body := e.get("/einstellungen"); !strings.Contains(body, "Verbunden: Papa") {
		t.Fatal("connected account missing on settings page")
	}
	if _, body := e.post("/einstellungen/nintendo/sync", nil); !strings.Contains(body, "abgerufen") || fn.synced != 1 {
		t.Fatal("sync not triggered")
	}
	accs, _ := st.NintendoAccounts()
	if _, body := e.post("/einstellungen/nintendo/"+strconv.FormatInt(accs[0].ID, 10)+"/entfernen", nil); !strings.Contains(body, "entfernt") {
		t.Fatal("remove failed")
	}
	if _, body := e.post("/einstellungen/nintendo/abc/entfernen", nil); !strings.Contains(body, "banner err") {
		t.Fatal("invalid id accepted")
	}
}

func TestHistoryGames(t *testing.T) {
	e := newEnv(t, "")
	if _, body := e.get("/verlauf?profil=max"); !strings.Contains(body, "Benutzer auf der Switch diesem Profil zuordnen") {
		t.Fatal("hint for unmapped profile missing")
	}
	now := time.Now().In(berlin)
	if err := demo.SeedNintendo(e.store, now); err != nil {
		t.Fatal(err)
	}
	if err := demo.SeedNintendo(e.store, now); err != nil {
		t.Fatal(err)
	}
	if accs, _ := e.store.NintendoAccounts(); len(accs) != 1 {
		t.Fatalf("SeedNintendo not idempotent: %d accounts", len(accs))
	}
	_ = e.store.UpsertNintendoUsage(store.NintendoUsage{Date: now.Format("2006-01-02"), DeviceID: "demo-switch-1", PlayerID: "demo-max", Minutes: 30,
		Games: []store.NintendoGame{{ApplicationID: "0100000000de0001", Minutes: 20}, {ApplicationID: "0100FFFF", Minutes: 10}, {ApplicationID: "0100000000DE0003"}}})
	cover := "https://img.example/kart.png?x=1"
	_ = e.store.UpsertNintendoTitle(store.NintendoTitle{ApplicationID: "0100000000DE0001", ImageURI: cover})
	_ = e.store.UpsertNintendoTitle(store.NintendoTitle{ApplicationID: "0100000000DE0002", ImageURI: "https://img.example/not-cached.png"})
	_ = e.store.SaveImage(store.Image{Source: cover, ContentType: "image/png", Data: []byte("png"), Fetched: now})
	_, body := e.get("/verlauf?profil=max&tage=7")
	if !strings.Contains(body, `src="/bild/`+store.ImageKey(cover)+`"`) || strings.Contains(body, "img.example") {
		t.Fatal("covers must be served from the stored copy only")
	}
	resp, err := e.client.Get(e.srv.URL + "/bild/" + store.ImageKey(cover))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "png" || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "img-src 'self'") {
		t.Fatalf("image response: %q %v", b, resp.Header)
	}
	if code, _ := e.get("/bild/unknown"); code != http.StatusNotFound {
		t.Fatalf("unknown image: %d", code)
	}
	for _, want := range []string{"Kart-Rennen Turbo", "Unbekanntes Spiel", "Switch Kinderzimmer", "Pro Tag", "in 7 Tagen"} {
		if !strings.Contains(body, want) {
			t.Fatalf("games section misses %q", want)
		}
	}
	if _, body := e.get("/verlauf?profil=max&typ=phone"); strings.Contains(body, "Kart-Rennen") {
		t.Fatal("games shown for phones")
	}
	if _, body := e.get("/verlauf?profil=max&tage=7&tag=2000-01-01"); !strings.Contains(body, "Kart-Rennen") {
		t.Fatal("games depend on the timeline day")
	}
	_ = e.store.SetNintendoPlayerProfile("demo-max", "")
	_ = e.store.SetNintendoPlayerProfile("demo-mia", "max")
	for back := range 14 {
		day := now.AddDate(0, 0, -back).Format("2006-01-02")
		usage, _ := e.store.NintendoUsageOn(day)
		for _, u := range usage {
			u.Minutes = 0
			_ = e.store.UpsertNintendoUsage(u)
		}
	}
	// Nintendo corrected the days downwards: the next sync attributes that too.
	if err := nintendo.Attribute(e.store, now, berlin); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get("/verlauf?profil=max"); !strings.Contains(body, "nichts gespielt") {
		t.Fatal("empty period notice missing")
	}
}

func TestGamesPage(t *testing.T) {
	e := newEnv(t, "")
	if _, body := e.get("/spiele"); !strings.Contains(body, "keine Spielzeit gemeldet") {
		t.Fatal("empty games page hint missing")
	}
	now := time.Now().In(berlin)
	if err := demo.SeedNintendo(e.store, now); err != nil {
		t.Fatal(err)
	}
	// An unmapped user (a parent) and an unknown game.
	_ = e.store.UpsertNintendoPlayer(store.NintendoPlayer{PlayerID: "papa", DeviceID: "demo-switch-1", Nickname: "Papa"})
	_ = e.store.UpsertNintendoUsage(store.NintendoUsage{Date: now.AddDate(0, 0, -20).Format("2006-01-02"), DeviceID: "demo-switch-1", PlayerID: "papa", Minutes: 9,
		Games: []store.NintendoGame{{ApplicationID: "0100FFFF", Minutes: 9}}})
	_, body := e.get("/spiele")
	for _, want := range []string{"Inselabenteuer", "<strong>Max</strong>", "<strong>Mia</strong>", "<strong>Andere</strong>", "Unbekanntes Spiel", "zuletzt heute", "Switch Kinderzimmer", "insgesamt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("games page misses %q", want)
		}
	}
	_, body = e.get("/spiele?zeitraum=7")
	if strings.Contains(body, "Unbekanntes Spiel") || !strings.Contains(body, "in diesem Zeitraum") {
		t.Fatal("range filter not applied")
	}
	if _, body := e.get("/spiele?zeitraum=30"); !strings.Contains(body, `value="30" selected`) {
		t.Fatal("30 days not selected")
	}
}

func TestNotifySettings(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	box, _ := secret.NewBox([]byte("k"), "notify")
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"description":"chat not found"}`)
	}))
	t.Cleanup(tg.Close)
	hub := notify.NewHub(st, box, nil, log)
	eng := engine.New(st, demo.NewController(), hub, engine.Options{Location: berlin}, log)
	h, err := New(st, eng, Options{Notify: hub}, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	e := &env{t: t, store: st, srv: srv, client: &http.Client{Jar: jar}}

	if _, body := e.get("/einstellungen"); !strings.Contains(body, "Noch kein Kanal") || !strings.Contains(body, "Telegram hinzufügen") {
		t.Fatal("notification card missing")
	}
	if _, body := e.post("/einstellungen/benachrichtigungen", url.Values{"event_start": {"on"}}); !strings.Contains(body, "Benachrichtigungen gespeichert") {
		t.Fatal("saving events failed")
	}
	if !hub.Enabled(notify.EventStart) || hub.Enabled(notify.EventWarn) {
		t.Fatal("events not stored")
	}
	if _, body := e.post("/einstellungen/kanal", url.Values{"kind": {"telegram"}, "token": {"t"}}); !strings.Contains(body, "Chat-ID") {
		t.Fatal("incomplete channel accepted")
	}
	if _, body := e.post("/einstellungen/kanal", url.Values{"kind": {"ntfy"}, "label": {"Handy"}, "url": {tg.URL}}); !strings.Contains(body, "Kanal hinzugefügt") {
		t.Fatal("adding channel failed")
	}
	chans, _ := st.NotifyChannels()
	if len(chans) != 1 {
		t.Fatalf("channels: %+v", chans)
	}
	id := strconv.FormatInt(chans[0].ID, 10)
	if _, body := e.post("/einstellungen/kanal/"+id+"/test", nil); !strings.Contains(body, "Testnachricht verschickt") {
		t.Fatal("test message failed")
	}
	if _, body := e.post("/einstellungen/kanal/x/test", nil); !strings.Contains(body, "unbekannter Kanal") {
		t.Fatal("bad id accepted")
	}
	if _, body := e.post("/einstellungen/kanal/"+id+"/entfernen", nil); !strings.Contains(body, "Kanal entfernt") {
		t.Fatal("removing failed")
	}
	if _, body := e.post("/einstellungen/kanal/"+id+"/entfernen", nil); !strings.Contains(body, "banner err") {
		t.Fatal("removing twice should fail")
	}
	if _, body := e.post("/einstellungen/kanal/x/entfernen", nil); !strings.Contains(body, "unbekannter Kanal") {
		t.Fatal("bad id accepted")
	}

	// Without the hub the routes refuse.
	e2 := newEnv(t, "")
	if _, body := e2.post("/einstellungen/benachrichtigungen", nil); !strings.Contains(body, "nicht eingerichtet") {
		t.Fatal("events without hub")
	}
	if _, body := e2.post("/einstellungen/kanal", nil); !strings.Contains(body, "nicht eingerichtet") {
		t.Fatal("channel without hub")
	}
	if _, body := e2.get("/einstellungen"); strings.Contains(body, "Telegram hinzufügen") {
		t.Fatal("card shown without hub")
	}
}

func TestDaysAgo(t *testing.T) {
	f := funcs(berlin)["daysAgo"].(func(time.Time) string)
	now := time.Now().In(berlin)
	for in, want := range map[time.Time]string{now: "heute", now.AddDate(0, 0, -1): "gestern", now.AddDate(0, 0, -5): "vor 5 Tagen"} {
		if got := f(in); got != want {
			t.Errorf("daysAgo(%v) = %q, want %q", in, got, want)
		}
	}
	if got := f(time.Date(2020, 3, 1, 12, 0, 0, 0, berlin)); got != "01.03.2020" {
		t.Errorf("old date = %q", got)
	}
}

func TestNintendoDisabled(t *testing.T) {
	e := newEnv(t, "")
	if _, body := e.get("/einstellungen/nintendo"); !strings.Contains(body, "nicht eingerichtet") {
		t.Fatal("disabled notice missing")
	}
	if _, body := e.get("/einstellungen"); !strings.Contains(body, "ZOCKERZEIT_SESSION_SECRET") {
		t.Fatal("disabled notice missing on settings page")
	}
	if _, body := e.post("/einstellungen/nintendo/login", nil); !strings.Contains(body, "nicht eingerichtet") {
		t.Fatal("login without Nintendo should fail")
	}
}
