// Command mockingest is a stand-in for Plutus's kubernetes-cost ingest endpoint, for the
// end-to-end test. It accepts the agent's POSTs over TLS, remembers each body, and lists them at
// GET /received so the test can assert on what a real install would have sent.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

func main() {
	var (
		mu       sync.Mutex
		received []json.RawMessage
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })

	mux.HandleFunc("/api/ingest/kubernetes-cost", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil || !json.Valid(body) {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		var parsed struct {
			Rows []json.RawMessage `json:"rows"`
		}
		_ = json.Unmarshal(body, &parsed)

		mu.Lock()
		received = append(received, json.RawMessage(body))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": len(parsed.Rows), "rejected": 0})
	})

	mux.HandleFunc("/received", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		out := received
		if out == nil {
			out = []json.RawMessage{}
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	cert, key := os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY")
	log.Printf("mock ingest listening on :8443")
	log.Fatal(http.ListenAndServeTLS(":8443", cert, key, mux))
}
