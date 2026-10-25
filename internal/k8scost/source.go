package k8scost

import (
	"context"
	"time"

	"github.com/plutus-cloud/plutus-collector/internal/opencost"
	"github.com/plutus-cloud/plutus-collector/internal/pusher"
)

// AllocationSource is the subset of opencost.Client's behavior this source needs — an interface
// so tests can substitute a fake instead of a real OpenCost HTTP server.
type AllocationSource interface {
	ComputeAllocation(ctx context.Context, start, end time.Time) ([]opencost.Row, error)
}

// Source adapts OpenCost to the pusher's Source interface.
//
// This is the code that used to live inline in pusher.tick — the OpenCost query, the row
// mapping, and the batch assembly. Moving it here is what let the pusher stop being a Kubernetes
// agent, and it costs nothing: the behaviour is unchanged and the seam it plugs into
// (AllocationSource) already existed.
type Source struct {
	Allocations AllocationSource

	// ClusterName labels rows whose own cluster identity is empty, and is sent as the
	// batch-level cluster_name. Required — see internal/config.
	ClusterName string

	// Currency is the ISO 4217 code OpenCost was configured to price in. OpenCost emits no
	// currency field anywhere, so this is carried in the payload and can never be defaulted.
	Currency string

	// EmptyOK makes an empty window a normal outcome (EMPTY_RESULT=ignore). It is off by
	// default: a cluster always runs at least its own system pods, so a day with no priced
	// allocations means the pipeline behind OpenCost is broken, and the agent should say so
	// rather than report a healthy push of nothing.
	EmptyOK bool
}

func (s *Source) Name() string { return "OpenCost" }

func (s *Source) EmptyIsHealthy() bool { return s.EmptyOK }

// Annotate fills the batch's collector block from what the pusher knows, keeping an empty reason
// the source already set. Implementing it is also what tells the pusher this endpoint accepts a
// heartbeat: Plutus's kubernetes-cost ingest writes nothing for rows: [].
func (s *Source) Annotate(payload any, meta pusher.CollectorMeta) any {
	batch, ok := payload.(Batch)
	if !ok {
		return payload
	}
	c := Collector{}
	if batch.Collector != nil {
		c = *batch.Collector
	}
	c.Version = meta.Version
	c.PrometheusMode = meta.PrometheusMode
	c.Window = CollectorWindow{
		Start: meta.WindowStart.UTC().Format(time.RFC3339),
		End:   meta.WindowEnd.UTC().Format(time.RFC3339),
	}
	if len(meta.Preflight) > 0 {
		c.Preflight = meta.Preflight
	}
	batch.Collector = &c
	return batch
}

func (s *Source) Collect(ctx context.Context, start, end time.Time, dateKey string) (any, int, error) {
	rows, err := s.Allocations.ComputeAllocation(ctx, start, end)
	if err != nil {
		return nil, 0, err
	}

	ocRows := make([]OpenCostRow, 0, len(rows))
	for _, r := range rows {
		ocRows = append(ocRows, OpenCostRow{
			Namespace:      r.Namespace,
			Cluster:        r.Cluster,
			ControllerKind: r.ControllerKind,
			ControllerName: r.ControllerName,
			Pod:            r.Pod,
			Container:      r.Container,
			CPUCost:        r.CPUCost,
			RAMCost:        r.RAMCost,
			NetworkCost:    r.NetworkCost,
			PVCost:         r.PVCost,
			GPUCost:        r.GPUCost,
			TotalCost:      r.TotalCostOrSum(),
			Labels:         r.Labels,
		})
	}

	batch := Batch{
		ClusterName: s.ClusterName,
		Currency:    s.Currency,
		Rows:        MapRows(dateKey, s.ClusterName, ocRows),
	}
	if len(batch.Rows) == 0 {
		reason := EmptyAllZeroCost
		if len(rows) == 0 {
			reason = EmptyNoAllocations
		}
		batch.Collector = &Collector{EmptyReason: reason}
	}
	return batch, len(batch.Rows), nil
}
