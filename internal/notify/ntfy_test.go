package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNtfy(t *testing.T) {
	var gotTitle, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()

	n := NewNtfy(srv.URL+"/topic", "tk")
	if err := n.Notify(context.Background(), "Zockerzeit", "noch 5 Minuten"); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "Zockerzeit" || gotAuth != "Bearer tk" || gotBody != "noch 5 Minuten" {
		t.Fatalf("unexpected request: %q %q %q", gotTitle, gotAuth, gotBody)
	}
	if err := NewNtfy(srv.URL+"/fail", "").Notify(context.Background(), "t", "m"); err == nil {
		t.Fatal("expected error on 403")
	}
	if err := NewNtfy("http://127.0.0.1:1/x", "").Notify(context.Background(), "t", "m"); err == nil {
		t.Fatal("expected connection error")
	}
}
