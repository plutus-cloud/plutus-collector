// Package k8scost holds the Kubernetes half of this repo: the exact JSON batch shape POSTed to
// Plutus's POST /api/ingest/kubernetes-cost, and the mapping from OpenCost allocation rows into
// it.
//
// Split out of internal/ingest when the LiteLLM collector was added. That package is now the
// shared transport and knows nothing about what it sends; the per-source payload shape,
// sentinels and rounding rules live beside the source that produces them. Nothing here is
// reachable from the LiteLLM binary, which is the point — the two contracts can move
// independently.
//
// This shape is a fixed contract with plutus-backend's routes/kubernetes-cost-ingest.js — do
// not rename fields or change types without coordinating there. The sentinel values below
// (`__unallocated__`, `__unknown_cluster__`) mirror that route's row-mapping conventions
// exactly.
package k8scost

import (
	"math"
)

const UnallocatedWorkload = "__unallocated__"

// UnknownCluster marks a row with no cluster identity at all — reachable only when OpenCost's
// own row carries no cluster AND the agent's CLUSTER_NAME fallback is also empty (which
// config.Load prevents at startup, since CLUSTER_NAME is required; kept here for robustness and
// to mirror lib/sync/opencost.js's UNKNOWN_CLUSTER exactly). Labelling the spend beats dropping
// it.
const UnknownCluster = "__unknown_cluster__"

// Row is one line of Kubernetes cost allocation, matching the backend's documented ingest
// contract field-for-field.
type Row struct {
	Date           string            `json:"date"`
	Cluster        string            `json:"cluster"`
	Namespace      string            `json:"namespace"`
	ControllerKind string            `json:"controller_kind,omitempty"`
	ControllerName string            `json:"controller_name,omitempty"`
	Pod            string            `json:"pod,omitempty"`
	Container      string            `json:"container,omitempty"`
	TotalCost      float64           `json:"total_cost"`
	CPUCost        float64           `json:"cpu_cost"`
	RAMCost        float64           `json:"ram_cost"`
	NetworkCost    float64           `json:"network_cost"`
	PVCost         float64           `json:"pv_cost"`
	GPUCost        float64           `json:"gpu_cost"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// Batch is the full POST body.
type Batch struct {
	ClusterName string `json:"cluster_name"`
	Currency    string `json:"currency"`
	Rows        []Row  `json:"rows"`
}

// OpenCostRow is the minimal shape this package maps from — deliberately not a direct
// dependency on internal/opencost.Row, so the mapping logic is unit-testable without pulling in
// the HTTP client, and so a future second source of allocation rows (e.g. a local CSV fallback)
// can reuse MapRows.
type OpenCostRow struct {
	Namespace      string
	Cluster        string
	ControllerKind string
	ControllerName string
	Pod            string
	Container      string
	CPUCost        float64
	RAMCost        float64
	NetworkCost    float64
	PVCost         float64
	GPUCost        float64
	TotalCost      float64
	Labels         map[string]string
}

// MapRows converts OpenCost allocation rows for a single UTC day into the batch's Row shape.
// date must be "YYYY-MM-DD" UTC. defaultCluster is the agent's configured CLUSTER_NAME, used
// only when a row carries no cluster of its own — the row's own cluster always wins (matches
// lib/sync/opencost.js's precedent: "the row's own Cluster wins over any configured cluster-name
// override").
//
// Rows with zero total cost are dropped (matches the pull runner's `if (cost === 0) continue`) —
// there is nothing informative about pushing a zero-cost line, and it only inflates row counts.
func MapRows(date string, defaultCluster string, rows []OpenCostRow) []Row {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		total := r.TotalCost
		if total == 0 && (r.CPUCost != 0 || r.RAMCost != 0 || r.NetworkCost != 0 || r.PVCost != 0 || r.GPUCost != 0) {
			total = r.CPUCost + r.RAMCost + r.NetworkCost + r.PVCost + r.GPUCost
		}
		if total == 0 {
			continue
		}
		if !isValidCost(total) {
			// A NaN/Inf/out-of-int64-range total (a malformed OpenCost response) would
			// silently corrupt the row via round2's float->int64 conversion — skip it the
			// same way a zero-cost row is skipped, rather than pushing bad data.
			continue
		}

		namespace := r.Namespace
		if namespace == "" {
			namespace = UnallocatedWorkload
		}

		cluster := r.Cluster
		if cluster == "" {
			cluster = defaultCluster
		}
		if cluster == "" {
			cluster = UnknownCluster
		}

		controllerName := r.ControllerName
		if controllerName == "" && r.Pod == "" && r.Container == "" {
			// No workload identity at all (cluster-level cost OpenCost couldn't attribute) —
			// keep it under the unallocated sentinel rather than dropping it, matching
			// lib/sync/opencost.js's fallback-chain-of-three (ControllerName -> Pod -> Container).
			controllerName = UnallocatedWorkload
		}

		out = append(out, Row{
			Date:           date,
			Cluster:        cluster,
			Namespace:      namespace,
			ControllerKind: r.ControllerKind,
			ControllerName: controllerName,
			Pod:            r.Pod,
			Container:      r.Container,
			TotalCost:      round2(total),
			CPUCost:        round2(r.CPUCost),
			RAMCost:        round2(r.RAMCost),
			NetworkCost:    round2(r.NetworkCost),
			PVCost:         round2(r.PVCost),
			GPUCost:        round2(r.GPUCost),
			Labels:         nonEmptyLabels(r.Labels),
		})
	}
	return out
}

func nonEmptyLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isValidCost reports whether f is safe to feed through round2's float->int64 cents
// conversion: finite, and small enough that f*100 does not overflow int64.
func isValidCost(f float64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	const maxCents = float64(math.MaxInt64) / 100
	return f > -maxCents && f < maxCents
}

func round2(f float64) float64 {
	return float64(int64(f*100+sign(f)*0.5)) / 100
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}
