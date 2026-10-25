package ingest

import (
	"encoding/pem"
	"net/http/httptest"
)

// pemOf returns the test server's certificate as PEM.
func pemOf(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}
