package unifi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeController struct {
	mu       sync.Mutex
	unifiOS  bool
	logins   int
	expire   bool // reject the next authenticated request with 401
	commands []map[string]string
}

func (f *fakeController) handler(t *testing.T) http.Handler {
	prefix := ""
	loginPath := "/api/login"
	if f.unifiOS {
		prefix = "/proxy/network"
		loginPath = "/api/auth/login"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+loginPath, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "zz" || body["password"] != "secret" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.logins++
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "unifises", Value: "ok", Path: "/"})
		w.Header().Set("X-Csrf-Token", "csrf-1")
		writeOK(w, nil)
	})
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			expire := f.expire
			f.expire = false
			f.mu.Unlock()
			key := r.Header.Get("X-API-KEY")
			if c, err := r.Cookie("unifises"); (err != nil || c.Value != "ok") && key != "key-1" || expire {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	base := prefix + "/api/s/default/"
	mux.HandleFunc("GET "+base+"stat/sta", auth(func(w http.ResponseWriter, _ *http.Request) {
		writeOK(w, []map[string]any{
			{"mac": "AA:BB:CC:00:00:01", "hostname": "Nintendo-Switch", "ap_mac": "F0:9F:C2:00:00:01", "rx_bytes": 100, "tx_bytes": 50, "essid": "Kids"},
		})
	}))
	mux.HandleFunc("GET "+base+"rest/user", auth(func(w http.ResponseWriter, _ *http.Request) {
		writeOK(w, []map[string]any{
			{"mac": "aa:bb:cc:00:00:01", "name": "Switch Wohnzimmer"},
			{"mac": "aa:bb:cc:00:00:02", "hostname": "iPhone", "blocked": true},
		})
	}))
	mux.HandleFunc("GET "+base+"stat/device-basic", auth(func(w http.ResponseWriter, _ *http.Request) {
		writeOK(w, []map[string]any{{"mac": "f0:9f:c2:00:00:01", "name": "AP Kinderzimmer", "type": "uap"}})
	}))
	mux.HandleFunc("POST "+base+"cmd/stamgr", auth(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("X-API-KEY"); key == "" && r.Header.Get("X-Csrf-Token") != "csrf-1" {
			t.Errorf("missing csrf token")
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["mac"] == "de:ad:be:ef:00:00" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"meta":{"rc":"error","msg":"api.err.UnknownStation"},"data":[]}`))
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, body)
		f.mu.Unlock()
		writeOK(w, nil)
	}))
	return mux
}

func writeOK(w http.ResponseWriter, data any) {
	if data == nil {
		data = []any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"rc": "ok"}, "data": data})
}

func newTestClient(t *testing.T, f *fakeController, cfg Config) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(f.handler(t))
	t.Cleanup(srv.Close)
	cfg.URL = srv.URL + "/"
	cfg.UniFiOS = f.unifiOS
	cfg.InsecureSkipVerify = true
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassicSessionFlow(t *testing.T) {
	f := &fakeController{}
	c := newTestClient(t, f, Config{Username: "zz", Password: "secret"})
	ctx := context.Background()

	sta, err := c.Stations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sta) != 1 || sta[0].MAC != "aa:bb:cc:00:00:01" || sta[0].APMAC != "f0:9f:c2:00:00:01" || sta[0].RxBytes != 100 {
		t.Fatalf("unexpected stations: %+v", sta)
	}
	if sta[0].DisplayName() != "Nintendo-Switch" {
		t.Errorf("DisplayName = %q", sta[0].DisplayName())
	}

	known, err := c.KnownClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 2 || !known[1].Blocked || known[0].DisplayName() != "Switch Wohnzimmer" || known[1].DisplayName() != "iPhone" {
		t.Fatalf("unexpected known clients: %+v", known)
	}

	devs, err := c.Devices(ctx)
	if err != nil || len(devs) != 1 || devs[0].Name != "AP Kinderzimmer" {
		t.Fatalf("unexpected devices: %+v %v", devs, err)
	}

	if err := c.Block(ctx, "AA-BB-CC-00-00-02"); err != nil {
		t.Fatal(err)
	}
	if err := c.Unblock(ctx, "aa:bb:cc:00:00:02"); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 2 || f.commands[0]["cmd"] != "block-sta" || f.commands[0]["mac"] != "aa:bb:cc:00:00:02" || f.commands[1]["cmd"] != "unblock-sta" {
		t.Fatalf("unexpected commands: %+v", f.commands)
	}
	if f.logins != 1 {
		t.Fatalf("expected a single login, got %d", f.logins)
	}

	// Session expiry triggers exactly one re-login.
	f.expire = true
	if _, err := c.Stations(ctx); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Fatalf("expected re-login, got %d logins", f.logins)
	}

	err = c.Block(ctx, "de:ad:be:ef:00:00")
	if err == nil || !strings.Contains(err.Error(), "UnknownStation") {
		t.Fatalf("expected api error, got %v", err)
	}
}

func TestUniFiOSPaths(t *testing.T) {
	f := &fakeController{unifiOS: true}
	c := newTestClient(t, f, Config{Username: "zz", Password: "secret"})
	if _, err := c.Stations(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAPIKey(t *testing.T) {
	f := &fakeController{unifiOS: true}
	c := newTestClient(t, f, Config{APIKey: "key-1"})
	if err := c.Block(context.Background(), "aa:bb:cc:00:00:01"); err != nil {
		t.Fatal(err)
	}
	if f.logins != 0 {
		t.Fatalf("api key must not log in")
	}
}

func TestBadCredentials(t *testing.T) {
	f := &fakeController{}
	c := newTestClient(t, f, Config{Username: "zz", Password: "wrong"})
	if _, err := c.Stations(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("expected error without url")
	}
	if _, err := New(Config{URL: "https://x"}); err == nil {
		t.Error("expected error without credentials")
	}
	c, err := New(Config{URL: "https://x/", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.apiPath("stat/sta"); got != "https://x/api/s/default/stat/sta" {
		t.Errorf("apiPath = %q", got)
	}
}

func TestDisplayNameFallbacks(t *testing.T) {
	if (Station{MAC: "m"}).DisplayName() != "m" || (Station{Name: "n", Hostname: "h"}).DisplayName() != "n" {
		t.Error("Station.DisplayName fallbacks wrong")
	}
	if (KnownClient{MAC: "m"}).DisplayName() != "m" {
		t.Error("KnownClient.DisplayName fallback wrong")
	}
}
