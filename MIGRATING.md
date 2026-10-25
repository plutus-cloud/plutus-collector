# Migrating from chart v1 to v2

Chart v2 renders OpenCost itself and, by default, a small Prometheus for it to read from. It no
longer depends on the upstream OpenCost chart, so values for that chart (everything under
`opencost.opencost.*` in a v1 values file) are no longer passed through.

A v1 values file keeps working for one release. A translation layer reads the keys listed below and
produces the v2 configuration, and `helm install` / `helm upgrade` prints a deprecation notice for
each v1 key it used. The layer is removed in the next minor release, so move to the v2 keys when you
next touch your values. A v1 key it cannot honour **fails the install** with the key's name, rather
than being ignored.

## What changes for you

- **Without any Prometheus configuration, v2 installs its own.** A v1 install that set nothing about
  Prometheus relied on OpenCost's default endpoint,
  `prometheus-server.prometheus-system.svc.cluster.local:80`. Under v2 the same values install the
  bundled Prometheus instead. To keep using the one you have, set `prometheus.mode=external` and its
  URL.
- **The agent pushes on a schedule, not on an interval.** Daily at 01:30 UTC plus a random delay,
  covering the last three closed UTC days, instead of at start and every `pushIntervalMinutes`.
  `pushIntervalMinutes` still works as a deprecated alias.
- **An empty day is a fault.** The pod reports `0/1` Ready and `plutus_collector_last_push_success`
  is `0` when a window has no cost rows (`pusher.emptyResult: fail`); set `ignore` to restore the
  old behaviour.
- **OpenCost's cluster id is `clusterName`.** `opencost.opencost.exporter.defaultClusterId` is no
  longer needed. If it differs from `clusterName` it is ignored and the install notes say so.
- **The OpenCost UI and MCP server are gone** (they were on by default in v1).
- **Selectors and names.** The pusher Deployment keeps its name and selector labels, so an upgrade
  is an in-place rollout. OpenCost is now rendered by this chart under the same name
  (`<release>-opencost`), with the component label added; because a Deployment's selector is
  immutable, Helm replaces that Deployment on upgrade. Expect OpenCost to restart once.

## Key by key

| v1 | v2 |
|---|---|
| `opencost.opencost.prometheus.external.enabled: true` and `.url` | `prometheus.mode: external`, `prometheus.external.url` |
| `opencost.opencost.prometheus.internal.enabled: false` | must be paired with `external.enabled: true`; otherwise the install fails |
| `opencost.opencost.prometheus.internal.{serviceName,namespaceName,port,path,scheme}` set | `prometheus.mode: external` with the URL built from them |
| `opencost.opencost.prometheus.existingSecretName` with `username_key` / `password_key` | `prometheus.external.auth: {type: basic, existingSecret, usernameKey, passwordKey}` |
| `opencost.opencost.prometheus.bearer_token_key` with `existingSecretName` | `prometheus.external.auth: {type: bearer, existingSecret, tokenKey}` |
| `opencost.opencost.prometheus.insecureSkipVerify` | `prometheus.external.insecureSkipVerify` |
| `opencost.opencost.customPricing.enabled: true`, `provider: custom` and `costModel.*` | `opencost.pricing: {provider: custom, custom: {CPU, RAM, GPU, storage, spotCPU, spotRAM, zoneNetworkEgress, regionNetworkEgress, internetNetworkEgress}}`; `costModel.description` is dropped |
| `opencost.opencost.exporter.defaultClusterId` | dropped; `clusterName` is used |
| `pushIntervalMinutes` | `pusher.pushIntervalMinutes` (deprecated), or remove it to use `pusher.schedule` |
| `imagePullSecrets` | `global.imagePullSecrets` |
| `image.{repository,tag,pullPolicy}` | `pusher.image.*` |
| `resources` | `pusher.resources` |
| `opencost.enabled`, `opencost.endpoint` | unchanged |
| `existingSecret`, `existingSecretKey`, `plutusIngestUrl`, `clusterName`, `currency` | unchanged |
| `replicaCount` | removed; the agent is always one replica. The schema rejects it, delete the line |
| any other `opencost.opencost.*` key | fails: `opencost.extraEnv`, `extraContainers` and `extraVolumes` cover what the chart does not expose |

Not carried over and failing the install if set: Amazon Managed Prometheus (`prometheus.amp`),
Thanos (`prometheus.thanos`), kube-rbac-proxy, inline `username`/`password`/`bearer_token` (v2 reads
credentials from a Secret only), cloud-cost integration, carbon cost, custom metrics, plugins,
persistence for the CSV exporter. For AMP, run a SigV4 proxy as `opencost.extraContainers`; for
Thanos, point `prometheus.external.url` at the query endpoint.

## Example

v1:

```yaml
clusterName: plutus-hetzner-app-box
currency: USD
existingSecret: plutus-collector-api-key
pushIntervalMinutes: 1440
opencost:
  enabled: true
  opencost:
    exporter:
      defaultClusterId: plutus-hetzner-app-box
    prometheus:
      existingSecretName: grafana-cloud-read
      username_key: username
      password_key: password
      internal: { enabled: false }
      external:
        enabled: true
        url: https://prometheus-prod-01-eu-west-0.grafana.net/api/prom
    customPricing:
      enabled: true
      provider: custom
      costModel: { CPU: "0.003154", RAM: "0.000423", storage: "0", GPU: "0", spotCPU: "0.003154", spotRAM: "0.000423" }
```

v2:

```yaml
clusterName: plutus-hetzner-app-box
currency: USD
existingSecret: plutus-collector-api-key
prometheus:
  mode: external
  external:
    url: https://prometheus-prod-01-eu-west-0.grafana.net/api/prom
    auth: {type: basic, existingSecret: grafana-cloud-read, usernameKey: username, passwordKey: password}
opencost:
  pricing:
    provider: custom
    custom: {CPU: "0.003154", RAM: "0.000423", storage: "0", GPU: "0", spotCPU: "0.003154", spotRAM: "0.000423"}
```

The repository's chart tests render a v1 file of this shape and its v2 equivalent and require the two
to be identical (`test/chart/values/v1-plutus-values*.yaml`).

## Checking an upgrade

```bash
helm template plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector --version <v2> \
  -f your-values.yaml > /dev/null            # fails with the reason if a key is not honoured
helm upgrade plutus-collector oci://ghcr.io/plutus-cloud/charts/plutus-collector --version <v2> \
  -n plutus-collector -f your-values.yaml    # prints each deprecation
helm test plutus-collector -n plutus-collector
```
