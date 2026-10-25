#!/usr/bin/env bash
# Chart tests: golden renders for each values file, the v1 translation shim, the install-time
# failures, the properties every default render must have, and kubeconform on every render.
#
#   test/chart/run.sh            compare renders with test/chart/golden/
#   UPDATE=1 test/chart/run.sh   rewrite the goldens after an intended change, then review the diff
#
# Needs helm. kubeconform is used when it is on PATH (CI installs it) and skipped with a notice
# otherwise. Nothing here talks to a cluster.
set -euo pipefail

cd "$(dirname "$0")/../.."
chart=chart
values=test/chart/values
golden=test/chart/golden
k8s_version="${KUBERNETES_VERSION:-1.30.0}"
failures=0

fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

render() { # values-file [extra helm args]
  local f="$1"; shift
  helm template plutus-collector "$chart" --namespace plutus-collector -f "$f" "$@"
}

extra_args() { # per-case helm flags
  case "$1" in
    external-multicluster) echo "--api-versions monitoring.coreos.com/v1/ServiceMonitor" ;;
  esac
}

have_kubeconform=0
if command -v kubeconform >/dev/null 2>&1; then have_kubeconform=1; else echo "note: kubeconform not found, skipping schema validation"; fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

for f in "$values"/*.yaml; do
  name="$(basename "$f" .yaml)"
  case "$name" in v1-unknown-passthrough|*.v2-equivalent) continue ;; esac

  # shellcheck disable=SC2046
  if ! render "$f" $(extra_args "$name") >"$tmp/$name.yaml" 2>"$tmp/$name.err"; then
    fail "$name: helm template failed: $(cat "$tmp/$name.err")"
    continue
  fi
  helm lint "$chart" -f "$f" >"$tmp/$name.lint" 2>&1 || fail "$name: helm lint failed: $(cat "$tmp/$name.lint")"

  if [ "${UPDATE:-}" = "1" ]; then
    cp "$tmp/$name.yaml" "$golden/$name.yaml"
  elif ! diff -u "$golden/$name.yaml" "$tmp/$name.yaml" >"$tmp/$name.diff"; then
    fail "$name: render differs from golden (UPDATE=1 to accept an intended change)"
    head -40 "$tmp/$name.diff" >&2
  fi

  if [ "$have_kubeconform" = 1 ]; then
    kubeconform -strict -ignore-missing-schemas -kubernetes-version "$k8s_version" -summary "$tmp/$name.yaml" >"$tmp/$name.kc" 2>&1 \
      || fail "$name: kubeconform: $(cat "$tmp/$name.kc")"
  fi
done

# The v1 shim: a v1 values file must render exactly what its v2 translation renders.
if ! diff -u <(render "$values/v1-plutus-values.v2-equivalent.yaml") <(render "$values/v1-plutus-values.yaml") >"$tmp/shim.diff"; then
  fail "v1-plutus-values: the shim output differs from the v2-native equivalent"
  head -40 "$tmp/shim.diff" >&2
fi

# A v1 key the shim cannot carry over fails the install and points at the migration guide.
if render "$values/v1-unknown-passthrough.yaml" >/dev/null 2>"$tmp/unknown.err"; then
  fail "v1-unknown-passthrough: expected the install to fail"
elif ! grep -q "MIGRATING.md" "$tmp/unknown.err"; then
  fail "v1-unknown-passthrough: the error should point at MIGRATING.md: $(cat "$tmp/unknown.err")"
fi

# Install-time validation: each of these must be refused, with a message that says what to change.
expect_fail() { # description expected-text helm-args...
  local what="$1" text="$2"; shift 2
  if helm template plutus-collector "$chart" --set clusterName=c --set currency=USD --set apiKey=k "$@" >/dev/null 2>"$tmp/neg.err"; then
    fail "$what: expected the install to fail"
  elif ! grep -qi "$text" "$tmp/neg.err"; then
    fail "$what: error does not mention '$text': $(cat "$tmp/neg.err")"
  fi
}
expect_fail "mode none without acknowledgement" "acknowledgeNodesProxy" --set prometheus.mode=none
expect_fail "mode external without url" "url" --set prometheus.mode=external
expect_fail "unknown mode" "mode" --set prometheus.mode=thanos
expect_fail "unknown top-level key" "additional" --set bogus=1
expect_fail "enabling the OpenCost UI" "enabled" --set opencost.ui.enabled=true
expect_fail "auth without a secret" "existingSecret" --set prometheus.mode=external --set prometheus.external.url=http://p --set prometheus.external.auth.type=basic
expect_fail "custom pricing without rates" "opencost.pricing.custom" --set opencost.pricing.provider=custom
expect_fail "retention shorter than catch-up" "retention" --set prometheus.bundled.retention=2d
expect_fail "credentials in the url" "pattern" --set prometheus.mode=external --set prometheus.external.url=https://user:pw@prom.example
expect_fail "missing clusterName" "clusterName" --set clusterName=
expect_fail "lowercase currency" "currency" --set currency=usd

# Properties the default install must have.
d="$tmp/default.yaml"
grep -q "nodes/proxy" "$d" && fail "default render grants nodes/proxy"
grep -q "CustomResourceDefinition" "$d" && fail "default render contains a CRD"
grep -qE "opencost-ui|name: http-ui|containerPort: 8081" "$d" && fail "default render contains the OpenCost UI or MCP server"
grep -q "MCP_SERVER_ENABLED" "$d" || fail "default render does not set MCP_SERVER_ENABLED"
grep -A1 "MCP_SERVER_ENABLED" "$d" | grep -q '"false"' || fail "MCP server is not disabled by default"
grep -qE "^kind: (ServiceMonitor|PodMonitor|PrometheusRule)" "$d" && fail "default render contains an operator resource"
grep -q "@sha256:" "$d" || fail "default render does not pin image digests"
grep -q "PROMETHEUS_SERVER_ENDPOINT" "$d" || fail "default render does not point OpenCost at a store"
grep -A1 "name: CLUSTER_ID" "$d" | grep -q "prod-us-east-1" || fail "OpenCost's cluster id is not clusterName"

# The experimental mode is the only one that may grant nodes/proxy, and it must also drop Prometheus.
grep -q "nodes/proxy" "$tmp/mode-none.yaml" || fail "mode none does not render nodes/proxy"
grep -q "kind: Deployment" "$tmp/mode-none.yaml" && grep -q "name: plutus-collector-prometheus" "$tmp/mode-none.yaml" && fail "mode none renders a Prometheus"

# The external modes render no Prometheus and no nodes/proxy.
for n in external-basic external-grafana-cloud external-mtls external-multicluster opencost-disabled; do
  grep -q "plutus-collector-prometheus" "$tmp/$n.yaml" && fail "$n renders the bundled Prometheus"
  grep -q "nodes/proxy" "$tmp/$n.yaml" && fail "$n grants nodes/proxy"
done
grep -q "CURRENT_CLUSTER_ID_FILTER_ENABLED" "$tmp/external-multicluster.yaml" || fail "external-multicluster does not enable OpenCost's cluster filter"
grep -q "kind: ServiceMonitor" "$tmp/external-multicluster.yaml" || fail "external-multicluster did not render its ServiceMonitor"
grep -q "kind: Deployment" "$tmp/opencost-disabled.yaml" && grep -q "name: plutus-collector-opencost" "$tmp/opencost-disabled.yaml" && fail "opencost-disabled renders OpenCost"

if [ "$failures" -gt 0 ]; then
  echo "$failures chart test failure(s)" >&2
  exit 1
fi
echo "chart tests passed"
