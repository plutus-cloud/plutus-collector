# plutus-collector

A small Go agent you install into your own Kubernetes cluster to send Kubernetes
cost-allocation data to [Plutus](https://plutus-cloud.com). It periodically queries
[OpenCost](https://www.opencost.io/)'s local `/allocation` API (OpenCost computes
per-namespace/workload cost allocation; this agent just forwards that output) and pushes it to
Plutus over HTTPS.

This is the companion agent for the Kubernetes cost source in Plutus's backend. The two
communicate over one fixed HTTP/JSON contract (`POST /api/ingest/kubernetes-cost`, an API key
you mint in Plutus) — no shared code, no shared release cadence, and no access to anything else
in your account beyond that one endpoint.

## Why a separate repo

This is Plutus-authored code that runs inside *your* cluster, not Plutus's own infrastructure —
a materially different trust relationship than the rest of the product. Every comparable vendor
(Datadog Agent, Grafana Alloy, Kubecost's collector) ships the equivalent component from its own
dedicated public repo, specifically so you can read the source of what you're granting cluster
access to before installing it. Splitting it out also gives this component its own
version/release cadence, independent of the main product.

## Why a long-lived process, not a CronJob

This runs as a single-replica `Deployment` with an internal ticker, not a Kubernetes `CronJob`.
A one-shot Job pays a cold TLS-handshake and container-init cost on every run and gives no
continuous health signal. A long-lived process keeps a warm connection to the ingest endpoint,
retries a failed push in-process instead of relying on Job `backoffLimit` semantics, and exposes
normal liveness/readiness probes plus a `/metrics` endpoint that Prometheus/Alloy can scrape
like everything else in the cluster. Same shape as every comparable in-cluster agent.

## Prerequisites

- A Kubernetes cluster (1.24+ is a safe assumption; nothing here needs anything exotic).
- Helm 3.
- Either let this chart install OpenCost for you (default), or already run OpenCost yourself.
- A Plutus account with the Kubernetes cost source added. In the Plutus UI: **Cost Sources →
  Kubernetes → Generate API Key** — this mints the key and shows you the exact `helm install`
  command below with your key already filled in. Generating or revoking this key requires an
  account admin. You can also mint one via the API:
  `POST /api/accounts/:accountId/cost-sources/kubernetes/api-key`.

## Quickstart

```bash
helm install plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector \
  --namespace plutus-collector --create-namespace \
  --set clusterName=prod-us-east-1 \
  --set currency=USD \
  --set apiKey=<the key from the Plutus UI>
```

That's the whole install: it deploys OpenCost (bundled subchart) plus this pusher, and cost data
starts flowing into Plutus within one push cycle (daily by default).

If you already run OpenCost:

```bash
helm install plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector \
  --namespace plutus-collector --create-namespace \
  --set opencost.enabled=false \
  --set opencost.endpoint=http://opencost.your-namespace.svc.cluster.local:9003 \
  --set clusterName=prod-us-east-1 \
  --set currency=USD \
  --set apiKey=<the key from the Plutus UI>
```

Prefer not to put your API key in Helm values/history? Create the Secret yourself first and
point the chart at it instead of `apiKey`:

```bash
kubectl create secret generic plutus-collector-key \
  --namespace plutus-collector \
  --from-literal=api-key=<the key from the Plutus UI>

helm install plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector \
  --namespace plutus-collector --create-namespace \
  --set clusterName=prod-us-east-1 \
  --set currency=USD \
  --set existingSecret=plutus-collector-key
```

### Uninstalling

```bash
helm uninstall plutus-collector --namespace plutus-collector
```

Revoking the API key in Plutus (Cost Sources → Kubernetes → Revoke) stops the agent from being
able to push immediately, independent of whether you've uninstalled the chart yet — useful if
you need to cut access off right away and tear down the cluster resources afterward.

## Configuration reference

The agent itself reads only environment variables (12-factor, no config file); the Helm chart's
`values.yaml` maps to them 1:1. Values on the left, the container env var they set on the right.

| `values.yaml` key                | env var                 | required | default                                                        |
|-----------------------------------|--------------------------|----------|-----------------------------------------------------------------|
| `clusterName`                     | `CLUSTER_NAME`           | **yes**  | none — install fails validation if left as the empty placeholder |
| `currency`                        | `CURRENCY`               | **yes**  | none — never defaults to USD; see "Currency" below               |
| `apiKey` / `existingSecret`+`existingSecretKey` | `PLUTUS_API_KEY` (via Secret) | **yes** | none |
| `plutusIngestUrl`                 | `PLUTUS_INGEST_URL`      | no       | `https://console.plutus-cloud.com/api/ingest/kubernetes-cost`   |
| `pushIntervalMinutes`             | `PUSH_INTERVAL_MINUTES`  | no       | `1440` (daily)                                                   |
| n/a — derived from `opencost.enabled`/`opencost.endpoint` | `OPENCOST_URL` | no | bundled subchart's in-cluster Service when `opencost.enabled=true`, otherwise `opencost.endpoint` |
| `metricsPort` / `service.port`    | `METRICS_ADDR`           | no       | `9100`                                                            |

Retry count and backoff on a failed push are fixed constants, not currently exposed as
`values.yaml` knobs — they haven't needed to be configurable yet.

### Currency

OpenCost does not report its own currency anywhere in its allocation output — it's a cluster-side
configuration value with no API field to read it back from. Guessing `USD` would silently
mislabel every figure, so `currency`/`CURRENCY` has no default anywhere in this component — set
it to whatever ISO 4217 code OpenCost was configured with.

## Security & permissions

- **No Kubernetes API access.** This agent has no `ServiceAccount` RBAC bindings of its own — it
  only calls OpenCost's HTTP `/allocation` endpoint (in-cluster) and Plutus's ingest endpoint
  (outbound HTTPS). It never reads pods, nodes, or any other cluster object directly; OpenCost
  is what needs cluster-read access, under its own subchart's own RBAC, unrelated to this
  component. If you disable the bundled subchart (`opencost.enabled=false`) and point at an
  OpenCost you already run, this agent's own permission footprint stays exactly the same: none.
- **Locked-down pod security context**: runs as non-root, with a read-only root filesystem,
  `allowPrivilegeEscalation: false`, and every Linux capability dropped.
- **Small, fixed resource footprint**: requests `10m` CPU / `32Mi` memory, capped at `64Mi`
  memory. Fixed at 1 replica — a second replica would just push the same batch twice, which the
  backend's idempotent ingest makes harmless but pointless, so there's nothing to autoscale.
- **What it sends outbound**: only the JSON batch described below, over HTTPS, to the
  `plutusIngestUrl` you configure (defaults to Plutus's production ingest endpoint) — nothing
  else leaves your cluster.

## What this pushes

Once a day (or every `pushIntervalMinutes`), the agent queries OpenCost for the previous UTC
day's allocation, grouped by namespace/controller/pod/container, and POSTs a batch like:

```json
{
  "cluster_name": "prod-us-east-1",
  "currency": "USD",
  "rows": [
    {
      "date": "2026-08-10",
      "cluster": "prod-us-east-1",
      "namespace": "checkout",
      "controller_kind": "Deployment",
      "controller_name": "checkout-api",
      "pod": "checkout-api-abc123",
      "container": "app",
      "total_cost": 12.34,
      "cpu_cost": 5.0,
      "ram_cost": 4.0,
      "network_cost": 1.0,
      "pv_cost": 2.0,
      "gpu_cost": 0.34,
      "labels": { "team": "platform" }
    }
  ]
}
```

to `POST /api/ingest/kubernetes-cost` with `Authorization: Bearer <api key>`. A row with no
namespace/workload identity is kept under an `__unallocated__` sentinel rather than dropped
(unallocated/idle capacity is actionable, not noise), and a row with no cluster identity falls
back to the connection's own `cluster_name`. If OpenCost's response omits pod labels or GPU
cost for a row, the agent treats them as empty/zero rather than failing the whole push — so an
occasional missing label or GPU figure in Plutus reflects what OpenCost itself reported, not a
bug in this agent. The response is `{ accepted, rejected, errors: [...] }`; any rejections are
logged with full detail — check the pod logs if `/metrics` shows a push succeeding but data
looks incomplete in Plutus.

## How do I verify it's working?

1. **`/healthz`** — liveness/readiness, always `200 ok` once the process is up (a down
   OpenCost/Plutus endpoint is a push failure to retry, not a readiness failure).
2. **`/metrics`** (Prometheus text format) — the fields that matter:
   - `plutus_collector_last_push_success` — `1` after a successful push, `0` after one that
     failed all its retries.
   - `plutus_collector_last_push_timestamp_seconds` — when that last attempt finished.
   - `plutus_collector_last_push_row_count` — how many rows were in the last successful batch.
   - `plutus_collector_last_push_duration_seconds` — how long the attempt (including retries)
     took.

   ```bash
   kubectl -n plutus-collector port-forward svc/plutus-collector 9100:9100
   curl localhost:9100/metrics
   ```
3. **The Plutus UI** — the Kubernetes cost source's connection panel shows "Receiving data" with
   a last-push timestamp once the first push succeeds.
4. **Pod logs** — structured JSON (one object per line), so your own log aggregation can parse
   it directly. Every push cycle logs a start line, the row count about to be pushed, and either
   a success (with `accepted`/`rejected` counts) or a failure (with the error).

### Scraping `/metrics`

This chart deploys a plain `Service` (`service.enabled`, default `true`) rather than a
`prometheus-operator` `ServiceMonitor` CRD — we can't assume every cluster has that CRD
installed, and a plain Service degrades gracefully (any scraper can find it via Service
discovery) where a `ServiceMonitor` would fail to apply at all without the CRD. If you do run
prometheus-operator, add your own `ServiceMonitor` pointed at this Service's `metrics` port
(`9100` by default).

## Development

```bash
go build ./...
go test ./...
docker build -t plutus-collector:dev .
helm lint chart/
```

### Package layout

- `cmd/plutus-collector` — entrypoint: wires config, the OpenCost client, the ingest client, the
  pusher loop, and the metrics/health HTTP server together; handles `SIGTERM`/`SIGINT` for a
  clean shutdown.
- `internal/config` — env-var loading and validation (12-factor, no config file).
- `internal/opencost` — the OpenCost `/allocation` HTTP client.
- `internal/ingest` — the Plutus batch JSON shape (a fixed contract with the backend's ingest
  route) and the OpenCost-row → batch-row mapping logic.
- `internal/pusher` — the ticker loop and in-process retry/backoff.
- `internal/metrics` — the hand-rolled `/metrics` (Prometheus text format) and `/healthz`
  handlers.

## Support

Questions or issues installing or running this agent: [contact Plutus support](https://plutus-cloud.com)
or open an issue in this repo.
