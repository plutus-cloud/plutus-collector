# plutus-collector

Small Go agents you run inside your own infrastructure to send cost data to
[Plutus](https://plutus-cloud.com). Two of them, from this one repo:

| Binary | Reads | Pushes to |
|---|---|---|
| `plutus-collector` | [OpenCost](https://www.opencost.io/)'s local `/allocation` API, in your Kubernetes cluster | `POST /api/ingest/kubernetes-cost` |
| `plutus-litellm-collector` | your self-hosted [LiteLLM](https://docs.litellm.ai/) proxy's admin spend API | `POST /api/ingest/litellm-cost` |

Both exist for the same reason: the system being measured runs **inside your network**, where
nothing of Plutus's can reach it. So the agent runs on your side and pushes out, rather than
Plutus polling in — which is also why neither needs you to open anything inbound.

Each communicates over one fixed HTTP/JSON contract with an API key you mint in Plutus — no
access to anything else in your account beyond that one endpoint. They share this repo's
transport, retry policy and metrics (`internal/ingest`, `internal/pusher`, `internal/metrics`)
and differ only in what they read and which environment variables they require.

Everything below describes the Kubernetes agent unless it says otherwise; the LiteLLM one has
its own section.

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

Shared by both binaries:

- `internal/pusher` — the ticker loop, in-process retry/backoff, and the `Source` interface each
  collector implements. Its real subject is the failure policy (which errors are retryable, that
  a 4xx must not consume the backoff budget, that an empty result is never pushed as an empty
  batch), which is identical for every source and is exactly what drifts when a second agent
  copies it instead of sharing it.
- `internal/ingest` — the HTTP transport: auth header, status-code rules, retryable-vs-not, and
  the `Response` shape. Deliberately knows nothing about what it is sending.
- `internal/config` — env-var loading and validation (12-factor, no config file). `Common` plus
  one loader per binary, since the required variables genuinely differ.
- `internal/metrics` — the hand-rolled `/metrics` (Prometheus text format) and `/healthz`
  handlers.

Kubernetes:

- `cmd/plutus-collector` — entrypoint.
- `internal/opencost` — the OpenCost `/allocation` HTTP client.
- `internal/k8scost` — the Plutus batch JSON shape (a fixed contract with the backend's ingest
  route), the OpenCost-row → batch-row mapping, and the `Source` implementation.

LiteLLM:

- `cmd/plutus-litellm-collector` — entrypoint.
- `internal/litellm` — the proxy's spend-log client, the request-log → daily-tuple aggregation,
  and the `Source` implementation.

## LiteLLM collector

Reads your LiteLLM proxy's own spend records and pushes a daily aggregate to Plutus. What that
buys you is attribution a provider invoice structurally cannot give: OpenAI can tell you an API
key spent $400, but not which of your teams or services spent it. Your gateway knows.

```
docker run -d --name plutus-litellm-collector \
  -e PLUTUS_API_KEY=<key minted in Plutus> \
  -e LITELLM_BASE_URL=http://litellm:4000 \
  -e LITELLM_MASTER_KEY=<a LiteLLM key with admin scope> \
  ghcr.io/plutus-cloud/plutus-litellm-collector:latest
```

`LITELLM_BASE_URL` may be a plain `http://` internal address — it never leaves your network.
`PLUTUS_INGEST_URL` may not: it carries a Bearer API key and is rejected unless it is `https://`.

| Variable | Required | Default |
|---|---|---|
| `PLUTUS_API_KEY` | yes | — |
| `LITELLM_BASE_URL` | yes | — |
| `LITELLM_MASTER_KEY` | yes | — |
| `PLUTUS_INGEST_URL` | no | `https://console.plutus-cloud.com/api/ingest/litellm-cost` |
| `PUSH_INTERVAL_MINUTES` | no | `1440` (daily) |
| `METRICS_ADDR` | no | `:9100` |

Note there is no `CURRENCY` here, unlike the Kubernetes agent. LiteLLM prices from a
USD-denominated model price map, so Plutus asserts the currency itself rather than asking you for
a value nothing would check.

### What it sends

One row per `(model, upstream provider, virtual key, team)` per UTC day:

```json
{"rows": [
  {"date": "2026-08-28", "model": "gpt-4o", "provider": "openai",
   "virtual_key": "checkout-svc", "team": "platform",
   "spend": 1.5, "input_tokens": 100, "output_tokens": 20}
]}
```

**Per-request data never leaves your network.** The collector reads request-level spend logs
locally and reduces them to those daily tuples before pushing. It reads request logs rather than
LiteLLM's own pre-aggregated daily endpoints because those break down one dimension at a time —
spend by model, spend by key — and not the combination, which is what Plutus needs to make every
breakdown add up to the same total.

### Spend for a provider you already track directly

If you also have Plutus's OpenAI or Anthropic connector enabled, the same dollars would arrive
twice — once as that provider's invoice, once as the gateway's estimate of the same traffic. So
Plutus does **not** count a gateway provider's spend until you decide which figure your totals
should use. New providers show up in the LiteLLM cost source's settings marked as awaiting a
decision; nothing is counted, and nothing is silently double-counted, in the meantime.

Token counts are recorded either way, so a provider whose costs you track through its own
connector still shows per-team and per-key usage in Plutus.

### Status

**The LiteLLM admin API this reads has not yet been verified against a live instance.** Unlike
the OpenCost client — checked against OpenCost's published `swagger.json` — the field names in
`internal/litellm/client.go` are read optimistically from LiteLLM's documentation, and that
file's header lists exactly what still needs confirming. Expect to hit issues; please report
them.

## Support

Questions or issues installing or running this agent: [contact Plutus support](https://plutus-cloud.com)
or open an issue in this repo.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
