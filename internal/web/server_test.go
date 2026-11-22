package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	if err := demo.Seed(st); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(st, nil, engine.Options{Location: berlin}, log)
	h, err := New(st, eng, Options{AdminPassword: password, SessionSecret: "s3cret", Version: "test"}, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, store: st, srv: srv, client: &http.Client{Jar: jar}}
}

// nintendo adds the demo account with one console per kid.
func (e *env) nintendo() {
	e.t.Helper()
	if err := demo.SeedNintendo(e.store, time.Now().In(berlin)); err != nil {
		e.t.Fatal(err)
	}
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
	if _, body := e.get("/konsolen"); !strings.Contains(body, "Noch keine Konsolen") {
		t.Error("empty consoles page")
	}
	e.nintendo()
	for path, want := range map[string]string{
		"/":                              "Mehr Zeit für heute",
		"/konsolen":                      "Plan <strong>Schule Max</strong>",
		"/kinder":                        "Neues Kind",
		"/plaene":                        "Neuer Plan",
		"/verlauf":                       "Spielzeit pro Tag",
		"/protokoll":                     "Protokoll",
		"/protokoll?art=zeit":            "Zeit um",
		"/einstellungen":                 "Zugang ohne Passwort",
		"/kind/max":                      "Hallo Max!",
		"/kind/max/manifest.webmanifest": `"start_url":"/kind/max"`,
		"/kind/sw.js":                    "zockerzeit-kid-v2",
		"/manifest.webmanifest":          `"start_url":"/"`,
		"/static/install.js":             "Zum Home-Bildschirm",
		"/login":                         "Passwort",
		"/healthz":                       "ok",
		"/readyz":                        "ok",
	} {
		code, body := e.get(path)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: status %d, missing %q", path, code, want)
		}
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
	// Kid apps by name are for parents only.
	if code, _ := e.get("/kind/max"); code != http.StatusSeeOther {
		t.Fatalf("kid view by name: %d", code)
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

// newNintendoEnv is newEnv with a fake Nintendo integration.
func newNintendoEnv(t *testing.T) (*env, *fakeNintendo) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := demo.Seed(st); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(st, nil, engine.Options{Location: berlin}, log)
	fn := &fakeNintendo{st: st}
	h, err := New(st, eng, Options{Nintendo: fn}, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	e := &env{t: t, store: st, srv: srv, client: &http.Client{Jar: jar}}
	e.nintendo()
	return e, fn
}

func TestActions(t *testing.T) {
	e, fn := newNintendoEnv(t)
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := e.client.PostForm(e.srv.URL+"/aktion", url.Values{"action": {"bonus"}, "profile": {"max"}, "minutes": {"15"}, "back": {"/konsolen#demo-switch-2"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/konsolen?ok=") || !strings.HasSuffix(loc, "#demo-switch-2") {
		t.Fatalf("expected redirect back to the console, got %q", loc)
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
	e.client.CheckRedirect = nil
	if strings.Join(fn.extra, ",") != "max15" {
		t.Fatalf("extra time on the switch: %v", fn.extra)
	}
	// Refused by Nintendo: counted in Zockerzeit, reported.
	if _, body := e.post("/aktion", url.Values{"action": {"bonus"}, "profile": {"mia"}, "minutes": {"30"}}); !strings.Contains(body, "aber nicht an der Switch") {
		t.Fatal("refused extra time not reported")
	}

	var status struct {
		Profiles []engine.ProfileView `json:"profiles"`
	}
	_, body := e.get("/api/status")
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	for _, p := range status.Profiles {
		if p.Profile.ID == "max" && (p.Status.LimitToday != 90+15 && p.Status.LimitToday != 120+15 || len(p.Consoles) != 1 || p.Plan.ID != "schule-max") {
			t.Fatalf("max: %+v", p)
		}
	}

	ovs, _ := e.store.ActiveOverrides(time.Now(), berlin)
	if len(ovs) != 2 {
		t.Fatalf("expected 2 overrides, got %d", len(ovs))
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
		{"action": {"bonus"}, "profile": {"max"}, "minutes": {"0"}},
		{"action": {"bonus"}, "minutes": {"10"}},
	} {
		if _, body := e.post("/aktion", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestPlanSwitch(t *testing.T) {
	e, fn := newNintendoEnv(t)
	now := time.Now().In(berlin)
	if _, body := e.post("/aktion", url.Values{"action": {"sperren"}, "profile": {"max"}}); !strings.Contains(body, "Max: Gesperrt bis") {
		t.Fatal("lock failed")
	}
	if fn.applied == 0 {
		t.Fatal("the plan was not written to the switch")
	}
	ovs, _ := e.store.ActiveOverrides(now, berlin)
	if len(ovs) != 1 || ovs[0].PlanID != store.PlanLocked || !ovs[0].Until.After(now) || ovs[0].Until.After(now.AddDate(0, 0, 2)) {
		t.Fatalf("overrides = %+v", ovs)
	}
	_, body := e.get("/")
	if strings.Contains(body, "disabled") || !strings.Contains(body, `<option value="gesperrt">Gesperrt (gilt gerade)</option>`) || !strings.Contains(body, `<option value="ferien" selected>Ferien</option>`) {
		t.Fatal("plan choice: the plan in force is marked, the first other one picked")
	}
	if !strings.Contains(body, "Plan <strong>Gesperrt</strong> bis") || !strings.Contains(body, "Zurück zum Standardplan") {
		t.Fatal("locked plan missing on the dashboard")
	}
	// Frei replaces it, for all kids, including a date.
	date := now.AddDate(0, 0, 3).Format("2006-01-02")
	if _, body := e.post("/aktion", url.Values{"action": {"plan"}, "plan": {store.PlanFree}, "profile": {"alle"}, "dauer": {"datum"}, "bis": {date}}); !strings.Contains(body, "Max und Mia: Frei bis") {
		t.Fatal("holiday for all kids failed")
	}
	ovs, _ = e.store.ActiveOverrides(now, berlin)
	if len(ovs) != 2 || ovs[0].PlanID != store.PlanFree || !ovs[0].Until.After(now.AddDate(0, 0, 3)) {
		t.Fatalf("overrides = %+v", ovs)
	}
	if _, body := e.get("/kind/max"); !strings.Contains(body, `<p class="kid-big">Frei</p>`) {
		t.Fatal("free state missing in the kid app")
	}
	// For good: the new standard plan.
	if _, body := e.post("/aktion", url.Values{"action": {"plan"}, "plan": {"ferien"}, "profile": {"mia"}, "dauer": {"dauerhaft"}}); !strings.Contains(body, "ist jetzt der Standardplan") {
		t.Fatal("permanent switch failed")
	}
	if p, _ := e.store.Profile("mia"); p.PlanID != "ferien" {
		t.Fatalf("standard plan = %s", p.PlanID)
	}
	for _, bad := range []url.Values{
		{"action": {"plan"}, "plan": {"ferien"}, "profile": {"niemand"}, "dauer": {"morgen"}},
		{"action": {"plan"}, "plan": {"nope"}, "profile": {"max"}, "dauer": {"morgen"}},
		{"action": {"plan"}, "plan": {"ferien"}, "profile": {"max"}},
		{"action": {"plan"}, "plan": {store.PlanLocked}, "profile": {"max"}, "dauer": {"dauerhaft"}},
		{"action": {"plan"}, "plan": {"ferien"}, "profile": {"max"}, "dauer": {"datum"}, "bis": {"2000-01-01"}},
		{"action": {"plan"}, "plan": {"ferien"}, "profile": {"max"}, "dauer": {"datum"}, "bis": {"kaputt"}},
	} {
		if _, body := e.post("/aktion", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}

	// The morning is the start of the time window, after midnight today's.
	plan, _ := e.store.Plan("schule-max")
	monday := time.Date(2026, 11, 23, 0, 0, 0, 0, berlin)
	if got := nextMorning(plan, monday.Add(time.Hour)); !got.Equal(monday.Add(9 * time.Hour)) {
		t.Fatalf("after midnight = %v", got)
	}
	if got := nextMorning(plan, monday.Add(13*time.Hour)); !got.Equal(monday.AddDate(0, 0, 1).Add(9 * time.Hour)) {
		t.Fatalf("afternoon = %v", got)
	}
	free, _ := e.store.Plan(store.PlanFree)
	if got := morningOf(free, monday); !got.Equal(monday) {
		t.Fatalf("whole day = %v", got)
	}
}

func TestKidsPlansConsoles(t *testing.T) {
	e := newEnv(t, "")
	form := url.Values{"name": {"Wochenende Oma"}}
	for _, d := range []string{"0", "1", "2", "3", "4", "5", "6"} {
		form.Set("from_"+d, "08:00")
		form.Set("to_"+d, "21:00")
		form.Set("limit_"+d, "120")
	}
	form.Set("from_0", "")
	form.Set("to_0", "")
	form.Set("limit_0", "")
	form.Set("to_6", "24:00")
	form.Set("limit_3", "0")
	if _, body := e.post("/plaene/plan", form); !strings.Contains(body, "Plan Wochenende Oma gespeichert") {
		t.Fatal("saving the plan failed")
	}
	p, err := e.store.Plan("wochenende-oma")
	if err != nil || p.Day(time.Monday).From != 8*60 || p.Day(time.Sunday).Limit != -1 || p.Day(time.Saturday).To != 24*60 || p.Day(time.Wednesday).Limit != 0 {
		t.Fatalf("plan = %+v %v", p, err)
	}
	if _, body := e.post("/plaene/plan", form); !strings.Contains(body, "gibt es schon") {
		t.Fatal("duplicate plan accepted")
	}
	for name, change := range map[string][2]string{
		"start too late": {"from_1", "10:00"},
		"end too early":  {"to_1", "15:00"},
		"end too late":   {"to_1", "23:30"},
		"no end":         {"to_1", ""},
		"bad limit":      {"limit_1", "400"},
		"bad clock":      {"from_1", "acht"},
	} {
		bad := url.Values{}
		maps.Copy(bad, form)
		bad.Set("name", "X "+name)
		bad.Set(change[0], change[1])
		if _, body := e.post("/plaene/plan", bad); !strings.Contains(body, "banner err") {
			t.Errorf("%s accepted", name)
		}
	}
	if _, body := e.post("/plaene/plan", url.Values{"name": {""}}); !strings.Contains(body, "Namen") {
		t.Fatal("plan without name accepted")
	}
	if _, body := e.get("/plaene"); !strings.Contains(body, "Standardplan für Max") || !strings.Contains(body, `value="24:00"`) {
		t.Fatal("plans page")
	}

	// Kids get a standard plan.
	if _, body := e.post("/kinder", url.Values{"name": {"Großer Bruder"}, "plan": {"wochenende-oma"}}); !strings.Contains(body, "Großer Bruder gespeichert") {
		t.Fatal("saving the kid failed")
	}
	if k, err := e.store.Profile("grosser-bruder"); err != nil || k.PlanID != "wochenende-oma" {
		t.Fatalf("kid = %+v %v", k, err)
	}
	for _, bad := range []url.Values{
		{"name": {""}},
		{"name": {"Großer Bruder"}, "plan": {"wochenende-oma"}},
		{"name": {"Y"}, "plan": {store.PlanFree}},
		{"name": {"Z"}, "plan": {"nope"}},
	} {
		if _, body := e.post("/kinder", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}
	if _, body := e.post("/plaene/plan/wochenende-oma/loeschen", nil); !strings.Contains(body, "noch Standardplan") {
		t.Fatal("plan in use deleted")
	}

	// Handing a console to another kid moves its play time, past included.
	e.nintendo()
	before, _ := e.store.NintendoMinutes("max", "2000-01-01", "2100-01-01")
	if _, body := e.post("/konsolen/demo-switch-1", url.Values{"name": {"Switch Oma"}, "profile": {"grosser-bruder"}}); !strings.Contains(body, "Switch Oma gehört jetzt Großer Bruder") {
		t.Fatal("changing the owner failed")
	}
	moved, _ := e.store.NintendoMinutes("grosser-bruder", "2000-01-01", "2100-01-01")
	after, _ := e.store.NintendoMinutes("max", "2000-01-01", "2100-01-01")
	if moved == 0 || after >= before {
		t.Fatalf("play time not moved: before=%d after=%d moved=%d", before, after, moved)
	}
	if _, body := e.get("/kinder"); !strings.Contains(body, "Switch Oma") || !strings.Contains(body, "Spiele der letzten 30 Tage") {
		t.Fatal("kids page misses consoles or games")
	}
	if _, body := e.post("/konsolen/demo-switch-1", url.Values{"name": {"Switch Oma"}, "profile": {"grosser-bruder"}}); !strings.Contains(body, "Switch Oma gespeichert") {
		t.Fatal("renaming failed")
	}
	if _, body := e.post("/konsolen/demo-switch-1", url.Values{"name": {""}, "profile": {""}}); !strings.Contains(body, "gehört jetzt niemandem") {
		t.Fatal("removing the owner failed")
	}
	if _, body := e.get("/"); !strings.Contains(body, "noch niemandem") {
		t.Fatal("unowned console not shown on the dashboard")
	}
	for path, form := range map[string]url.Values{
		"/konsolen/nope":          {"profile": {"max"}},
		"/konsolen/demo-switch-1": {"profile": {"niemand"}},
	} {
		if _, body := e.post(path, form); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %s %v", path, form)
		}
	}

	if _, body := e.post("/kinder/grosser-bruder/loeschen", nil); !strings.Contains(body, "Kind gelöscht") {
		t.Fatal("deleting the kid failed")
	}
	if _, body := e.post("/plaene/plan/wochenende-oma/loeschen", nil); !strings.Contains(body, "Plan gelöscht") {
		t.Fatal("deleting the plan failed")
	}
	if _, body := e.post("/plaene/plan/wochenende-oma/loeschen", nil); !strings.Contains(body, "nicht gefunden") {
		t.Fatal("deleting twice")
	}
	if code, _ := e.get("/regeln"); code != http.StatusOK {
		t.Fatalf("old rules link: %d", code)
	}
}

func TestStatsAndKidAPI(t *testing.T) {
	e := newEnv(t, "")
	e.nintendo()
	code, body := e.get("/api/stats?profile=max&days=7")
	if code != http.StatusOK {
		t.Fatalf("stats: %d", code)
	}
	var st statsResponse
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if (st.Limit != 90 && st.Limit != 120) || len(st.Daily.Labels) != 7 || len(st.Consoles) == 0 || st.Consoles[0].Name != "Switch Kinderzimmer" {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if code, _ := e.get("/api/stats?profile=niemand&days=abc"); code != http.StatusOK {
		t.Fatalf("stats with bad params: %d", code)
	}

	code, body = e.get("/api/kind/max")
	if code != http.StatusOK || !strings.Contains(body, `"name":"Max"`) || !strings.Contains(body, `"limit":`) {
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
}

type fakeNintendo struct {
	st      *store.Store
	synced  int
	applied int
	removed int64
	extra   []string
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

func (f *fakeNintendo) Apply(context.Context, bool) { f.applied++ }

func (f *fakeNintendo) AddExtraTime(_ context.Context, profileID string, minutes int) error {
	if minutes == 30 {
		return errors.New("refused")
	}
	f.extra = append(f.extra, fmt.Sprint(profileID, minutes))
	return nil
}

func TestNintendoPages(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := demo.Seed(st); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(st, nil, engine.Options{Location: berlin}, log)
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
	if _, body := e.get("/"); !strings.Contains(body, "<strong>Switch 2</strong> gehört noch niemandem") {
		t.Fatal("unowned console not shown")
	}
	if _, body := e.post("/konsolen/D1", url.Values{"name": {"Switch Kinderzimmer"}, "profile": {"max"}}); !strings.Contains(body, "gehört jetzt Max") {
		t.Fatal("assigning the console failed")
	}
	_, body := e.get("/einstellungen/nintendo")
	if !strings.Contains(body, "Spieler A") || !strings.Contains(body, "42 Min") || !strings.Contains(body, "gehört <strong>Max</strong>") {
		t.Fatal("players, usage or owner missing on the page")
	}
	if _, body := e.get("/"); !strings.Contains(body, "Heute gespielt: Switch Kinderzimmer <strong>42 Min</strong>") {
		t.Fatal("dashboard Nintendo line missing")
	}
	if _, body := e.get("/konsolen"); !strings.Contains(body, "Heute <strong>42 Min</strong>: Unbekanntes Spiel 42 Min") {
		t.Fatal("console usage missing")
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
	if _, body := e.get("/verlauf?profil=max"); !strings.Contains(body, "die Switches zuordnen") {
		t.Fatal("hint without Nintendo missing")
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
	// A console without owner and an unknown game.
	_ = e.store.UpsertNintendoDevice(store.NintendoDevice{DeviceID: "papa-switch", Account: 1, Label: "Switch Papa"})
	_ = e.store.UpsertNintendoUsage(store.NintendoUsage{Date: now.AddDate(0, 0, -20).Format("2006-01-02"), DeviceID: "papa-switch", PlayerID: "papa", Minutes: 9,
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
	var sent []string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botTOKEN/getMe":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"zz_bot"}}`)
		case "/botTOKEN/sendMessage":
			b, _ := io.ReadAll(r.Body)
			sent = append(sent, string(b))
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
		}
	}))
	t.Cleanup(tg.Close)
	hub := notify.NewHub(st, box, nil, log)
	hub.NewBot = func(token string) *notify.Bot {
		b := notify.NewBot(token)
		b.BaseURL = tg.URL
		return b
	}
	eng := engine.New(st, hub, engine.Options{Location: berlin}, log)
	h, err := New(st, eng, Options{Notify: hub}, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	e := &env{t: t, store: st, srv: srv, client: &http.Client{Jar: jar}}

	if _, body := e.get("/einstellungen"); !strings.Contains(body, "Bot einrichten") || strings.Contains(body, "Mich eintragen") {
		t.Fatal("bot setup missing")
	}
	if _, body := e.post("/einstellungen/telegram", url.Values{"token": {"FALSCH"}}); !strings.Contains(body, "kennt dieses Token nicht") {
		t.Fatal("wrong token accepted")
	}
	if _, body := e.post("/einstellungen/telegram", url.Values{"token": {"TOKEN"}}); !strings.Contains(body, "@zz_bot eingerichtet") {
		t.Fatal("bot setup failed")
	}
	if _, body := e.post("/einstellungen/empfaenger", url.Values{"name": {""}}); !strings.Contains(body, "Namen") {
		t.Fatal("recipient without name accepted")
	}
	_, body := e.post("/einstellungen/empfaenger", url.Values{"name": {"Papa"}, "event_warn": {"on"}, "event_over": {"on"}})
	if !strings.Contains(body, "https://t.me/zz_bot?start=") || !strings.Contains(body, "wartet auf Start") {
		t.Fatal("connect link missing")
	}
	rs, _ := st.NotifyRecipients()
	id := strconv.FormatInt(rs[0].ID, 10)
	if _, body := e.post("/einstellungen/empfaenger/"+id+"/test", nil); !strings.Contains(body, "noch nicht auf Start") {
		t.Fatal("test before connecting")
	}
	if _, err := st.ConnectNotifyRecipient(rs[0].Code, "42"); err != nil {
		t.Fatal(err)
	}
	if _, body := e.post("/einstellungen/empfaenger/"+id, url.Values{"name": {"Papa"}, "event_start": {"on"}}); !strings.Contains(body, "Benachrichtigungen gespeichert") || !strings.Contains(body, "verbunden") {
		t.Fatal("saving events failed")
	}
	if rs, _ := st.NotifyRecipients(); strings.Join(rs[0].Events, ",") != "start" {
		t.Fatalf("events = %v", rs[0].Events)
	}
	if _, body := e.post("/einstellungen/empfaenger/"+id+"/test", nil); !strings.Contains(body, "Testnachricht verschickt") || len(sent) != 1 || !strings.Contains(sent[0], `"chat_id":"42"`) {
		t.Fatalf("test message failed: %v", sent)
	}
	for _, path := range []string{"/einstellungen/empfaenger/x", "/einstellungen/empfaenger/x/test", "/einstellungen/empfaenger/x/entfernen"} {
		if _, body := e.post(path, url.Values{"name": {"x"}}); !strings.Contains(body, "unbekannter Empfänger") {
			t.Errorf("%s: bad id accepted", path)
		}
	}
	if _, body := e.post("/einstellungen/empfaenger/"+id+"/entfernen", nil); !strings.Contains(body, "Empfänger entfernt") {
		t.Fatal("removing failed")
	}
	if _, body := e.post("/einstellungen/empfaenger/"+id+"/entfernen", nil); !strings.Contains(body, "banner err") {
		t.Fatal("removing twice should fail")
	}
	if _, body := e.post("/einstellungen/telegram/entfernen", nil); !strings.Contains(body, "Bot entfernt") || !strings.Contains(body, "Bot einrichten") {
		t.Fatal("removing the bot failed")
	}

	// Without the hub the routes refuse.
	e2 := newEnv(t, "")
	for _, path := range []string{"/einstellungen/telegram", "/einstellungen/empfaenger"} {
		if _, body := e2.post(path, nil); !strings.Contains(body, "nicht eingerichtet") {
			t.Errorf("%s without hub", path)
		}
	}
	if _, body := e2.get("/einstellungen"); strings.Contains(body, "Bot einrichten") {
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

func TestKidStats(t *testing.T) {
	e := newEnv(t, "pw")
	if _, err := e.store.AddAccessLink(store.AccessLink{Kind: store.LinkKid, ProfileID: "max", Label: "iPad", Created: time.Now()}, "kidtoken"); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get("/k/kidtoken"); strings.Contains(body, "Meine Statistik") || !strings.Contains(body, "Hallo Max!") {
		t.Fatal("stats link without Nintendo")
	}
	now := time.Now().In(berlin)
	if err := demo.SeedNintendo(e.store, now); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get("/k/kidtoken"); strings.Contains(body, "Zurück zur Elternansicht") {
		t.Fatal("the kid app must not lead to the parents")
	}
	if _, body := e.get("/k/kidtoken"); !strings.Contains(body, `href="/k/kidtoken/statistik"`) || !strings.Contains(body, `href="/k/kidtoken/manifest.webmanifest"`) || strings.Contains(body, "So viel hast du gezockt") {
		t.Fatal("the main page links to the statistics instead of showing them")
	}
	if code, _ := e.get("/k/falsch/statistik"); code != http.StatusNotFound {
		t.Fatalf("statistics of an unknown kid: %d", code)
	}
	cases := map[string][]string{
		"":                  {"So viel hast du gezockt", `href="/k/kidtoken"`, `href="?zeitraum=heute" aria-current="page"`, "Inselabenteuer"},
		"?zeitraum=7":       {`href="?zeitraum=7" aria-current="page"`, "Pro Tag", "kid-bar", "kid-limit", "zuletzt heute"},
		"?zeitraum=30":      {`href="?zeitraum=30" aria-current="page"`, "is-dense"},
		"?zeitraum=alle":    {`href="?zeitraum=alle" aria-current="page"`, "Woche ab"},
		"?zeitraum=quatsch": {`href="?zeitraum=heute" aria-current="page"`},
	}
	for q, wants := range cases {
		code, body := e.get("/k/kidtoken/statistik" + q)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", q, code)
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s misses %q", q, want)
			}
		}
	}

	// Long history is shown per month, at most a year.
	old := now.AddDate(-2, 0, 0).Format("2006-01-02")
	_ = e.store.UpsertNintendoUsage(store.NintendoUsage{Date: old, DeviceID: "demo-switch-1", PlayerID: "demo-max", Minutes: 30,
		Games: []store.NintendoGame{{ApplicationID: "0100DEMO0001", Minutes: 30}}})
	_ = e.store.AddNintendoPlay([]store.NintendoPlay{{Date: old, DeviceID: "demo-switch-1", PlayerID: "demo-max", ProfileID: "max", Minutes: 30, TS: now}})
	_, body := e.get("/k/kidtoken/statistik?zeitraum=alle")
	if got := strings.Count(body, `class="kid-bar`); got != maxMonthBars || !strings.Contains(body, monthNames[now.Month()-1]+" ") {
		t.Fatalf("month bars: %d", got)
	}

	// Covers are served to the kid app without a parent session.
	src := "https://example.invalid/cover.png"
	if err := e.store.SaveImage(store.Image{Source: src, ContentType: "image/png", Data: []byte("png"), Fetched: now}); err != nil {
		t.Fatal(err)
	}
	key := store.ImageKey(src)
	if code, body := e.get("/k/kidtoken/bild/" + key); code != http.StatusOK || body != "png" {
		t.Fatalf("kid image: %d", code)
	}
	if _, body := e.get("/k/kidtoken/manifest.webmanifest"); !strings.Contains(body, `"start_url":"/k/kidtoken"`) {
		t.Fatalf("kid manifest: %s", body)
	}
	if code, body := e.get("/k/kidtoken/status"); code != http.StatusOK || !strings.Contains(body, `"name":"Max"`) {
		t.Fatalf("kid status: %d %s", code, body)
	}
	if code, _ := e.get("/k/falsch/bild/" + key); code != http.StatusNotFound {
		t.Fatalf("kid image of unknown kid: %d", code)
	}
	if _, body := e.get("/bild/" + key); body == "png" {
		t.Fatal("parent image route open without login")
	}
}

func TestKidPreview(t *testing.T) {
	e := newEnv(t, "")
	for _, path := range []string{"/kind/max", "/kind/max/statistik"} {
		if _, body := e.get(path); !strings.Contains(body, `href="/kinder#kind-max"`) {
			t.Errorf("%s: no way back for parents", path)
		}
	}
}

func TestKidWindow(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, berlin) // a Monday
	p := store.Plan{Days: map[time.Weekday]store.PlanDay{
		time.Monday:  {From: 9 * 60, To: 24 * 60, Limit: 60},
		time.Tuesday: {Limit: -1},
	}}
	if got := kidWindow(p, now); got != "09:00 bis 24:00 Uhr" {
		t.Fatalf("window %q", got)
	}
	if got := kidWindow(p, now.AddDate(0, 0, 1)); got != "" {
		t.Fatalf("whole day %q", got)
	}
	if got := kidWindow(p, now.AddDate(0, 0, 2)); got != "keine Zockzeit" {
		t.Fatalf("no play %q", got)
	}
}

func TestAccessLinks(t *testing.T) {
	e := newEnv(t, "geheim")
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	login := func(pw string) string {
		t.Helper()
		resp, err := e.client.PostForm(e.srv.URL+"/login", url.Values{"password": {pw}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if loc := login("geheim"); loc != "/" {
		t.Fatalf("login: %s", loc)
	}
	e.client.CheckRedirect = nil

	// A parent link for a phone, shown once.
	_, body := e.post("/einstellungen/zugang", url.Values{"kind": {"parent"}, "label": {"iPhone Papa"}})
	m := regexp.MustCompile(`value="(http://[^"]+/zugang/([A-Za-z0-9_-]+))"`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, "Elternzugang für „iPhone Papa“") {
		t.Fatal("new parent link not shown")
	}
	parentToken := m[2]
	_, body = e.post("/einstellungen/zugang", url.Values{"kind": {"kid"}, "profile": {"mia"}, "label": {"Tablet"}})
	k := regexp.MustCompile(`value="http://[^"]+/k/([A-Za-z0-9_-]+)"`).FindStringSubmatch(body)
	if k == nil || !strings.Contains(body, "Kinder-App für Mia auf „Tablet“") {
		t.Fatal("new kid link not shown")
	}
	if _, body := e.get("/k/" + k[1]); !strings.Contains(body, "Hallo Mia!") {
		t.Fatal("kid link does not open the kid app")
	}
	for _, bad := range []url.Values{
		{"kind": {"parent"}},
		{"kind": {"kid"}, "profile": {"niemand"}, "label": {"x"}},
		{"kind": {"admin"}, "label": {"x"}},
	} {
		if _, body := e.post("/einstellungen/zugang", bad); !strings.Contains(body, "banner err") {
			t.Errorf("expected error for %v", bad)
		}
	}

	// A new device with only the link is signed in, also after a restart.
	jar, _ := cookiejar.New(nil)
	phone := &env{t: t, store: e.store, srv: e.srv, client: &http.Client{Jar: jar}}
	if code, body := phone.get("/zugang/" + parentToken); code != http.StatusOK || !strings.Contains(body, "Mehr Zeit für heute") {
		t.Fatalf("link sign in: %d", code)
	}
	if _, body := phone.get("/"); !strings.Contains(body, "/manifest.webmanifest?zugang="+parentToken) {
		t.Fatal("the installed app would not start with its link")
	}
	if _, body := phone.get("/manifest.webmanifest?zugang=" + parentToken); !strings.Contains(body, `"start_url":"/zugang/`+parentToken+`"`) {
		t.Fatalf("manifest: %s", body)
	}
	if _, body := phone.get("/manifest.webmanifest?zugang=falsch"); !strings.Contains(body, `"start_url":"/"`) {
		t.Fatalf("manifest with a wrong token: %s", body)
	}
	if _, body := e.get("/einstellungen"); !strings.Contains(body, "iPhone Papa") || !strings.Contains(body, "zuletzt") {
		t.Fatal("link list missing")
	}

	// Revoking ends the access right away.
	links, _ := e.store.AccessLinks()
	for _, l := range links {
		if _, body := e.post("/einstellungen/zugang/"+strconv.FormatInt(l.ID, 10)+"/entfernen", nil); !strings.Contains(body, "gesperrt") {
			t.Fatal("revoking failed")
		}
	}
	if _, body := e.post("/einstellungen/zugang/abc/entfernen", nil); !strings.Contains(body, "nicht gefunden") {
		t.Fatal("bad id accepted")
	}
	if _, body := phone.get("/"); !strings.Contains(body, "Passwort") {
		t.Fatal("revoked link still signed in")
	}
	if _, body := phone.get("/zugang/" + parentToken); !strings.Contains(body, "gilt nicht mehr") {
		t.Fatal("revoked link not reported")
	}
	if code, _ := phone.get("/k/" + k[1]); code != http.StatusNotFound {
		t.Fatal("revoked kid link still works")
	}

	// Posts from other sites are refused, own forms (Origin null because of
	// no-referrer) pass.
	for _, c := range []struct {
		header, value string
		want          int
	}{
		{"Origin", "https://evil.example", http.StatusForbidden},
		{"Sec-Fetch-Site", "cross-site", http.StatusForbidden},
		{"Sec-Fetch-Site", "same-origin", http.StatusOK},
		{"Origin", "null", http.StatusOK},
	} {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/aktion", strings.NewReader("action=bonus&profile=max&minutes=5"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set(c.header, c.value)
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("%s %s: %d", c.header, c.value, resp.StatusCode)
		}
	}

	// Logging out drops both cookies.
	if _, body := phone.post("/logout", nil); !strings.Contains(body, "Passwort") {
		t.Fatal("logout")
	}
}

func TestLoginThrottle(t *testing.T) {
	e := newEnv(t, "geheim")
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := e.srv.Config.Handler.(*Server)
	for range loginMaxFails {
		s.limiter.fail("127.0.0.1", time.Now())
	}
	resp, err := e.client.PostForm(e.srv.URL+"/login", url.Values{"password": {"geheim"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/login?gesperrt=1" {
		t.Fatalf("not throttled: %s", loc)
	}
	if _, body := e.get("/login?gesperrt=1"); !strings.Contains(body, "Zu viele Fehlversuche") {
		t.Fatal("throttle message missing")
	}
	if s.limiter.blocked("127.0.0.1", time.Now().Add(loginWindow)) {
		t.Fatal("block does not expire")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.9")
	if clientIP(r) != "10.0.0.9" {
		t.Fatalf("client ip = %s", clientIP(r))
	}
}
