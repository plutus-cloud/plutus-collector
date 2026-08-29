package k8scost

import (
	"math"
	"testing"
)

func TestMapRows_UsesTotalCostWhenPresent(t *testing.T) {
	rows := []OpenCostRow{
		{
			Namespace:      "checkout",
			Cluster:        "prod-1",
			ControllerKind: "Deployment",
			ControllerName: "checkout-api",
			Pod:            "checkout-api-abc123",
			Container:      "app",
			CPUCost:        5.0,
			RAMCost:        4.0,
			NetworkCost:    1.0,
			PVCost:         2.0,
			GPUCost:        0.34,
			TotalCost:      12.34,
			Labels:         map[string]string{"team": "platform"},
		},
	}

	out := MapRows("2026-08-10", "default-cluster", rows)
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	got := out[0]
	if got.TotalCost != 12.34 {
		t.Errorf("expected total_cost 12.34, got %v", got.TotalCost)
	}
	if got.Cluster != "prod-1" {
		t.Errorf("row's own cluster should win over default, got %q", got.Cluster)
	}
	if got.Namespace != "checkout" {
		t.Errorf("expected namespace checkout, got %q", got.Namespace)
	}
	if got.Labels["team"] != "platform" {
		t.Errorf("expected label team=platform, got %v", got.Labels)
	}
}

func TestMapRows_FallsBackToComponentSumWhenTotalCostMissing(t *testing.T) {
	rows := []OpenCostRow{
		{
			Namespace: "checkout",
			Cluster:   "prod-1",
			CPUCost:   0.3,
			RAMCost:   0.6,
			// TotalCost intentionally zero/unset — mirrors OpenCost's own blank-TotalCost rows,
			// see docs/kubernetes-cost-allocation.md §9.7.
		},
	}
	out := MapRows("2026-08-10", "default-cluster", rows)
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	if out[0].TotalCost != 0.9 {
		t.Errorf("expected component-sum fallback of 0.9, got %v", out[0].TotalCost)
	}
}

func TestMapRows_DropsZeroCostRows(t *testing.T) {
	rows := []OpenCostRow{
		{Namespace: "checkout", Cluster: "prod-1"},
	}
	out := MapRows("2026-08-10", "default-cluster", rows)
	if len(out) != 0 {
		t.Fatalf("expected zero-cost row to be dropped, got %d rows", len(out))
	}
}

func TestMapRows_ClusterFallsBackToDefaultWhenRowHasNone(t *testing.T) {
	rows := []OpenCostRow{
		{Namespace: "checkout", TotalCost: 1.0},
	}
	out := MapRows("2026-08-10", "default-cluster", rows)
	if out[0].Cluster != "default-cluster" {
		t.Errorf("expected fallback to default-cluster, got %q", out[0].Cluster)
	}
}

func TestMapRows_UnknownClusterSentinelWhenNoFallback(t *testing.T) {
	rows := []OpenCostRow{
		{Namespace: "checkout", TotalCost: 1.0},
	}
	out := MapRows("2026-08-10", "", rows)
	if out[0].Cluster != UnknownCluster {
		t.Errorf("expected sentinel %q, got %q", UnknownCluster, out[0].Cluster)
	}
}

func TestMapRows_UnallocatedNamespaceSentinel(t *testing.T) {
	rows := []OpenCostRow{
		{Namespace: "", Cluster: "prod-1", TotalCost: 1.0},
	}
	out := MapRows("2026-08-10", "prod-1", rows)
	if out[0].Namespace != UnallocatedWorkload {
		t.Errorf("expected sentinel %q, got %q", UnallocatedWorkload, out[0].Namespace)
	}
}

func TestMapRows_SkipsNaNAndOutOfRangeCostRows(t *testing.T) {
	rows := []OpenCostRow{
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: math.NaN()},
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: math.Inf(1)},
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: math.MaxFloat64},
		{Namespace: "checkout", Cluster: "prod-1", TotalCost: 3.5},
	}
	out := MapRows("2026-08-10", "default-cluster", rows)
	if len(out) != 1 {
		t.Fatalf("expected only the well-formed row to survive, got %d rows: %+v", len(out), out)
	}
	if out[0].TotalCost != 3.5 {
		t.Errorf("expected surviving row's total_cost 3.5, got %v", out[0].TotalCost)
	}
}

func TestMapRows_UnallocatedWorkloadSentinelWhenNoIdentity(t *testing.T) {
	rows := []OpenCostRow{
		{Namespace: "kube-system", Cluster: "prod-1", TotalCost: 1.0},
	}
	out := MapRows("2026-08-10", "prod-1", rows)
	if out[0].ControllerName != UnallocatedWorkload {
		t.Errorf("expected sentinel %q for controller_name, got %q", UnallocatedWorkload, out[0].ControllerName)
	}
}
