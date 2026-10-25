# plutus-collector

Small Go agents you run inside your own infrastructure to send cost data to
[Plutus](https://plutus-cloud.com). Three of them, from this one repo:

| Binary | Reads | Pushes to |
|---|---|---|
| `plutus-collector` | [OpenCost](https://www.opencost.io/)'s local `/allocation` API, in your Kubernetes cluster | `POST /api/ingest/kubernetes-cost` |
| `plutus-litellm-collector` | your self-hosted [LiteLLM](https://docs.litellm.ai/) proxy's admin spend API | `POST /api/ingest/litellm-cost` |
| `plutus-bifrost-collector` | your self-hosted [Bifrost](https://github.com/maximhq/bifrost) gateway's `/api/logs` | `POST /api/ingest/bifrost-cost` |

All three exist for the same reason: the system being measured runs **inside your network**, where
nothing of Plutus's can reach it. So the agent runs on your side and pushes out, rather than
Plutus polling in — which is also why none of them needs you to open anything inbound.

Each communicates over one fixed HTTP/JSON contract with an API key you mint in Plutus — no
access to anything else in your account beyond that one endpoint. They share this repo's
transport, retry policy and metrics (`internal/ingest`, `internal/pusher`, `internal/metrics`)
and differ only in what they read and which environment variables they require.

Each has its own section below; what they genuinely share — security posture, the `/metrics` and
`/healthz` endpoints, and how to develop on them — is under "All collectors".

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

## Kubernetes collector (OpenCost)

Everything in this section is specific to `plutus-collector`, the Kubernetes agent. The LiteLLM
collector has its own section below; what the two genuinely share is under "All collectors".

### Prerequisites

- A Kubernetes cluster (1.24+ is a safe assumption; nothing here needs anything exotic).
- Helm 3.
- **A Prometheus-compatible store that OpenCost can query, and that scrapes OpenCost.** OpenCost
  computes costs from time series (container usage and requests, node prices, labels) that live
  in Prometheus, and it does not store them itself. This chart installs OpenCost but **does not
  install a Prometheus**. OpenCost's default endpoint is
  `http://prometheus-server.prometheus-system.svc.cluster.local:80`; on a cluster without one
  OpenCost never becomes ready and nothing is pushed. Where a Prometheus does answer but does not
  scrape OpenCost's own `:9003` metrics, OpenCost is ready but has no cost data. Either way the
  agent now says so: its pod stays `0/1` Ready, `plutus_collector_last_push_success` is `0` and
  the log reports the empty window.
- Either let this chart install OpenCost for you (default), or already run OpenCost yourself.
- A Plutus account with the Kubernetes cost source added. In the Plutus UI: **Cost Sources →
  Kubernetes → Generate API Key** — this mints the key and shows you the exact `helm install`
  command below with your key already filled in. Generating or revoking this key requires an
  account admin. You can also mint one via the API:
  `POST /api/accounts/:accountId/cost-sources/kubernetes/api-key`.

### Quickstart

```bash
helm install plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector \
  --namespace plutus-collector --create-namespace \
  --set clusterName=prod-us-east-1 \
  --set currency=USD \
  --set apiKey=<the key from the Plutus UI>
```

This deploys OpenCost (bundled subchart) plus this pusher. It works when the cluster already has a
Prometheus that scrapes OpenCost (see Prerequisites); point OpenCost at yours with the
subchart's `opencost.opencost.prometheus.*` values. Cost data reaches Plutus after the first
complete UTC day, on the daily push (01:30 UTC plus a random delay of up to 20 minutes).

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

#### Uninstalling

```bash
helm uninstall plutus-collector --namespace plutus-collector
```

Revoking the API key in Plutus (Cost Sources → Kubernetes → Revoke) stops the agent from being
able to push immediately, independent of whether you've uninstalled the chart yet — useful if
you need to cut access off right away and tear down the cluster resources afterward.

### Configuration reference

The agent itself reads only environment variables (12-factor, no config file); the Helm chart's
`values.yaml` maps to them 1:1. Values on the left, the container env var they set on the right.

| `values.yaml` key                | env var                 | required | default                                                        |
|-----------------------------------|--------------------------|----------|-----------------------------------------------------------------|
| `clusterName`                     | `CLUSTER_NAME`           | **yes**  | none — install fails validation if left as the empty placeholder |
| `currency`                        | `CURRENCY`               | **yes**  | none — never defaults to USD; see "Currency" below               |
| `apiKey` / `existingSecret`+`existingSecretKey` | `PLUTUS_API_KEY` (via Secret) | **yes** | none |
| `plutusIngestUrl`                 | `PLUTUS_INGEST_URL`      | no       | `https://console.plutus-cloud.com/api/ingest/kubernetes-cost`   |
| `pusher.schedule.utcTime`         | `PUSH_SCHEDULE_UTC`      | no       | `01:30` (UTC, once a day, and at startup)                        |
| `pusher.schedule.jitterMinutes`   | `PUSH_JITTER_MINUTES`    | no       | `20` (random delay added to the daily time)                      |
| `pusher.catchUpDays`              | `CATCH_UP_DAYS`          | no       | `3` (closed UTC days covered by each push)                       |
| `pusher.emptyResult`              | `EMPTY_RESULT`           | no       | `fail` (`ignore` treats a window with no cost rows as normal)    |
| `pushIntervalMinutes` (deprecated) | `PUSH_INTERVAL_MINUTES` | no       | unset; when set, replaces the daily schedule with a fixed interval |
| n/a — derived from `opencost.enabled`/`opencost.endpoint` | `OPENCOST_URL` | no | bundled subchart's in-cluster Service when `opencost.enabled=true`, otherwise `opencost.endpoint` |
| `metricsPort` / `service.port`    | `METRICS_ADDR`           | no       | `9100`                                                            |

Retry count and backoff on a failed push are fixed constants, not currently exposed as
`values.yaml` knobs — they haven't needed to be configurable yet.

#### Currency

OpenCost does not report its own currency anywhere in its allocation output — it's a cluster-side
configuration value with no API field to read it back from. Guessing `USD` would silently
mislabel every figure, so `currency`/`CURRENCY` has no default anywhere in this component — set
it to whatever ISO 4217 code OpenCost was configured with.

### What this pushes

Once a day at the scheduled time (and when it starts), the agent queries OpenCost for each of the
last `catchUpDays` closed UTC days, grouped by namespace/controller/pod/container, and POSTs a
batch per day like the one below. Covering several days is what lets a day that was missed or
came back empty fill in later; Plutus replaces a day's rows on each push, so re-sending is safe.
Within one process a day that was already delivered is not queried again.

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

#### When a day comes back empty

A cluster always runs at least its own system pods, so a day with no priced allocations means the
data behind OpenCost is missing, not that nothing ran. The agent does not treat that as a
successful push of nothing:

- it logs a warning (an error from the second empty day in a row),
- `plutus_collector_last_push_success` is `0` and `plutus_collector_empty_results_total` and
  `plutus_collector_consecutive_empty_windows` count it,
- `/readyz` returns 503, so `kubectl get pods` shows `0/1` (`/healthz` stays 200: a data problem
  never restarts the pod), and
- it still POSTs a **heartbeat** for the newest window, `rows: []` plus a `collector` block, so
  Plutus can tell an agent that is running but has no data from one that is not running:

```json
{
  "cluster_name": "prod-us-east-1",
  "currency": "USD",
  "rows": [],
  "collector": {
    "version": "v1.3.0",
    "prometheus_mode": "external",
    "window": { "start": "2026-08-10T00:00:00Z", "end": "2026-08-11T00:00:00Z" },
    "empty_reason": "no_allocations"
  }
}
```

A heartbeat writes and deletes no cost rows. `EMPTY_RESULT=ignore` turns the failure behaviour
off for clusters where an empty day really is normal; the heartbeat is still sent.

#### Preflight checks

The agent checks the data behind OpenCost at startup, before each push and every 15 minutes: the
metrics store answers, OpenCost's series are in it, container usage is present, the store holds
one cluster, it holds enough history, and OpenCost computes allocations from all of that. Results
are logged with the fix (`check=<id> remedy=...`), exported as
`plutus_collector_preflight_ok{check="<id>"}`, sent to Plutus in the heartbeat's `preflight` block,
and any check at `error` severity makes `/readyz` return 503. Run them by hand:

```bash
helm test plutus-collector -n plutus-collector
kubectl -n plutus-collector exec deploy/plutus-collector -- /plutus-collector preflight
```

Check ids: `prometheus_reachable`, `opencost_series`, `allocation_series`, `pods_running`,
`pod_labels`, `namespace_labels`, `controller_labels`, `usage_series`, `cluster_ambiguity`,
`history`, `opencost_reachable`, `allocation_nonempty`, `pricing`. The agent finds the store from
the same `opencost.opencost.prometheus.*` values OpenCost uses (override with
`preflight.prometheusUrl`). `cluster_ambiguity` blocks the push by default: when one store holds
several clusters and nothing tells them apart, their costs would be attributed to this cluster.
Set `preflight.clusterLabel` and configure OpenCost's `PROM_CLUSTER_ID_LABEL` to the same label.

A window that ended before the store's first OpenCost sample (the day an install happened) is
reported as `empty_reason: "no_history"` and is not a fault.

#### One-off pushes

`plutus-collector --once` does one pass and exits non-zero if any window failed or came back
empty. `--window=<start>,<end>` (RFC3339) pushes that window instead of the recent days:

```bash
kubectl -n plutus-collector exec deploy/plutus-collector -- \
  /plutus-collector --once --window=2026-10-24T00:00:00Z,2026-10-25T00:00:00Z
```

Rows are dated by the start's UTC day and a push replaces that whole day in Plutus, so backfill
whole UTC days; a shorter window replaces the day with a partial one.

## LiteLLM collector

`plutus-litellm-collector` reads your LiteLLM proxy's own spend records and pushes a daily
aggregate to Plutus. What that buys you is attribution a provider invoice structurally cannot
give: OpenAI can tell you an API key spent $400, but not which of your teams or services spent
it. Your gateway knows.

### Prerequisites

- A LiteLLM proxy **with spend logging persisted to a database**. LiteLLM only keeps spend
  records when it is configured with a database; a proxy running without one serves traffic
  perfectly well and has nothing for this collector to read. If your first push reports zero
  rows, check this first.
- A LiteLLM key with **admin scope** — the master key, or a key created with admin permissions.
  An ordinary virtual key authenticates fine for inference and returns `401` on every admin
  route, which is the most common misconfiguration here.
- Somewhere to run a container that can reach the proxy over your own network. That is usually
  the same host or cluster the proxy runs on — it does **not** need to be reachable from the
  internet.
- A Plutus account with the LiteLLM cost source added. In the Plutus UI: **Cost Sources →
  LiteLLM → Generate API Key**, which mints the key and shows the `docker run` below with it
  already filled in. Generating or revoking it requires an account admin. Via the API:
  `POST /api/accounts/:accountId/cost-sources/litellm/api-key`.

### Quickstart

```bash
docker run -d --name plutus-litellm-collector \
  -e PLUTUS_API_KEY=<the key from the Plutus UI> \
  -e LITELLM_BASE_URL=http://litellm:4000 \
  -e LITELLM_MASTER_KEY=<a LiteLLM key with admin scope> \
  ghcr.io/plutus-cloud/plutus-litellm-collector:main
```

`:main` is this repo's "latest published release" tag — release.yml pushes it alongside the
version tag on every release, and it is the tag the nightly vulnerability scan actually scans.
There is deliberately no `:latest`. For production, pin the version tag (`:v1.2.3`) and verify
its signature; see [SECURITY.md](SECURITY.md).

Docker Compose, as a service alongside an existing LiteLLM:

```yaml
  plutus-litellm-collector:
    image: ghcr.io/plutus-cloud/plutus-litellm-collector:main
    restart: unless-stopped
    environment:
      PLUTUS_API_KEY: ${PLUTUS_API_KEY}
      LITELLM_BASE_URL: http://litellm:4000
      LITELLM_MASTER_KEY: ${LITELLM_MASTER_KEY}
```

**On Kubernetes**: there is no Helm chart for this one yet — the chart in `chart/` installs the
OpenCost agent only. Run it as an ordinary one-replica `Deployment` with the same environment
variables, pointing `LITELLM_BASE_URL` at your LiteLLM `Service`. One replica, for the same
reason the Kubernetes agent is fixed at one: a second would push the same batch again, which the
backend's idempotent ingest makes harmless but pointless.

### Configuration reference

Shared variables (`PLUTUS_INGEST_URL`, `PUSH_INTERVAL_MINUTES`, `METRICS_ADDR`) behave exactly as
they do for the Kubernetes agent.

| env var | required | default |
|---|---|---|
| `PLUTUS_API_KEY` | **yes** | none — it is a credential |
| `LITELLM_BASE_URL` | **yes** | none — there is no conventional address for a LiteLLM proxy |
| `LITELLM_MASTER_KEY` | **yes** | none — must have admin scope |
| `PLUTUS_INGEST_URL` | no | `https://console.plutus-cloud.com/api/ingest/litellm-cost` |
| `PUSH_INTERVAL_MINUTES` | no | `1440` (daily) |
| `METRICS_ADDR` | no | `:9100` |

`LITELLM_BASE_URL` may be a plain `http://` internal address — that request never leaves your
network. `PLUTUS_INGEST_URL` may not: it carries a Bearer API key, and a non-`https` value is
rejected at startup.

**There is no `CURRENCY` here**, unlike the Kubernetes agent. LiteLLM prices from a
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

Where a record carries no virtual-key alias, the key hash is sent instead: a poor label, but a
stable identity, and better than attributing the spend to nobody.

### Optional: label spend by activity (`activity`)

Off by default. When enabled, the collector reads each request's tags and, where **exactly one**
tag starts with a prefix you choose, sends the rest of that tag as the row's `activity`, so
Plutus can break spend down by what the calls were *for* (`code-review`, `support-triage`, ...).
The label is whatever your own callers attached; Plutus never infers it.

```bash
# any one of these turns it on
plutus-litellm-collector --activity-tag-prefix=activity:
ACTIVITY_TAG_PREFIX=activity:      # same, via the environment (the flag wins if both are set)
ACTIVITY_TAGS=true                 # on, with the conventional prefix "activity:"
```

Callers tag a request in LiteLLM the usual way, e.g. `"metadata": {"tags": ["activity:code-review"]}`.

- **Zero or several matching tags: the field is omitted** for that request. The collector never
  picks one of several and never splits spend between them.
- `activity` is part of the daily key, so requests that differ only in activity are separate
  rows; totals are unchanged.
- With the flag unset, payloads are byte-identical to earlier versions (the field is not
  serialised at all).
- Enabling it affects future pushes only; a push replaces its day, so history is not relabelled.
- Plutus normalises the value (lowercase, `[a-z0-9._-]`, max 64 chars), ignores values that do not
  fit, and admits at most 50 distinct values per push; the collector logs a warning when any are
  dropped. Spend is never dropped. Plutus shows a label only once a cost-tag rule maps it.

**What is confirmed about the tag field.** LiteLLM's docs say tags are *sent* as `metadata.tags`
(or the `x-litellm-tags` header) and *stored* in the spend log as `request_tags`, and its source
types that field as a list on the in-memory payload and as a JSON string on the stored row. The
collector accepts a list or a JSON-string list from `request_tags` and falls back to
`metadata.tags`. **Not confirmed:** that `GET /spend/logs/v2` actually returns `request_tags` on
every LiteLLM version (its documented example response omits it), because this was checked
against docs and source, not a live proxy. If your version omits it, the flag is harmless and
every row simply goes unlabelled.

### Spend for a provider you already track directly

If you also have Plutus's OpenAI or Anthropic connector enabled, the same dollars would arrive
twice — once as that provider's invoice, once as the gateway's estimate of the same traffic. So
Plutus does **not** count a gateway provider's spend until you decide which figure your totals
should use. New providers appear in the LiteLLM cost source's settings marked as awaiting a
decision; nothing is counted, and nothing is silently double-counted, in the meantime.

Token counts are recorded either way, so a provider whose costs you track through its own
connector still shows per-team and per-key usage in Plutus.

### Troubleshooting

| Symptom | Cause |
|---|---|
| `LiteLLM rejected the master key with HTTP 401/403` | `LITELLM_MASTER_KEY` is an ordinary virtual key. Admin scope is required for `/spend/logs`. |
| Push succeeds, `row_count` is 0 every day | The proxy is not persisting spend logs — see Prerequisites — or genuinely had no traffic in the window. |
| Model names look like `openai/gpt-4o` | The proxy reported no `model_group` for those calls, so the provider-qualified target is used as a fallback. |
| Rows appear in Plutus but spend is $0 | Providers are still awaiting a decision. See the section above. |
| Spend shows under a provider named `unknown` | The proxy's records carried no provider field. Decide about it like any other provider; the spend is real. |
| `PLUTUS_INGEST_URL must use https` at startup | It carries a Bearer key and is never allowed in cleartext. |

### Status

Verified against a running LiteLLM proxy and the OpenAPI document it serves — see
`internal/litellm/client.go`'s header for what that check found and corrected. It has not yet run
against a production gateway with a large history, so if paging or a field behaves differently at
volume, please report it.

## Bifrost collector

`plutus-bifrost-collector` reads your [Bifrost](https://github.com/maximhq/bifrost) gateway's
request logs and pushes a daily aggregate to Plutus: spend and tokens by upstream provider, model,
virtual key and team (or customer).

> **Not yet verified against a real Bifrost.** This collector was written from Bifrost's public
> source and docs and tested against a fake Bifrost server and a fake Plutus endpoint. It has
> never read a live Bifrost. The route, query parameters and record field names below are what
> Bifrost's source says; the exact token field names, the management API's authentication on your
> deployment, and the largest page size it honours are unconfirmed. It is built to fail small
> rather than wrong: it reads fields defensively, reports counts of anything unexpected, and every
> field name is overridable. If you run it against a real Bifrost, a report of what differed is
> very welcome.

### Prerequisites

- A Bifrost gateway **with log storage enabled** (the logs store is what `/api/logs` reads) and
  retention long enough to still hold yesterday.
- A container that can reach Bifrost over your own network. It does **not** need to be reachable
  from the internet.
- Bifrost management-API credentials **if you turned auth on** (see below).
- A Plutus account with the Bifrost cost source added, and a push API key minted for it (an
  account admin does this; a key minted for a different cost source is refused with `403`).

### Quickstart

```bash
docker run -d --name plutus-bifrost-collector \
  -e PLUTUS_API_KEY=<the key from the Plutus UI> \
  -e BIFROST_BASE_URL=http://bifrost:8080 \
  ghcr.io/plutus-cloud/plutus-bifrost-collector:main
```

As with the LiteLLM image, pin the version tag in production and verify its signature
([SECURITY.md](SECURITY.md)). There is no Helm chart for it; on Kubernetes run it as a
one-replica `Deployment` with the same environment variables.

### Configuration reference

Shared variables (`PLUTUS_INGEST_URL`, `PUSH_INTERVAL_MINUTES`, `METRICS_ADDR`) behave as they do
for the other collectors. There is no command-line flag; everything is an environment variable.

| env var | required | default |
|---|---|---|
| `PLUTUS_API_KEY` | **yes** | none (credential) |
| `BIFROST_BASE_URL` | **yes** | none; `http://` allowed (internal address) |
| `PLUTUS_INGEST_URL` | no | `https://console.plutus-cloud.com/api/ingest/bifrost-cost` (must be `https`) |
| `BIFROST_API_KEY` | no | none; sent as `<scheme> <key>` in `BIFROST_API_KEY_HEADER` |
| `BIFROST_API_KEY_HEADER` | no | `Authorization` |
| `BIFROST_API_KEY_SCHEME` | no | `Bearer` for `Authorization`; none (raw value) for any other header |
| `BIFROST_BASIC_USER` / `BIFROST_BASIC_PASSWORD` | no | none; use instead of `BIFROST_API_KEY`, not with it |
| `BIFROST_FIELD_<NAME>` | no | built-in defaults; see below |
| `PUSH_INTERVAL_MINUTES` | no | `1440` |
| `METRICS_ADDR` | no | `:9100` |

Base URLs (and `PLUTUS_INGEST_URL`) may not contain `user:password@`; that is rejected at startup,
because URLs end up in logs. Credentials go in the variables above.

**Authentication is configuration because it is unconfirmed.** Bifrost's management API is the
dashboard's own and is unauthenticated unless the operator enables auth; how a given deployment
authenticates it was not confirmed, so the collector supports a header (any name, any scheme) or
HTTP basic auth and sends nothing at all by default. A `401/403` produces an error naming these
variables.

**Field names.** `BIFROST_FIELD_<NAME>` replaces the candidate paths for one value with a
comma-separated list of dotted paths (first one that yields a value wins).
`<NAME>` is one of `TIMESTAMP`, `STATUS`, `PROVIDER`, `MODEL`, `VIRTUAL_KEY`, `TEAM`, `COST`,
`INPUT_TOKENS`, `OUTPUT_TOKENS`. Defaults:

| value | candidate paths |
|---|---|
| timestamp / status | `timestamp` / `status` |
| provider / model | `provider` / `model` |
| virtual key | `virtual_key_name`, `virtual_key_id` |
| team | `team_name`, `customer_name`, `team_id`, `customer_id` |
| cost | `cost` |
| input tokens | `token_usage.prompt_tokens`, `token_usage.input_tokens`, `prompt_tokens` |
| output tokens | `token_usage.completion_tokens`, `token_usage.output_tokens`, `completion_tokens` |

A Bifrost virtual key belongs to a team **or** a customer, never both; either fills the single
`team` field Plutus has.

### What it reads and sends

Each cycle it asks for **yesterday (UTC)** only:
`GET /api/logs?start_time=...&end_time=...&status=success&limit=1000&offset=N&sort_by=timestamp&order=asc`,
paging until a short page. It reads `GET /api/logs/stats` and `GET /api/logs/dropped` as a
cross-check only. It reduces the records in memory to one row per UTC day,
`(provider, model, virtual key, team)`, and pushes:

```json
{"rows": [
  {"date": "2026-10-16", "provider": "anthropic", "model": "claude-sonnet-4-5",
   "virtual_key": "vk-checkout", "team": "platform",
   "spend": 12.5, "input_tokens": 1000, "output_tokens": 200}
]}
```

- **Provider ids are sent exactly as Bifrost reports them**, un-normalised; Plutus keeps a
  per-provider decision (count it, or treat it as covered by a direct connector) and holds the
  dollars until someone decides. The collector logs `held_providers` when Plutus reports that.
- **Per-request data never leaves your network, and no prompt or response content is parsed or
  retained.** Only the handful of fields above are extracted from each record; keys, prompts and
  record content are never logged, and HTTP error bodies from Bifrost are not echoed.
- **Cost is Bifrost's own estimate**, not an invoice, and Bifrost can recalculate history; Plutus
  replaces the whole day on every push, so a re-push corrects it.
- **Window safety.** Records whose timestamp falls outside the requested day (a server ignoring
  the time filter) are skipped and counted in the log rather than pushed under the wrong date.
- **Reconciliation, advisory only.** If the sum it built differs from Bifrost's own `total_cost`
  for the day, or Bifrost reports dropped (unlogged) requests, it logs a warning with the numbers.
  It never fails the push over this.
- A day with no records (including logs already purged by Bifrost's retention) pushes nothing and
  leaves Plutus's existing rows for that day alone.

### Troubleshooting

| Symptom | Cause |
|---|---|
| `Bifrost rejected the request ... HTTP 401/403` | Auth is enabled on the management API; set `BIFROST_API_KEY*` or `BIFROST_BASIC_*`. |
| Every day pushes nothing | Log storage is off, retention has purged the day, or there was no traffic. |
| Tokens are 0 but spend is not | Your Bifrost nests usage differently; set `BIFROST_FIELD_INPUT_TOKENS`/`_OUTPUT_TOKENS`. |
| Spend is 0 for everything | The cost field is named differently; set `BIFROST_FIELD_COST`. |
| Log: `records outside the requested day` | This Bifrost version is not honouring `start_time`/`end_time`. Report it. |
| Rows appear in Plutus but spend is $0 | Providers are awaiting a decision in the Bifrost cost source's settings. |

## All collectors

### Security & permissions

Common to all: the only thing that leaves your network is the JSON batch documented above, over
HTTPS, to the `PLUTUS_INGEST_URL` you configure. No collector opens a listening port beyond
its own `/metrics` and `/healthz`, and neither needs any inbound access from Plutus.

The LiteLLM collector additionally holds a LiteLLM **admin-scope key** and reads request-level
spend records, which can carry user and metadata fields. Those records are read over your own
network and reduced to daily totals in-process — they are never written to disk and never sent
to Plutus. Treat that key like any other admin credential; it is the most privileged thing the LiteLLM
collector holds. The Bifrost collector holds whatever management-API credential you give it (often
none) and reads request-level logs the same way: in memory, reduced in-process, never written to
disk, never sent.

Kubernetes agent specifics:

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
### How do I verify it's working?

All collectors expose the same endpoints and emit the same metric names, on `METRICS_ADDR`
(`:9100` by default).

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
   For the LiteLLM collector outside Kubernetes, that is just
   `curl localhost:9100/metrics` against the container's published port.
3. **The Plutus UI** — the cost source's connection panel shows "Receiving data" with a
   last-push timestamp once the first push succeeds.
4. **Logs** (`kubectl logs`, or `docker logs plutus-litellm-collector`) — structured JSON, one
   object per line, so your own log aggregation can parse it directly. Every push cycle logs a
   start line naming the source, the row count about to be pushed, and either a success (with
   `accepted`/`rejected` counts) or a failure (with the error).

#### Scraping `/metrics`

For the Kubernetes agent, this chart deploys a plain `Service` (`service.enabled`, default `true`) rather than a
`prometheus-operator` `ServiceMonitor` CRD — we can't assume every cluster has that CRD
installed, and a plain Service degrades gracefully (any scraper can find it via Service
discovery) where a `ServiceMonitor` would fail to apply at all without the CRD. If you do run
prometheus-operator, add your own `ServiceMonitor` pointed at this Service's `metrics` port
(`9100` by default).

The LiteLLM collector ships no chart, so scraping it is however you scrape any other container
you run — publish or expose `METRICS_ADDR` and point your scraper at it.

### Development

```bash
go build ./...
go test ./...
helm lint chart/

# Two images from one Dockerfile. Stage order is load-bearing: the Kubernetes image is
# deliberately last, so a build with no --target still produces what it always did.
docker build -t plutus-collector:dev .
docker build --target litellm -t plutus-litellm-collector:dev .
docker build --target bifrost -t plutus-bifrost-collector:dev .
```

Conventions for commits, PR descriptions and merging are in [CONTRIBUTING.md](CONTRIBUTING.md).

Adding a third source: implement `pusher.Source`, put its payload shape and mapping in its own
`internal/` package beside its client, add a `Load<X>` to `internal/config`, and add a `cmd/`
entrypoint plus a Dockerfile stage. Deliberately **not** a `MODE` env var — the required
variables differ per source (`CLUSTER_NAME`/`CURRENCY` are mandatory for OpenCost and meaningless
for LiteLLM), and branching validation on a mode is what the one-loader-per-binary split exists
to avoid.

#### Package layout

Shared by all binaries:

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

Bifrost:

- `cmd/plutus-bifrost-collector` — entrypoint.
- `internal/bifrost` — the log-API client (paging, window and status handling, defensive field
  extraction with overridable paths), the log -> daily-tuple aggregation, reconciliation, and the
  `Source` implementation. Tested against `httptest` fakes of both Bifrost and the Plutus endpoint.

## Support

Questions or issues installing or running this agent: [contact Plutus support](https://plutus-cloud.com)
or open an issue in this repo.

**Security issues go to `security@plutus-cloud.com`, not a public issue** — see
[SECURITY.md](SECURITY.md), which also covers how to verify the signatures on what you install.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
