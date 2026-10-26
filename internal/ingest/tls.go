package ingest

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// NewHTTPClient returns the client used to reach the ingest endpoint. With caFile set it trusts
// that PEM bundle in addition to the system roots, for installs behind a TLS-intercepting proxy
// or an internal gateway with a private CA. It never disables verification.
func NewHTTPClient(timeout time.Duration, caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{Timeout: timeout}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading PLUTUS_INGEST_CA_FILE: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("PLUTUS_INGEST_CA_FILE contains no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}
