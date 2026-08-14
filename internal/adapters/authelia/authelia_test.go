package authelia

import (
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyHeaderAuth(t *testing.T) {
	a, err := New([]string{"172.16.0.0/12"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.5:44321"
	r.Header.Set("Remote-User", "mikey")
	r.Header.Set("Remote-Name", "Mikey Austin")
	id, err := a.Authenticate(r)
	if err != nil || string(id.ID) != "mikey" || id.DisplayName != "Mikey Austin" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
}

func TestUntrustedPeerRejected(t *testing.T) {
	a, _ := New([]string{"172.16.0.0/12"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:1234" // spoofed header from outside
	r.Header.Set("Remote-User", "mallory")
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestMissingHeaderRejected(t *testing.T) {
	a, _ := New([]string{"172.16.0.0/12"})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.5:44321"
	if _, err := a.Authenticate(r); err == nil {
		t.Fatal("expected rejection")
	}
}
