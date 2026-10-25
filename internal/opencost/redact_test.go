package opencost

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A connection failure makes net/http echo the request URL into its error; a password in the
// base URL must not survive into the message from either the allocation or the probe calls.
func TestErrorsNeverCarryAPasswordFromTheBaseURL(t *testing.T) {
	c := New("http://admin:hunter2@127.0.0.1:1", &http.Client{Timeout: time.Second})

	_, err1 := c.ComputeAllocation(context.Background(), time.Now().Add(-time.Hour), time.Now())
	_, err2 := c.Provider(context.Background())
	for _, err := range []error{err1, err2} {
		if err == nil {
			t.Fatal("expected a connection error")
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}
