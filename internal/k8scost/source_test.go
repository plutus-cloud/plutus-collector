package k8scost

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/opencost"
)

type fakeAllocations struct {
	rows []opencost.Row
	err  error
}

func (f *fakeAllocations) ComputeAllocation(ctx context.Context, start, end time.Time) ([]opencost.Row, error) {
	return f.rows, f.err
}

// The batch header used to be assembled by the pusher; it moved here when the pusher stopped
// being Kubernetes-specific. This asserts the move preserved it — the backend requires both
// fields, and currency in particular can never be defaulted.
func TestSource_SendsClusterNameAndCurrencyOnTheBatch(t *testing.T) {
	s := &Source{
		Allocations: &fakeAllocations{rows: []opencost.Row{
			{Namespace: "checkout", Cluster: "prod-1", TotalCost: 5.0, HasTotalCost: true},
		}},
		ClusterName: "test-cluster",
		Currency:    "USD",
	}

	payload, count, err := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	batch, ok := payload.(Batch)
	if !ok {
		t.Fatalf("expected a Batch payload, got %T", payload)
	}
	if batch.ClusterName != "test-cluster" || batch.Currency != "USD" {
		t.Errorf("unexpected batch header: %+v", batch)
	}
	if count != 1 || len(batch.Rows) != 1 {
		t.Errorf("expected 1 row, got count=%d rows=%d", count, len(batch.Rows))
	}
}

func TestSource_PropagatesQueryFailure(t *testing.T) {
	s := &Source{Allocations: &fakeAllocations{err: errors.New("opencost unreachable")}}
	if _, _, err := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01"); err == nil {
		t.Fatal("expected the OpenCost error to propagate")
	}
}

// Every row being zero-cost is a real outcome (an idle cluster), and MapRows drops those — so
// the source reports zero rows and the pusher skips the push rather than sending an empty batch.
func TestSource_ReportsZeroRowsWhenEverythingWasDropped(t *testing.T) {
	s := &Source{
		Allocations: &fakeAllocations{rows: []opencost.Row{{Namespace: "idle", Cluster: "prod-1"}}},
		ClusterName: "test-cluster",
		Currency:    "USD",
	}
	_, count, err := s.Collect(context.Background(), time.Now(), time.Now(), "2026-06-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows for an all-zero-cost day, got %d", count)
	}
}
