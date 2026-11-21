package secret

import "testing"

func TestBox(t *testing.T) {
	b, err := NewBox([]byte("s3cret"), "nintendo")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal("session-token")
	if err != nil {
		t.Fatal(err)
	}
	if sealed == "session-token" {
		t.Fatal("not encrypted")
	}
	if got, err := b.Open(sealed); err != nil || got != "session-token" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	other, _ := NewBox([]byte("s3cret"), "other")
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a different purpose must not decrypt")
	}
	for _, bad := range []string{"!!", "YQ=="} {
		if _, err := b.Open(bad); err == nil {
			t.Errorf("Open(%q) should fail", bad)
		}
	}
	if _, err := NewBox(nil, "x"); err == nil {
		t.Fatal("empty key accepted")
	}
}
