// Package nintendo reads play time from the Nintendo Switch Parental Controls
// service. The API is not documented by Nintendo; endpoints, headers and the
// login flow follow the community library pynintendoparental.
package nintendo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	clientID    = "54789befb391a838"
	redirectURI = "npf54789befb391a838://auth"
	grantType   = "urn:ietf:params:oauth:grant-type:jwt-bearer-session-token"

	appID      = "com.nintendo.znma"
	appVersion = "2.6.0"
	appBuild   = "700"
	osName     = "ANDROID"
	osVersion  = "34"
	model      = "Pixel 4 XL"
)

var scopes = []string{
	"openid", "user", "moonUser:administration", "moonDevice:create", "moonOwnedDevice:administration",
	"moonParentalControlSetting", "moonParentalControlSetting:update", "moonParentalControlSettingState",
	"moonPairingState", "moonSmartDevice:administration", "moonDailySummary", "moonMonthlySummary",
}

// ErrInvalidSession means the stored session token was rejected and the
// account has to be connected again.
var ErrInvalidSession = errors.New("nintendo: session token rejected, please connect the account again")

// Client talks to the Nintendo account and parental controls services.
type Client struct {
	HTTP *http.Client
	// Base URLs, overridable for tests.
	AccountsURL   string
	AccountAPIURL string
	MoonURL       string
	// TimeZone and Language are sent to the parental controls service and
	// decide which calendar day a summary belongs to.
	TimeZone string
	Language string

	allowHTTPImages bool // tests only
}

// New creates a client for the given IANA time zone.
func New(timeZone string) *Client {
	return &Client{
		HTTP:          &http.Client{Timeout: 20 * time.Second},
		AccountsURL:   "https://accounts.nintendo.com",
		AccountAPIURL: "https://api.accounts.nintendo.com",
		MoonURL:       "https://app.lp1.znma.srv.nintendo.net",
		TimeZone:      timeZone,
		Language:      "de-DE",
	}
}

// LoginURL returns the Nintendo login URL and the PKCE verifier that has to
// be kept for ExchangeCode.
func (c *Client) LoginURL() (string, string, error) {
	verifier, err := randomLetters(50)
	if err != nil {
		return "", "", err
	}
	state, err := randomLetters(50)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":                           {clientID},
		"redirect_uri":                        {redirectURI},
		"response_type":                       {"session_token_code"},
		"scope":                               {strings.Join(scopes, " ")},
		"session_token_code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"session_token_code_challenge_method": {"S256"},
		"state":                               {state},
		"theme":                               {"login_form"},
	}
	return c.AccountsURL + "/connect/1.0.0/authorize?" + q.Encode(), verifier, nil
}

// ParseRedirect extracts the session token code from the redirect URL
// (npf54789befb391a838://auth#session_token_code=...).
func ParseRedirect(redirect string) (string, error) {
	redirect = strings.TrimSpace(redirect)
	_, fragment, ok := strings.Cut(redirect, "#")
	if !ok {
		return "", errors.New("nintendo: the URL contains no session_token_code, copy the complete link of the button")
	}
	vals, err := url.ParseQuery(fragment)
	if err != nil {
		return "", fmt.Errorf("nintendo: invalid redirect URL: %w", err)
	}
	code := vals.Get("session_token_code")
	if code == "" {
		return "", errors.New("nintendo: the URL contains no session_token_code, copy the complete link of the button")
	}
	return code, nil
}

// ExchangeCode turns the redirect URL of a login into a long lived session
// token.
func (c *Client) ExchangeCode(ctx context.Context, redirect, verifier string) (string, error) {
	code, err := ParseRedirect(redirect)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"client_id":                   {clientID},
		"session_token_code":          {code},
		"session_token_code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AccountsURL+"/connect/1.0.0/api/session_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		SessionToken string `json:"session_token"`
	}
	if err := c.do(req, &out); err != nil {
		return "", fmt.Errorf("nintendo session token: %w", err)
	}
	if out.SessionToken == "" {
		return "", errors.New("nintendo: no session token in the response")
	}
	return out.SessionToken, nil
}

// Account is a connected Nintendo account.
type Account struct {
	c            *Client
	sessionToken string

	mu          sync.Mutex
	idToken     string
	accessToken string
	expiry      time.Time
}

// Account returns an account handle for a session token.
func (c *Client) Account(sessionToken string) *Account {
	return &Account{c: c, sessionToken: sessionToken}
}

func (a *Account) token(ctx context.Context) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.idToken != "" && time.Until(a.expiry) > 5*time.Minute {
		return a.idToken, a.accessToken, nil
	}
	body, _ := json.Marshal(map[string]string{"client_id": clientID, "grant_type": grantType, "session_token": a.sessionToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.c.AccountsURL+"/connect/1.0.0/api/token", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := a.c.do(req, &out); err != nil {
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusUnauthorized) {
			return "", "", ErrInvalidSession
		}
		return "", "", fmt.Errorf("nintendo token: %w", err)
	}
	a.idToken, a.accessToken = out.IDToken, out.AccessToken
	a.expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return a.idToken, a.accessToken, nil
}

// ID returns the Nintendo account id.
func (a *Account) ID(ctx context.Context) (string, error) {
	_, access, err := a.token(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.c.AccountAPIURL+"/2.0.0/users/me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	var out struct {
		ID       string `json:"id"`
		Nickname string `json:"nickname"`
	}
	if err := a.c.do(req, &out); err != nil {
		return "", fmt.Errorf("nintendo account: %w", err)
	}
	return out.ID, nil
}

// Device is a console linked to the parental controls of an account.
type Device struct {
	ID    string `json:"deviceId"`
	Label string `json:"label"`
	// Generation is "P00" for Switch and "P01" for Switch 2.
	Generation string `json:"platformGeneration"`
}

// Model returns the console model.
func (d Device) Model() string {
	switch d.Generation {
	case "P00":
		return "Switch"
	case "P01":
		return "Switch 2"
	}
	return "Switch"
}

// Devices lists the consoles of the account.
func (a *Account) Devices(ctx context.Context) ([]Device, error) {
	var out struct {
		OwnedDevices []Device `json:"ownedDevices"`
	}
	err := a.moon(ctx, "/v2/actions/user/fetchOwnedDevices", &out)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return nil, nil
	}
	return out.OwnedDevices, err
}

// Game is the play time of one application. Title and ImageURI are only
// set when the summary carries them.
type Game struct {
	ApplicationID string
	Title         string
	ImageURI      string
	Minutes       int
}

// PlayerDay is the play time of one user of a console on one day.
type PlayerDay struct {
	PlayerID string
	Nickname string
	ImageURI string
	Minutes  int
	Games    []Game
}

// DailySummary is the play time of a console on one day.
type DailySummary struct {
	// Date is the calendar day in the client's time zone (YYYY-MM-DD).
	Date    string
	Minutes int
	Players []PlayerDay
}

// DailySummaries returns the recent daily summaries of a console, newest first.
func (a *Account) DailySummaries(ctx context.Context, deviceID string) ([]DailySummary, error) {
	var raw struct {
		DailySummaries []struct {
			Date        string `json:"date"`
			PlayingTime *int   `json:"playingTime"`
			Players     []struct {
				Profile struct {
					PlayerID string `json:"playerId"`
					Nickname string `json:"nickname"`
					ImageURI string `json:"imageUri"`
				} `json:"profile"`
				PlayingTime *int `json:"playingTime"`
				PlayedGames []struct {
					appMeta
					Meta        appMeta `json:"meta"`
					PlayingTime *int    `json:"playingTime"`
				} `json:"playedGames"`
			} `json:"players"`
		} `json:"dailySummaries"`
	}
	if err := a.moon(ctx, "/v2/actions/playSummary/fetchDailySummaries?deviceId="+url.QueryEscape(deviceID), &raw); err != nil {
		return nil, err
	}
	out := make([]DailySummary, 0, len(raw.DailySummaries))
	for _, d := range raw.DailySummaries {
		ds := DailySummary{Date: d.Date, Minutes: deref(d.PlayingTime)}
		for _, p := range d.Players {
			pd := PlayerDay{PlayerID: p.Profile.PlayerID, Nickname: p.Profile.Nickname, ImageURI: p.Profile.ImageURI, Minutes: deref(p.PlayingTime)}
			for _, g := range p.PlayedGames {
				// Switch summaries carry the fields directly, Switch 2 summaries in "meta".
				m := g.appMeta
				if m.ApplicationID == "" {
					m = g.Meta
				}
				pd.Games = append(pd.Games, Game{ApplicationID: m.ApplicationID, Title: m.Title, ImageURI: string(m.ImageURI), Minutes: deref(g.PlayingTime)})
			}
			ds.Players = append(ds.Players, pd)
		}
		out = append(out, ds)
	}
	return out, nil
}

type appMeta struct {
	ApplicationID string   `json:"applicationId"`
	Title         string   `json:"title"`
	ImageURI      imageURI `json:"imageUri"`
}

// imageURI is either a plain URL or an object with one URL per size.
type imageURI string

func (u *imageURI) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*u = imageURI(s)
		return nil
	}
	// Anything else than a string or an object carries no usable URL.
	var sizes map[string]any
	_ = json.Unmarshal(b, &sizes)
	keys := make([]string, 0, len(sizes))
	for k := range sizes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	keys = append([]string{"medium", "large", "small"}, keys...)
	for _, k := range keys {
		if v, ok := sizes[k].(string); ok && strings.HasPrefix(v, "http") {
			*u = imageURI(v)
			return nil
		}
	}
	*u = ""
	return nil
}

// Title is the name and cover image of an application.
type Title struct {
	ApplicationID string
	Name          string
	ImageURI      string
}

// Titles returns the applications known to a console. They come from the
// parental control settings, which list every game installed or played.
func (a *Account) Titles(ctx context.Context, deviceID string) ([]Title, error) {
	var raw struct {
		Setting struct {
			Apps []appMeta `json:"whitelistedApplicationList"`
		} `json:"parentalControlSetting"`
	}
	if err := a.moon(ctx, "/v2/actions/parentalControlSetting/fetchParentalControlSetting?deviceId="+url.QueryEscape(deviceID), &raw); err != nil {
		return nil, err
	}
	out := make([]Title, 0, len(raw.Setting.Apps))
	for _, app := range raw.Setting.Apps {
		if app.ApplicationID != "" {
			out = append(out, Title{ApplicationID: app.ApplicationID, Name: app.Title, ImageURI: string(app.ImageURI)})
		}
	}
	return out, nil
}

// maxImageSize limits downloaded covers and avatars.
const maxImageSize = 2 << 20

// FetchImage downloads a cover or avatar. Only https images are accepted.
func (c *Client) FetchImage(ctx context.Context, src string) (string, []byte, error) {
	u, err := url.Parse(src)
	if err != nil || u.Scheme != "https" && !c.allowHTTPImages {
		return "", nil, fmt.Errorf("image %q: only https URLs are fetched", src)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", nil, &HTTPError{Status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) > maxImageSize {
		return "", nil, fmt.Errorf("image %q is larger than %d bytes", src, maxImageSize)
	}
	ct := http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		return "", nil, fmt.Errorf("image %q is %s", src, ct)
	}
	return ct, data, nil
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (a *Account) moon(ctx context.Context, path string, out any) error {
	return a.moonDo(ctx, http.MethodGet, path, nil, out)
}

// moonDo calls the parental controls API; body is sent as JSON if not nil.
func (a *Account) moonDo(ctx context.Context, method, path string, body, out any) error {
	id, _, err := a.token(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.c.MoonURL+path, rd)
	if err != nil {
		return err
	}
	h := req.Header
	if body != nil {
		h.Set("Content-Type", "application/json")
	}
	h.Set("User-Agent", fmt.Sprintf("moon_%s/%s (%s; build:%s; %s %s)", osName, appVersion, appID, appBuild, osName, osVersion))
	h.Set("X-Moon-App-Id", appID)
	h.Set("X-Moon-Os", osName)
	h.Set("X-Moon-Os-Version", osVersion)
	h.Set("X-Moon-Model", model)
	h.Set("X-Moon-App-Display-Version", appVersion)
	h.Set("X-Moon-App-Internal-Version", appBuild)
	h.Set("X-Moon-TimeZone", a.c.TimeZone)
	h.Set("X-Moon-Os-Language", a.c.Language)
	h.Set("X-Moon-App-Language", a.c.Language)
	h.Set("Authorization", "Bearer "+id)
	if err := a.c.do(req, out); err != nil {
		return fmt.Errorf("nintendo %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

// HTTPError is a non-2xx answer of a Nintendo service.
type HTTPError struct {
	Status int
	Detail string
}

func (e *HTTPError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("status %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("status %d", e.Status)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var problem struct {
			Detail string `json:"detail"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal(body, &problem)
		detail := problem.Detail
		if detail == "" {
			detail = problem.Error
		}
		return &HTTPError{Status: resp.StatusCode, Detail: detail}
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func randomLetters(n int) (string, error) {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return "", err
		}
		b[i] = letters[k.Int64()]
	}
	return string(b), nil
}
