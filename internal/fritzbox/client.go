// Package fritzbox blocks and unblocks the internet access of single devices
// through the TR-064 interface of a FRITZ!Box (service X_AVM-DE_HostFilter).
// Devices stay in the local network, only the WAN access is cut.
package fritzbox

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // TR-064 HTTP digest authentication requires MD5
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config configures the connection to the FRITZ!Box.
type Config struct {
	// URL of the TR-064 interface, e.g. http://192.168.178.1:49000
	URL      string
	Username string
	Password string
	Timeout  time.Duration
}

// ErrUnknownHost is returned when the FRITZ!Box does not know the device.
var ErrUnknownHost = errors.New("fritzbox: device unknown to the FRITZ!Box")

const (
	hostsURL       = "/upnp/control/hosts"
	hostsService   = "urn:dslforum-org:service:Hosts:1"
	filterURL      = "/upnp/control/x_hostfilter"
	filterService  = "urn:dslforum-org:service:X_AVM-DE_HostFilter:1"
	errNoSuchEntry = "714"
)

// Client talks to the TR-064 interface.
type Client struct {
	cfg  Config
	http *http.Client
}

// New creates a client.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("fritzbox: url, username and password are required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}, nil
}

// HostIP returns the IPv4 address the FRITZ!Box assigned to a MAC address.
func (c *Client) HostIP(ctx context.Context, mac string) (string, error) {
	out, err := c.call(ctx, hostsURL, hostsService, "GetSpecificHostEntry", [][2]string{{"NewMACAddress", strings.ToUpper(mac)}})
	if err != nil {
		return "", err
	}
	ip := out["NewIPAddress"]
	if ip == "" {
		return "", ErrUnknownHost
	}
	return ip, nil
}

// InternetBlocked reports whether the internet access of a device is blocked.
func (c *Client) InternetBlocked(ctx context.Context, mac string) (bool, error) {
	ip, err := c.HostIP(ctx, mac)
	if err != nil {
		return false, err
	}
	out, err := c.call(ctx, filterURL, filterService, "GetWANAccessByIP", [][2]string{{"NewIPv4Address", ip}})
	if err != nil {
		return false, err
	}
	return out["NewDisallow"] == "1", nil
}

// SetInternetBlocked blocks or unblocks the internet access of a device.
func (c *Client) SetInternetBlocked(ctx context.Context, mac string, blocked bool) error {
	ip, err := c.HostIP(ctx, mac)
	if err != nil {
		return err
	}
	disallow := "0"
	if blocked {
		disallow = "1"
	}
	_, err = c.call(ctx, filterURL, filterService, "DisallowWANAccessByIP", [][2]string{{"NewIPv4Address", ip}, {"NewDisallow", disallow}})
	return err
}

// call performs a SOAP action, answering the digest challenge if needed.
func (c *Client) call(ctx context.Context, path, service, action string, args [][2]string) (map[string]string, error) {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&body, `<u:%s xmlns:u="%s">`, action, service)
	for _, a := range args {
		fmt.Fprintf(&body, "<%s>", a[0])
		_ = xml.EscapeText(&body, []byte(a[1]))
		fmt.Fprintf(&body, "</%s>", a[0])
	}
	fmt.Fprintf(&body, `</u:%s></s:Body></s:Envelope>`, action)
	payload := body.Bytes()

	send := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SoapAction", service+"#"+action)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return c.http.Do(req)
	}

	resp, err := send("")
	if err != nil {
		return nil, fmt.Errorf("fritzbox %s: %w", action, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		auth, err := c.digest(challenge, path)
		if err != nil {
			return nil, err
		}
		if resp, err = send(auth); err != nil {
			return nil, fmt.Errorf("fritzbox %s: %w", action, err)
		}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("fritzbox %s: %w", action, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("fritzbox %s: access denied, check username, password and the right \"FRITZ!Box Einstellungen\"", action)
	}
	fields, fault := parseBody(data)
	if fault != nil {
		if fault.code == errNoSuchEntry {
			return nil, ErrUnknownHost
		}
		return nil, fmt.Errorf("fritzbox %s: error %s %s", action, fault.code, fault.description)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fritzbox %s: unexpected status %d", action, resp.StatusCode)
	}
	return fields, nil
}

type soapFault struct {
	code        string
	description string
}

// parseBody returns the leaf elements of the SOAP response, or the UPnP error.
func parseBody(data []byte) (map[string]string, *soapFault) {
	fields := map[string]string{}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var stack []string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if len(stack) > 0 {
				fields[stack[len(stack)-1]] = strings.TrimSpace(text.String())
				stack = stack[:len(stack)-1]
			}
			text.Reset()
		}
	}
	if code, ok := fields["errorCode"]; ok {
		return nil, &soapFault{code: code, description: fields["errorDescription"]}
	}
	return fields, nil
}

// digest builds the Authorization header for an HTTP digest challenge (RFC 2617, MD5, qop=auth).
func (c *Client) digest(challenge, uri string) (string, error) {
	if !strings.HasPrefix(challenge, "Digest ") {
		return "", fmt.Errorf("fritzbox: unsupported authentication %q", challenge)
	}
	params := map[string]string{}
	for _, part := range splitParams(strings.TrimPrefix(challenge, "Digest ")) {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			params[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	realm, nonce := params["realm"], params["nonce"]
	if nonce == "" {
		return "", errors.New("fritzbox: digest challenge without nonce")
	}
	cnonceRaw := make([]byte, 8)
	if _, err := rand.Read(cnonceRaw); err != nil {
		return "", err
	}
	cnonce := hex.EncodeToString(cnonceRaw)
	nc := "00000001"
	ha1 := md5hex(c.cfg.Username + ":" + realm + ":" + c.cfg.Password)
	ha2 := md5hex(http.MethodPost + ":" + uri)
	header := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5`, c.cfg.Username, realm, nonce, uri)
	if strings.Contains(params["qop"], "auth") {
		header += fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s", response="%s"`, nc, cnonce, md5hex(ha1+":"+nonce+":"+nc+":"+cnonce+":auth:"+ha2))
	} else {
		header += fmt.Sprintf(`, response="%s"`, md5hex(ha1+":"+nonce+":"+ha2))
	}
	if op := params["opaque"]; op != "" {
		header += fmt.Sprintf(`, opaque="%s"`, op)
	}
	return header, nil
}

// splitParams splits a comma separated parameter list, keeping quoted commas.
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // required by HTTP digest authentication
	return hex.EncodeToString(sum[:])
}
