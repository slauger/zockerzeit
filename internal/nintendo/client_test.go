package nintendo

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeNintendo emulates the account and parental controls services.
type fakeNintendo struct {
	t          *testing.T
	challenge  string
	tokenCalls atomic.Int32
	badSession bool
}

func (f *fakeNintendo) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /connect/1.0.0/api/session_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("session_token_code_verifier")))
		if r.Form.Get("client_id") != clientID || r.Form.Get("session_token_code") != "CODE" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"session_token":"SESSION"}`)
	})
	mux.HandleFunc("POST /connect/1.0.0/api/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.badSession || body["session_token"] != "SESSION" || body["grant_type"] != grantType {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"ACCESS","id_token":"ID","expires_in":900}`)
	})
	mux.HandleFunc("GET /2.0.0/users/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ACCESS" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":"acc123","nickname":"Papa"}`)
	})
	moonAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer ID" || r.Header.Get("X-Moon-App-Id") != appID || r.Header.Get("X-Moon-TimeZone") != "Europe/Berlin" {
				f.t.Errorf("missing moon headers: %v", r.Header)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("GET /v2/actions/user/fetchOwnedDevices", moonAuth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ownedDevices":[{"deviceId":"DEV1","label":"Switch 2 Wohnzimmer","platformGeneration":"P01"},{"deviceId":"DEV2","label":"Switch","platformGeneration":"P00"}]}`)
	}))
	mux.HandleFunc("GET /v2/actions/playSummary/fetchDailySummaries", moonAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deviceId") != "DEV1" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"device not found"}`)
			return
		}
		_, _ = io.WriteString(w, `{"dailySummaries":[
			{"date":"2026-11-21","playingTime":75,"players":[
				{"profile":{"playerId":"P1","nickname":"Kind A","imageUri":"https://img/a"},"playingTime":60,
				 "playedGames":[{"meta":{"applicationId":"0100A"},"playingTime":45},{"applicationId":"0100B","playingTime":15}]},
				{"profile":{"playerId":"P2","nickname":"Kind B"},"playingTime":15,"playedGames":[]}]},
			{"date":"2026-11-20","playingTime":null,"players":[]}]}`)
	}))
	return mux
}

func newFake(t *testing.T) (*fakeNintendo, *Client) {
	t.Helper()
	f := &fakeNintendo{t: t}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New("Europe/Berlin")
	c.AccountsURL, c.AccountAPIURL, c.MoonURL = srv.URL, srv.URL, srv.URL
	return f, c
}

func TestLoginAndExchange(t *testing.T) {
	f, c := newFake(t)
	login, verifier, err := c.LoginURL()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(login)
	q := u.Query()
	if q.Get("client_id") != clientID || q.Get("redirect_uri") != redirectURI || q.Get("session_token_code_challenge_method") != "S256" ||
		!strings.Contains(q.Get("scope"), "moonDailySummary") || len(verifier) != 50 {
		t.Fatalf("unexpected login url %s", login)
	}
	f.challenge = q.Get("session_token_code_challenge")

	token, err := c.ExchangeCode(context.Background(), "npf54789befb391a838://auth#session_token_code=CODE&state=x&session_state=y", verifier)
	if err != nil || token != "SESSION" {
		t.Fatalf("ExchangeCode = %q, %v", token, err)
	}
	if _, err := c.ExchangeCode(context.Background(), "npf54789befb391a838://auth#session_token_code=CODE", "wrong"); err == nil {
		t.Fatal("wrong verifier accepted")
	}
	for _, bad := range []string{"https://example.com", "npf54789befb391a838://auth#state=x", "x#%zz"} {
		if _, err := ParseRedirect(bad); err == nil {
			t.Errorf("ParseRedirect(%q) should fail", bad)
		}
	}
}

func TestAccountData(t *testing.T) {
	f, c := newFake(t)
	a := c.Account("SESSION")
	ctx := context.Background()

	id, err := a.ID(ctx)
	if err != nil || id != "acc123" {
		t.Fatalf("ID = %q, %v", id, err)
	}
	devs, err := a.Devices(ctx)
	if err != nil || len(devs) != 2 || devs[0].Model() != "Switch 2" || devs[1].Model() != "Switch" || (Device{}).Model() != "Switch" {
		t.Fatalf("Devices = %+v, %v", devs, err)
	}
	sums, err := a.DailySummaries(ctx, "DEV1")
	if err != nil || len(sums) != 2 {
		t.Fatalf("DailySummaries = %+v, %v", sums, err)
	}
	today := sums[0]
	if today.Date != "2026-11-21" || today.Minutes != 75 || len(today.Players) != 2 {
		t.Fatalf("today = %+v", today)
	}
	p := today.Players[0]
	if p.PlayerID != "P1" || p.Nickname != "Kind A" || p.Minutes != 60 || len(p.Games) != 2 || p.Games[0].ApplicationID != "0100A" || p.Games[1].ApplicationID != "0100B" {
		t.Fatalf("player = %+v", p)
	}
	if sums[1].Minutes != 0 {
		t.Fatalf("null playing time should be 0: %+v", sums[1])
	}
	if f.tokenCalls.Load() != 1 {
		t.Fatalf("token should be cached, got %d calls", f.tokenCalls.Load())
	}

	_, err = a.DailySummaries(ctx, "UNKNOWN")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound || !strings.Contains(err.Error(), "device not found") {
		t.Fatalf("expected 404 with detail, got %v", err)
	}
}

func TestInvalidSession(t *testing.T) {
	f, c := newFake(t)
	f.badSession = true
	if _, err := c.Account("SESSION").Devices(context.Background()); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expected ErrInvalidSession, got %v", err)
	}
}

func TestNoDevices(t *testing.T) {
	_, c := newFake(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_, _ = io.WriteString(w, `{"access_token":"A","id_token":"I","expires_in":900}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c.AccountsURL, c.MoonURL = srv.URL, srv.URL
	devs, err := c.Account("S").Devices(context.Background())
	if err != nil || len(devs) != 0 {
		t.Fatalf("no devices: %+v %v", devs, err)
	}
	if (&HTTPError{Status: 500}).Error() != "status 500" {
		t.Fatal("HTTPError without detail")
	}
}
