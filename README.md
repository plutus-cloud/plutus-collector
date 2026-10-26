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

Everything in this section is specific to `plutus-collector`, the Kubernetes agent. The LiteLLM and
Bifrost collectors have their own sections below; what the three share is under "All collectors".

### What gets installed

`helm install` puts three things in your cluster, in one namespace:

| Component | What it is | Why |
|---|---|---|
| `plutus-collector` (the pusher) | this repository's agent, one pod | queries OpenCost once a day and pushes the result to Plutus |
| OpenCost | one pod, rendered by this chart | computes cost per namespace, workload, pod and container |
| a bundled Prometheus | one pod, rendered by this chart | the time-series store OpenCost reads from; it scrapes OpenCost and the kubelet's cAdvisor endpoint and keeps four days |

OpenCost does not store the data it computes costs from. It reads container usage, requests, node
prices and labels from a Prometheus-compatible store, so something has to provide one. By default
this chart does, which is why a cluster with nothing pre-installed works. If you already run a
Prometheus (or Mimir, Thanos, Grafana Cloud), you can point OpenCost at it instead:
[Use your own Prometheus](#use-your-own-prometheus).

There is no node-exporter and no kube-state-metrics (OpenCost emits the series it needs itself), so
the bundled store is one pod per cluster, not one per node. The chart does not use the upstream
OpenCost or Prometheus charts and installs no CRDs. It does not install the OpenCost UI or its MCP
server.

### Prerequisites

- A Kubernetes cluster, 1.24 or later, and Helm 3.
- Nothing else for the default install. For `prometheus.mode=external`, a Prometheus-compatible
  store with a PromQL query API.
- A Plutus account with the Kubernetes cost source added. In the Plutus UI: **Cost Sources →
  Kubernetes → Generate API Key**, which mints the key and shows the `helm install` command below
  with your key filled in. Generating or revoking it requires an account admin. Via the API:
  `POST /api/accounts/:accountId/cost-sources/kubernetes/api-key`.
- Egress from the cluster to `console.plutus-cloud.com` on 443, and to OpenCost's cloud pricing
  sources if you want provider price lists (see Pricing).

### Quickstart

```bash
helm install plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector \
  --namespace plutus-collector --create-namespace \
  --set clusterName=prod-us-east-1 \
  --set currency=USD \
  --set apiKey=<the key from the Plutus UI>
```

Then check it:

```bash
helm test plutus-collector --namespace plutus-collector     # runs the preflight checks
kubectl -n plutus-collector get pods                         # pusher 1/1 when its data checks pass
```

**When data appears.** OpenCost and the bundled Prometheus start collecting when they start. The
agent pushes closed UTC days (01:30 UTC plus a random delay of up to 20 minutes), so the first
complete day appears the day after install. A day the install happened part-way through is pushed
as a partial day. Until then a pod that reports no data for a day that predates the install is
expected and is not marked unready (`empty_reason: "no_history"`).

**Footprint.** One extra pod for the bundled Prometheus (requests `50m` CPU and `256Mi`, limit
`1Gi`) and one for OpenCost (`10m` and `128Mi`, limit `1Gi`), plus the pusher (`10m`, `32Mi`, limit
`64Mi`). These are the chart's resource settings. Measured once, in the kind test on one amd64
node with about 120 containers running (100 of them idle `pause` containers) and a 60-second scrape
interval: the bundled Prometheus's working set was 132 to 148 MiB with 1,863 series, and OpenCost's
66 to 73 MiB, over two runs. That is one machine, not a sizing guide: usage grows with the number of
containers, and 1,000 or 10,000 containers have not been measured. `test/e2e/run.sh` prints the same
figures so you can see them on your own runner.

Prefer not to put your API key in Helm values/history? Create the Secret yourself first:

```bash
kubectl create secret generic plutus-collector-key \
  --namespace plutus-collector \
  --from-literal=api-key=<the key from the Plutus UI>

helm install plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector \
  --namespace plutus-collector --create-namespace \
  --set clusterName=prod-us-east-1 --set currency=USD \
  --set existingSecret=plutus-collector-key
```

The agent image, and the OpenCost and Prometheus images the chart pins, are multi-architecture
(amd64 and arm64), so the chart can run on Graviton and Ampere nodes. The kind end-to-end test runs
on both architectures in CI.

#### Where the Prometheus data lives

By default the bundled Prometheus keeps its data in an `emptyDir`. That survives the container
restarting but not the pod being rescheduled to another node, which loses the history it holds:
the preflight reports the shorter history, and days that were not yet pushed are partial. To keep
it, use a volume:

```bash
--set prometheus.bundled.storage.type=pvc --set prometheus.bundled.storage.size=10Gi
```

The pod then stays `Pending` on a cluster that has no default StorageClass; set
`prometheus.bundled.storage.storageClass`. Retention is four days (`prometheus.bundled.retention`),
which the chart requires to be at least `pusher.catchUpDays + 1` whole days.

#### Pricing

OpenCost prices CPU and memory from your cloud provider's price list when it can identify the
provider. On a cluster it cannot identify (on-premises or bare metal, for example) it falls back to
built-in default rates, so the dollar figures are an estimate shaped like a public-cloud price
list. The preflight warns (`pricing`) when that is the case. Set your own rates:

```yaml
opencost:
  pricing:
    provider: custom
    custom:
      CPU: "0.012"      # per core-hour
      RAM: "0.0016"     # per GB-hour
      storage: "0.00005"
```

An air-gapped cluster that cannot reach the pricing sources behaves the same way.

### Use your own Prometheus

Set `prometheus.mode=external` and give OpenCost the store's query URL:

```bash
helm install plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector \
  --namespace plutus-collector --create-namespace \
  --set clusterName=prod-us-east-1 --set currency=USD --set apiKey=<key> \
  --set prometheus.mode=external \
  --set prometheus.external.url=http://prometheus-server.monitoring.svc:80
```

The chart then renders no Prometheus, and the store has to hold what OpenCost needs. This is the
contract:

1. **Everything OpenCost serves on `:9003/metrics`.** Scrape the `<release>-opencost` Service with
   `honor_labels: true` at 60 seconds or faster. If you filter, keep at least:
   `container_cpu_allocation`, `container_memory_allocation_bytes`, `container_gpu_allocation`,
   `node_cpu_hourly_cost`, `node_ram_hourly_cost`, `node_gpu_hourly_cost`,
   `node_total_hourly_cost`, `kubecost_node_is_spot`, `kube_pod_container_status_running`,
   `kube_pod_container_resource_requests`, `kube_pod_labels`, `kube_namespace_labels`,
   `kube_pod_owner`, `deployment_match_labels`, `statefulSet_match_labels`,
   `service_selector_labels`, `pv_hourly_cost`, `kubecost_pv_info`, `pod_pvc_allocation`,
   `kube_persistentvolumeclaim_info`, `kube_persistentvolume_capacity_bytes`,
   `kube_persistentvolumeclaim_resource_requests_storage_bytes`, `kubecost_load_balancer_cost`.
2. **cAdvisor** (recommended): `container_cpu_usage_seconds_total` and
   `container_memory_working_set_bytes` with `container`, `pod` and `namespace` labels. Without
   them costs are based on requests only and a pod without requests costs nothing.
3. **Retention** of at least `pusher.catchUpDays + 1` days of raw (or 5-minute) data.
4. **One cluster, or a cluster label.** If the store holds more than one cluster, every series
   above must carry a label whose value equals `clusterName`; set `prometheus.external.clusterLabel`
   to its name and OpenCost filters every query to this cluster. Without it, other clusters'
   pods are attributed to this one, and the agent **refuses to push** (`cluster_ambiguity`).
5. **Do not drop OpenCost's kube-state-metrics-style series because you run kube-state-metrics.**
   kube-state-metrics v2 exports only allowlisted labels and has no `*_match_labels`; Plutus's tags
   and controller attribution come from OpenCost's copies. How OpenCost's label decoding behaves
   when both exist is not verified.

Optionally `--set prometheus.external.serviceMonitor.enabled=true` renders a `ServiceMonitor` for
OpenCost, only when the prometheus-operator CRD is present.

What has been run: the chart's own bundled Prometheus, and (as the failing case) a plain Prometheus
that does not scrape OpenCost, both in the kind test. Everything else in this table is derived from
the API shape and authentication each store uses, not from an install against it.

| Store | Works | How |
|---|---|---|
| Prometheus you run | yes | add the scrape job above |
| kube-prometheus-stack / prometheus-operator | yes | `serviceMonitor.enabled`, or your own |
| Grafana Cloud | yes, with an Alloy or Prometheus that ships OpenCost's series to it | URL `https://prometheus-<region>.grafana.net/api/prom`, `auth.type=basic` with the instance ID and a token that has `metrics:read`. Billed per active series |
| Mimir, Cortex | yes | `prometheus.external.tenantId` sets `X-Scope-OrgID`; the path is usually `/prometheus` |
| Thanos | yes | point at Query or Query Frontend; keep raw or 5-minute resolution for the last four days; extra variables through `opencost.extraEnv` |
| VictoriaMetrics, Google Managed Prometheus | probably | Prometheus-compatible query APIs; subquery behaviour not verified |
| Amazon Managed Prometheus | needs a SigV4 proxy | run an `aws-sigv4-proxy` sidecar through `opencost.extraContainers` (and IRSA) and point `url` at it |
| Azure Monitor managed Prometheus | not directly | needs an AAD-token proxy; OpenCost has no AAD auth |
| Remote-write-only or agent-mode Prometheus, Datadog, New Relic | no | OpenCost needs a PromQL query endpoint |

Recipes (untested sketches; adapt and verify against your store):

```yaml
# Amazon Managed Prometheus: a SigV4-signing proxy next to OpenCost, with IAM permission to query
# the workspace (for example through IRSA on opencost's service account), and the store URL pointed at it.
prometheus:
  mode: external
  external:
    url: http://localhost:8005/workspaces/<workspace-id>
opencost:
  extraContainers:
    - name: sigv4-proxy
      image: public.ecr.aws/aws-observability/aws-sigv4-proxy:<a version you have pinned>
      args: ["--name", "aps", "--region", "<region>", "--host", "aps-workspaces.<region>.amazonaws.com", "--port", ":8005"]
```

The agent's preflight cannot reach a store that is only reachable as `localhost` inside OpenCost's
pod, so set `preflight.enabled=false` for that case.

```yaml
# Thanos: the Query (or Query Frontend) endpoint is a Prometheus-compatible query API.
prometheus:
  mode: external
  external:
    url: http://thanos-query-frontend.monitoring.svc:9090
    clusterLabel: cluster      # if one Thanos serves several clusters
```

OpenCost's own Thanos settings (query offset, resolution) are not exposed as values; pass any you
need through `opencost.extraEnv`, using the names in OpenCost's documentation.

Authentication OpenCost supports natively: basic, bearer, mTLS and `X-Scope-OrgID`
(`prometheus.external.auth.type`: `none`, `basic`, `bearer`, `mtls`, always from a Secret you create,
never from values). Arbitrary headers are not supported; they need a proxy. The pusher's preflight
uses the same URL and credentials.

If you already run OpenCost too, `opencost.enabled=false` with `opencost.endpoint` renders no
OpenCost and no Prometheus; `prometheus.external.url` is then only where the preflight looks.

### Prometheus-less mode (experimental)

`prometheus.mode=none` uses OpenCost's built-in collector data source, which reads the kubelet itself
and needs no Prometheus. It is experimental because upstream's own documentation still describes a
Prometheus as required, and because it needs the `nodes/proxy` permission, which can be used to run
commands in any pod on the node. The chart therefore renders nothing for it until you set
`prometheus.none.acknowledgeNodesProxy=true`. Its state lives in an `emptyDir` or PVC
(`prometheus.none.storage`). Use `bundled` unless you have a reason not to.

### Configuration reference

`values.schema.json` validates every value at install time, so a mistyped key or a bad value fails
`helm install` and `helm lint` with the reason. The agent itself reads only environment variables;
the chart sets them. The keys you are most likely to touch:

| `values.yaml` key | default | meaning |
|---|---|---|
| `clusterName` | required | labels every row and is OpenCost's cluster id |
| `currency` | required | ISO 4217 code OpenCost is priced in; never defaulted |
| `apiKey` / `existingSecret` + `existingSecretKey` | required (one) | the Plutus API key |
| `plutusIngestUrl` | `https://console.plutus-cloud.com/api/ingest/kubernetes-cost` | must be https |
| `pusher.schedule.utcTime`, `.jitterMinutes` | `01:30`, `20` | daily push time (UTC) and random delay |
| `pusher.catchUpDays` | `3` | closed UTC days covered by each push |
| `pusher.emptyResult` | `fail` | `fail`: a window with no cost rows is a fault; `ignore`: normal |
| `pusher.ingestCA.existingSecret` | none | a private CA to trust for the ingest URL (never disables verification) |
| `prometheus.mode` | `bundled` | `bundled`, `external`, `none` |
| `prometheus.bundled.storage.type` | `emptyDir` | `emptyDir` or `pvc` |
| `prometheus.bundled.kubeletTLS.insecureSkipVerify` | `true` | see Security |
| `prometheus.external.url`, `.auth`, `.tenantId`, `.clusterLabel` | none | the store you run |
| `opencost.enabled`, `opencost.endpoint` | `true`, none | `false`: use an OpenCost you run |
| `opencost.pricing.provider`, `.custom` | `auto` | your own rates |
| `opencost.extraEnv`, `extraContainers`, `extraVolumes` | empty | escape hatches |
| `preflight.enabled`, `.blockPushOn`, `.helmTest` | `true`, `[cluster_ambiguity]`, `true` | the checks below |
| `networkPolicy.enabled`, `.extraIngress` | `true`, empty | ingress policies for Prometheus and OpenCost |
| `global.imageRegistry`, `global.imagePullSecrets` | empty | a private mirror |

Retry count and backoff on a failed push are fixed, not values.

For an air-gapped or mirrored install, three images are pulled (the agent, OpenCost, Prometheus),
each pinned by tag and digest; `global.imageRegistry` replaces the registry host of all three, and
the ingest endpoint must still be reachable (through your proxy and `pusher.ingestCA` if needed).

#### Currency

OpenCost does not report its own currency anywhere in its allocation output. Guessing `USD` would
silently mislabel every figure, so `currency` has no default: set it to the ISO 4217 code your
rates are in.

### What this pushes

Once a day at the scheduled time (and when it starts), the agent queries OpenCost for each of the
last `catchUpDays` closed UTC days, grouped by namespace, controller, pod and container, and POSTs
a batch per day:

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
  ],
  "collector": {
    "version": "v2.0.0",
    "prometheus_mode": "bundled",
    "window": { "start": "2026-08-10T00:00:00Z", "end": "2026-08-11T00:00:00Z" },
    "preflight": { "opencost_series": "ok", "usage_series": "ok", "history": "ok" }
  }
}
```

to `POST /api/ingest/kubernetes-cost` with `Authorization: Bearer <api key>`. Covering several days
lets a day that was missed or came back empty fill in later; Plutus replaces a day's rows on each
push, so re-sending is safe. Within one process a day already delivered is not queried again.

A row with no namespace or workload identity is kept under an `__unallocated__` sentinel rather than
dropped, and a row with no cluster identity falls back to the connection's `cluster_name`. If
OpenCost omits pod labels or GPU cost for a row, the agent treats them as empty or zero rather than
failing the push. The response is `{ accepted, rejected, errors: [...] }`; rejections are logged in
full.

#### When a day comes back empty

A cluster always runs at least its own system pods, so a day with no priced allocations means the
data behind OpenCost is missing. The agent does not report that as a successful push of nothing:

- it logs a warning (an error from the second empty day in a row);
- `plutus_collector_last_push_success` is `0`, and `plutus_collector_empty_results_total` and
  `plutus_collector_consecutive_empty_windows` count it;
- `/readyz` returns 503, so `kubectl get pods` shows `0/1` (`/healthz` stays 200: a data problem
  never restarts the pod);
- it POSTs a **heartbeat** for the newest window, `rows: []` plus the `collector` block with an
  `empty_reason`, so Plutus can tell an agent that is running but has no data from one that is not
  running. A heartbeat writes and deletes no cost rows.

```json
{
  "cluster_name": "prod-us-east-1",
  "currency": "USD",
  "rows": [],
  "collector": {
    "version": "v2.0.0",
    "prometheus_mode": "bundled",
    "window": { "start": "2026-08-10T00:00:00Z", "end": "2026-08-11T00:00:00Z" },
    "empty_reason": "no_allocations",
    "preflight": { "opencost_series": "error" }
  }
}
```

`empty_reason` is `no_allocations` (OpenCost returned none), `all_zero_cost`, `no_history` (the
store has no data yet for that window; not a fault) or `preflight_blocked:<check>`.
`pusher.emptyResult=ignore` turns the failure behaviour off; the heartbeat is still sent.

#### One-off pushes

`plutus-collector --once` does one pass and exits non-zero if any window failed or came back empty
where that is a fault. `--window=<start>,<end>` (RFC3339) pushes that window instead:

```bash
kubectl -n plutus-collector exec deploy/plutus-collector -- \
  /plutus-collector --once --window=2026-10-24T00:00:00Z,2026-10-25T00:00:00Z
```

Rows are dated by the start's UTC day and a push replaces that whole day in Plutus, so backfill
whole UTC days; a shorter window replaces the day with a partial one.

### Preflight checks and troubleshooting

The agent checks the data behind OpenCost at startup, before each push and every 5 minutes. Run
the same checks by hand:

```bash
helm test plutus-collector --namespace plutus-collector
kubectl -n plutus-collector exec deploy/plutus-collector -- /plutus-collector preflight
```

Results are logged with the fix (`check=<id> remedy=...`), exported as
`plutus_collector_preflight_ok{check="<id>"}`, sent to Plutus in the heartbeat, and any check at
`error` severity makes `/readyz` return 503. `warn` does not. One row per check:

| Check | Severity | Meaning and fix |
|---|---|---|
| `prometheus_reachable` | error | The store did not answer. 401/403: the credentials are wrong or the token lacks read scope. 404: wrong path (Grafana Cloud needs `/api/prom`, Mimir `/prometheus`). Unreachable: check the URL, DNS and network policy; with the bundled store check that its pod is Running (it is Pending if it asked for a PVC and there is no default StorageClass). |
| `opencost_series` | error | No `node_cpu_hourly_cost`: nothing scrapes OpenCost. Bundled: check the OpenCost pod. External: add the scrape job (contract above). |
| `allocation_series` | error | No `container_cpu_allocation`: your keep-list drops it, or the scrape is not reaching the store. |
| `pods_running` | error | No `kube_pod_container_status_running`: OpenCost cannot tell which pods existed. |
| `pod_labels`, `namespace_labels` | warn | Labels are missing, so cost tags and team attribution will be empty. |
| `controller_labels` | warn | No `deployment_match_labels`: cost cannot be attributed to a Deployment or StatefulSet, only to pods. |
| `usage_series` | warn | No cAdvisor CPU usage: costs are request-based and a pod without requests costs nothing. Bundled: the kubelet may be unreachable from pods (GKE Autopilot, EKS Fargate, a strict NetworkPolicy), or TLS verification is on and the certificate does not verify. |
| `cluster_ambiguity` | error, blocks the push | The store holds several clusters and no label is configured. Set `prometheus.external.clusterLabel`. |
| `history` | warn | The store holds less history than the days to push. Expected right after an install; if OpenCost has run for longer, retention or an `emptyDir` reschedule dropped it. |
| `opencost_reachable` | error | OpenCost does not answer. Its logs say what it is waiting for, most often its Prometheus. |
| `allocation_nonempty` | error | OpenCost is up and its series are present but it computed no allocations for the last hour. If it started minutes ago, wait a few scrape intervals. |
| `pricing` | warn | OpenCost found no cloud provider and no custom rates are set; see Pricing. |

`preflight.blockPushOn` lists the checks that stop a push (default `cluster_ambiguity`); a blocked
push still sends a heartbeat naming the check.

Other symptoms:

| Symptom | Cause |
|---|---|
| OpenCost restarts once or twice right after install | Expected. It exits when its Prometheus has no running scrape jobs yet (`no running jobs on Prometheus` in its log) and starts cleanly once the first scrape is configured, within a minute or two. If it keeps restarting, check that the Prometheus pod is Running. |
| `helm upgrade --wait` times out and the old pusher pod stays | Deliberate: the new pod is not Ready because a data check fails, so the rollout does not replace a working agent. `kubectl logs` on the new pod, or `helm test`, names the check. |
| Pusher `0/1` the morning after install | Read `kubectl logs`: the failing check and its fix are in the line. |
| Rows have `cluster: default-cluster` | You are on an OpenCost you run (`opencost.enabled=false`) that has its own cluster id; set it to `clusterName`. With the chart's OpenCost this cannot happen. |
| Costs look like GCP prices on a non-cloud cluster | `pricing` warning: set `opencost.pricing.custom`. |
| `PLUTUS_INGEST_URL must use https` | It carries a Bearer key and is never allowed in cleartext. |
| Install fails with `Additional property ... is not allowed` | A key the schema does not know: see MIGRATING.md if it came from a v1 values file. |

### Upgrading from v1

Chart v2 renders OpenCost itself and no longer takes values for the OpenCost subchart. A v1 values
file keeps working for one release through a translation layer; see [MIGRATING.md](MIGRATING.md).

### Uninstalling

```bash
helm uninstall plutus-collector --namespace plutus-collector
```

Revoking the API key in Plutus (Cost Sources → Kubernetes → Revoke) stops the agent from being able
to push immediately, independent of whether you've uninstalled the chart.

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
Kubernetes cost agent only. Run it as an ordinary one-replica `Deployment` with the same environment
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

- **The pusher has no Kubernetes API access.** It has no `ServiceAccount` token mounted and no
  RBAC of its own. It calls OpenCost's HTTP `/allocation` endpoint and, for the preflight, the
  metrics store's query API, both inside your cluster, and Plutus's ingest endpoint (outbound
  HTTPS). It never reads pods, nodes or any other cluster object.
- **Locked-down pod security context** for the pusher: non-root, read-only root filesystem,
  `allowPrivilegeEscalation: false`, every Linux capability dropped, `RuntimeDefault` seccomp.
  Fixed at one replica: a second would push the same batch twice.
- **RBAC the chart does create**, all cluster-wide and read-only except where noted:

  | Mode | OpenCost | Bundled Prometheus |
  |---|---|---|
  | `bundled` (default) | get/list/watch on pods, nodes, namespaces, services, workloads, volumes, storage classes and a few more (the list OpenCost needs; see `chart/templates/opencost.yaml`) | `nodes` and `nodes/metrics` (get/list/watch) |
  | `external` | the same | none; no Prometheus is rendered |
  | `none` (experimental) | the same, plus `nodes/proxy` | none |

  `nodes/proxy` is rendered only for `prometheus.mode=none`, and only after you set
  `prometheus.none.acknowledgeNodesProxy=true`: that permission can be used to run commands in
  any pod on the node (the kubelet's exec endpoint is reachable through it), which is also why it
  is not in the default. The chart installs no CRDs and no admission webhooks, and no UI or MCP
  server for OpenCost (the MCP server is an unauthenticated endpoint serving cost data).
- **The bundled Prometheus and OpenCost serve unauthenticated, cluster-wide telemetry**, so the
  chart renders ingress NetworkPolicies (`networkPolicy.enabled`, default on): Prometheus accepts
  only OpenCost and the pusher, OpenCost only the bundled Prometheus and the pusher. They restrict
  ingress only. Kubernetes enforces NetworkPolicy only when the cluster's network plugin does
  (Calico and Cilium do; flannel on its own does not), so on a cluster without enforcement this
  has no effect. With `prometheus.mode=external` no OpenCost policy is rendered, because the
  scraper is yours; add a policy for it, or rules through `networkPolicy.extraIngress`.
- **Kubelet TLS.** The bundled Prometheus scrapes each node's kubelet over HTTPS with its service
  account token, and by default does not verify the kubelet's certificate
  (`prometheus.bundled.kubeletTLS.insecureSkipVerify: true`). That default is a tradeoff: kubelet
  serving certificates are self-signed on kubeadm and k3s clusters, and verifying them there would
  make the scrape fail and silently drop usage metrics, so costs would fall back to requests only.
  What it means: someone able to intercept traffic between pods and nodes could present a fake
  kubelet and capture the token, which grants `nodes` and `nodes/metrics` read access. Where the
  kubelet certificates are signed by your cluster CA (check yours; this chart cannot tell), set it to
  `false` and the cluster CA that every pod already has is used. If they are signed by another CA, also set
  `prometheus.bundled.kubeletTLS.caSecret` to a Secret holding its PEM (key `caKey`, default
  `ca.crt`). The chart does not use the API server's node proxy instead, because that needs
  `nodes/proxy`. The install notes repeat this while the default is in effect.
- **Credentials** for an external store are read from a Secret you create (`existingSecret`) and
  mounted or referenced by the pods; they are never copied into the chart's own objects, and URLs
  containing `user:password@` are refused.
- **Images** are pinned by tag and digest (the digests come from the published multi-arch
  manifests); the agent image and chart are signed, see [SECURITY.md](SECURITY.md).

### How do I verify it's working?

All collectors expose the same endpoints and emit the same metric names, on `METRICS_ADDR`
(`:9100` by default).

1. **`/healthz`** — liveness, always `200 ok` once the process is up (a down OpenCost/Plutus
   endpoint is a push failure to retry, not a reason to restart the container).
   **`/readyz`** — readiness: `200`, or `503` with the reasons while the Kubernetes agent's last
   window came back empty or a preflight check is failing (gateway collectors are always ready).
2. **`/metrics`** (Prometheus text format) — the fields that matter:
   - `plutus_collector_last_push_success` — `1` after a successful push, `0` after one that
     failed all its retries.
   - `plutus_collector_last_push_timestamp_seconds` — when that last attempt finished.
   - `plutus_collector_last_push_row_count` — how many rows were in the last successful batch.
   - `plutus_collector_last_push_duration_seconds` — how long the attempt (including retries)
     took.
   - `plutus_collector_empty_results_total`, `plutus_collector_consecutive_empty_windows`,
     `plutus_collector_last_success_window_timestamp_seconds` — empty-window accounting.
   - `plutus_collector_preflight_ok{check="<id>"}` and `plutus_collector_window_coverage_ratio` —
     the Kubernetes agent's preflight outcome and how much of the newest window the store covers.

   ```bash
   kubectl -n plutus-collector port-forward svc/plutus-collector 9100:9100
   curl localhost:9100/metrics
   ```
   For the LiteLLM collector outside Kubernetes, that is just
   `curl localhost:9100/metrics` against the container's published port.
3. **The Plutus UI** — the cost source's connection panel shows the last push once the first
   one succeeds.
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
test/chart/run.sh            # helm template goldens, v1 shim, install-time failures, kubeconform
UPDATE=1 test/chart/run.sh   # accept an intended chart change, then review the golden diff
test/e2e/run.sh              # kind end-to-end test, about ten minutes (docker, kind, kubectl, helm, jq)

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

- `internal/pusher` — the schedule/ticker loop, catch-up windows, in-process retry/backoff, and the
  `Source` interface each collector implements. Its real subject is the failure policy (which errors are retryable, that
  a 4xx must not consume the backoff budget, that an empty result is never pushed as an empty
  batch), which is identical for every source and is exactly what drifts when a second agent
  copies it instead of sharing it.
- `internal/ingest` — the HTTP transport: auth header, status-code rules, retryable-vs-not, and
  the `Response` shape. Deliberately knows nothing about what it is sending.
- `internal/config` — env-var loading and validation (12-factor, no config file). `Common` plus
  one loader per binary, since the required variables genuinely differ.
- `internal/metrics` — the hand-rolled `/metrics` (Prometheus text format), `/healthz` and
  `/readyz` handlers.
- `internal/version` — the release tag stamped into the binary at build time.

Kubernetes:

- `cmd/plutus-collector` — entrypoint.
- `internal/opencost` — the OpenCost `/allocation` HTTP client and the preflight probes.
- `internal/preflight` — the data-pipeline checks and their stable ids; `internal/promclient` is the
  small Prometheus query client they use.
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
