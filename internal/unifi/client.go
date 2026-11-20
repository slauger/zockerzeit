// Package unifi is a minimal client for the UniFi Network Application API.
// It covers exactly what Zockerzeit needs: listing clients and access points
// and blocking or unblocking a client by MAC address.
package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"
)

// Config configures the connection to the UniFi Network Application.
type Config struct {
	// URL of the controller, e.g. https://unifi.example.lan:8443
	URL string
	// Site is the UniFi site name, usually "default".
	Site string
	// UniFiOS must be true for UniFi OS consoles (UDM, UCG, Cloud Key Gen2+).
	// Self-hosted Network Applications use the classic API (false).
	UniFiOS bool
	// APIKey authenticates via X-API-KEY header. If empty, Username and
	// Password are used for a session login.
	APIKey   string
	Username string
	Password string
	// InsecureSkipVerify disables TLS verification (self-signed certificates).
	InsecureSkipVerify bool
	Timeout            time.Duration
}

// Station is a currently connected client (stat/sta).
type Station struct {
	MAC      string `json:"mac"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	IP       string `json:"ip"`
	APMAC    string `json:"ap_mac"`
	ESSID    string `json:"essid"`
	IsWired  bool   `json:"is_wired"`
	RxBytes  int64  `json:"rx_bytes"`
	TxBytes  int64  `json:"tx_bytes"`
	Uptime   int64  `json:"uptime"`
	LastSeen int64  `json:"last_seen"`
}

// DisplayName returns the best human readable name of the station.
func (s Station) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	if s.Hostname != "" {
		return s.Hostname
	}
	return s.MAC
}

// KnownClient is a client the controller has ever seen (rest/user).
type KnownClient struct {
	MAC      string `json:"mac"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Blocked  bool   `json:"blocked"`
	LastSeen int64  `json:"last_seen"`
}

// DisplayName returns the best human readable name of the client.
func (k KnownClient) DisplayName() string {
	if k.Name != "" {
		return k.Name
	}
	if k.Hostname != "" {
		return k.Hostname
	}
	return k.MAC
}

// Device is a UniFi network device such as an access point.
type Device struct {
	MAC  string `json:"mac"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Client talks to the UniFi Network Application.
type Client struct {
	cfg  Config
	http *http.Client

	mu       sync.Mutex
	csrf     string
	loggedIn bool
}

// ErrUnauthorized is returned when the controller rejects the credentials.
var ErrUnauthorized = errors.New("unifi: unauthorized")

// New creates a new client.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("unifi: url is required")
	}
	if cfg.Site == "" {
		cfg.Site = "default"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.APIKey == "" && (cfg.Username == "" || cfg.Password == "") {
		return nil, errors.New("unifi: either apiKey or username and password are required")
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for self-signed controller certificates
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Jar: jar, Timeout: cfg.Timeout, Transport: transport},
	}, nil
}

func (c *Client) apiPath(p string) string {
	prefix := ""
	if c.cfg.UniFiOS {
		prefix = "/proxy/network"
	}
	return c.cfg.URL + prefix + "/api/s/" + c.cfg.Site + "/" + p
}

func (c *Client) login(ctx context.Context) error {
	path := "/api/login"
	if c.cfg.UniFiOS {
		path = "/api/auth/login"
	}
	body, _ := json.Marshal(map[string]any{
		"username": c.cfg.Username,
		"password": c.cfg.Password,
		"remember": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("unifi login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("unifi login: unexpected status %d", resp.StatusCode)
	}
	c.csrf = resp.Header.Get("X-Csrf-Token")
	c.loggedIn = true
	return nil
}

type envelope struct {
	Meta struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
	Data json.RawMessage `json:"data"`
}

// do performs an API request and decodes the "data" field into out.
// Session based clients log in lazily and retry once after a 401.
func (c *Client) do(ctx context.Context, method, path string, payload, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := range 2 {
		if c.cfg.APIKey == "" && !c.loggedIn {
			if err := c.login(ctx); err != nil {
				return err
			}
		}
		status, err := c.send(ctx, method, path, payload, out)
		if status == http.StatusUnauthorized && c.cfg.APIKey == "" && attempt == 0 {
			c.loggedIn = false
			continue
		}
		return err
	}
	return ErrUnauthorized
}

func (c *Client) send(ctx context.Context, method, path string, payload, out any) (int, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiPath(path), body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("X-API-KEY", c.cfg.APIKey)
	}
	if c.csrf != "" {
		req.Header.Set("X-Csrf-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("unifi %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if tok := resp.Header.Get("X-Csrf-Token"); tok != "" {
		c.csrf = tok
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, ErrUnauthorized
	}
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return resp.StatusCode, fmt.Errorf("unifi %s %s: status %d: decode: %w", method, path, resp.StatusCode, err)
	}
	if resp.StatusCode >= 300 || env.Meta.RC != "ok" {
		return resp.StatusCode, fmt.Errorf("unifi %s %s: status %d: %s", method, path, resp.StatusCode, env.Meta.Msg)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("unifi %s %s: decode data: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// Stations returns all currently connected clients.
func (c *Client) Stations(ctx context.Context) ([]Station, error) {
	var out []Station
	err := c.do(ctx, http.MethodGet, "stat/sta", nil, &out)
	normalize(out, func(s *Station) *string { return &s.MAC }, func(s *Station) *string { return &s.APMAC })
	return out, err
}

// KnownClients returns all clients known to the controller, including
// offline and blocked ones.
func (c *Client) KnownClients(ctx context.Context) ([]KnownClient, error) {
	var out []KnownClient
	err := c.do(ctx, http.MethodGet, "rest/user", nil, &out)
	normalize(out, func(k *KnownClient) *string { return &k.MAC })
	return out, err
}

// Devices returns all adopted UniFi devices (access points, switches, ...).
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out []Device
	err := c.do(ctx, http.MethodGet, "stat/device-basic", nil, &out)
	normalize(out, func(d *Device) *string { return &d.MAC })
	return out, err
}

// Block disconnects the client and prevents it from reconnecting.
func (c *Client) Block(ctx context.Context, mac string) error {
	return c.do(ctx, http.MethodPost, "cmd/stamgr", map[string]string{"cmd": "block-sta", "mac": NormalizeMAC(mac)}, nil)
}

// Unblock allows a previously blocked client to connect again.
func (c *Client) Unblock(ctx context.Context, mac string) error {
	return c.do(ctx, http.MethodPost, "cmd/stamgr", map[string]string{"cmd": "unblock-sta", "mac": NormalizeMAC(mac)}, nil)
}

// NormalizeMAC lower-cases a MAC address and converts dashes to colons.
func NormalizeMAC(mac string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(mac)), "-", ":")
}

func normalize[T any](items []T, fields ...func(*T) *string) {
	for i := range items {
		for _, f := range fields {
			p := f(&items[i])
			*p = NormalizeMAC(*p)
		}
	}
}
