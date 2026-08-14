package httpx

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Templates are read from web/templates relative to repo root.
	os.Chdir("../../..")
	os.Exit(m.Run())
}

func TestHealthz(t *testing.T) {
	srv := NewServer(Options{Addr: ":0"})
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("healthz body = %s", rec.Body.String())
	}
}

func TestHomeRenders(t *testing.T) {
	srv := NewServer(Options{Addr: ":0"})
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("home status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ようこそ") {
		t.Fatalf("home body missing greeting")
	}
}
