package ingest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewHTTPClient_TrustsAConfiguredCAAndNothingElse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()

	// Without the CA the private server is rejected: verification is never turned off.
	plain, err := NewHTTPClient(5*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Get(srv.URL); err == nil {
		t.Fatal("expected a certificate error without the CA")
	}

	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pemOf(srv)
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	trusting, err := NewHTTPClient(5*time.Second, ca)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := trusting.Get(srv.URL)
	if err != nil {
		t.Fatalf("expected the configured CA to be trusted: %v", err)
	}
	resp.Body.Close()
}

func TestNewHTTPClient_RejectsAMissingOrEmptyBundle(t *testing.T) {
	if _, err := NewHTTPClient(time.Second, filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Error("a missing file must be an error")
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHTTPClient(time.Second, empty); err == nil {
		t.Error("a file with no certificates must be an error")
	}
}
