package fritzbox

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeBox emulates the two TR-064 services with digest authentication.
type fakeBox struct {
	mu      sync.Mutex
	hosts   map[string]string // MAC -> IP
	blocked map[string]bool   // IP -> blocked
}

func (f *fakeBox) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="F!Box SOAP-Auth", nonce="abc123", algorithm=MD5, qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !validDigest(auth, "zockerzeit", "secret", r.URL.Path) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		action := r.Header.Get("SoapAction")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(action, "#GetSpecificHostEntry"):
			mac := field(body, "NewMACAddress")
			ip, ok := f.hosts[mac]
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, fault("714", "NoSuchEntryInArray"))
				return
			}
			_, _ = io.WriteString(w, response("GetSpecificHostEntry", "<NewIPAddress>"+ip+"</NewIPAddress><NewActive>1</NewActive>"))
		case strings.HasSuffix(action, "#GetWANAccessByIP"):
			v := "0"
			if f.blocked[field(body, "NewIPv4Address")] {
				v = "1"
			}
			_, _ = io.WriteString(w, response("GetWANAccessByIP", "<NewDisallow>"+v+"</NewDisallow><NewWANAccess>granted</NewWANAccess>"))
		case strings.HasSuffix(action, "#DisallowWANAccessByIP"):
			f.blocked[field(body, "NewIPv4Address")] = field(body, "NewDisallow") == "1"
			_, _ = io.WriteString(w, response("DisallowWANAccessByIP", ""))
		default:
			t.Errorf("unexpected action %q", action)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
}

func field(body []byte, name string) string {
	m := regexp.MustCompile("<" + name + ">([^<]*)</" + name + ">").FindSubmatch(body)
	if m == nil {
		return ""
	}
	return string(m[1])
}

func response(action, inner string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:` +
		action + `Response xmlns:u="x">` + inner + `</u:` + action + `Response></s:Body></s:Envelope>`
}

func fault(code, desc string) string {
	return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>` +
		`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:dslforum-org:control-1-0">` +
		`<errorCode>` + code + `</errorCode><errorDescription>` + desc + `</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`
}

func validDigest(header, user, pass, uri string) bool {
	p := map[string]string{}
	for _, part := range splitParams(strings.TrimPrefix(header, "Digest ")) {
		k, v, _ := strings.Cut(part, "=")
		p[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	h := func(s string) string { sum := md5.Sum([]byte(s)); return hex.EncodeToString(sum[:]) }
	ha1 := h(user + ":" + p["realm"] + ":" + pass)
	ha2 := h("POST:" + uri)
	return p["uri"] == uri && p["response"] == h(ha1+":"+p["nonce"]+":"+p["nc"]+":"+p["cnonce"]+":auth:"+ha2)
}

func TestBlockAndUnblock(t *testing.T) {
	box := &fakeBox{hosts: map[string]string{"AA:BB:CC:00:00:01": "10.0.0.50"}, blocked: map[string]bool{}}
	srv := httptest.NewServer(box.handler(t))
	defer srv.Close()
	c, err := New(Config{URL: srv.URL + "/", Username: "zockerzeit", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	ip, err := c.HostIP(ctx, "aa:bb:cc:00:00:01")
	if err != nil || ip != "10.0.0.50" {
		t.Fatalf("HostIP = %q, %v", ip, err)
	}
	if b, err := c.InternetBlocked(ctx, "aa:bb:cc:00:00:01"); err != nil || b {
		t.Fatalf("initially blocked=%v err=%v", b, err)
	}
	if err := c.SetInternetBlocked(ctx, "aa:bb:cc:00:00:01", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.InternetBlocked(ctx, "aa:bb:cc:00:00:01"); !b {
		t.Fatal("device should be blocked")
	}
	if err := c.SetInternetBlocked(ctx, "aa:bb:cc:00:00:01", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.InternetBlocked(ctx, "aa:bb:cc:00:00:01"); b {
		t.Fatal("device should be unblocked")
	}

	if _, err := c.InternetBlocked(ctx, "de:ad:be:ef:00:00"); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("expected ErrUnknownHost, got %v", err)
	}
	if err := c.SetInternetBlocked(ctx, "de:ad:be:ef:00:00", true); !errors.Is(err, ErrUnknownHost) {
		t.Fatalf("expected ErrUnknownHost, got %v", err)
	}
}

func TestWrongPassword(t *testing.T) {
	box := &fakeBox{hosts: map[string]string{}, blocked: map[string]bool{}}
	srv := httptest.NewServer(box.handler(t))
	defer srv.Close()
	c, _ := New(Config{URL: srv.URL, Username: "zockerzeit", Password: "wrong"})
	_, err := c.HostIP(context.Background(), "aa:bb:cc:00:00:01")
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("expected access denied, got %v", err)
	}
}

func TestNewValidationAndDigestErrors(t *testing.T) {
	if _, err := New(Config{URL: "http://x"}); err == nil {
		t.Error("expected error without credentials")
	}
	c, _ := New(Config{URL: "http://x", Username: "u", Password: "p"})
	if _, err := c.digest("Basic realm=x", "/"); err == nil {
		t.Error("expected error for basic auth challenge")
	}
	if _, err := c.digest(`Digest realm="x"`, "/"); err == nil {
		t.Error("expected error without nonce")
	}
	h, err := c.digest(`Digest realm="a, b", nonce="n", opaque="o"`, "/x")
	if err != nil || !strings.Contains(h, `opaque="o"`) || strings.Contains(h, "qop") || !strings.Contains(h, `realm="a, b"`) {
		t.Errorf("digest without qop = %q, %v", h, err)
	}
	if _, err := c.HostIP(context.Background(), "aa"); err == nil {
		t.Error("expected connection error")
	}
}

func TestParseBody(t *testing.T) {
	fields, f := parseBody([]byte(response("X", "<A>1</A><B> two </B>")))
	if f != nil || fields["A"] != "1" || fields["B"] != "two" {
		t.Fatalf("fields = %v, fault = %v", fields, f)
	}
	if _, f := parseBody([]byte(fault("401", "Invalid Action"))); f == nil || f.code != "401" {
		t.Fatalf("fault not parsed: %+v", f)
	}
}
