#!/usr/bin/env bash
# End-to-end test on a throwaway kind cluster: install the chart with nothing else in the
# cluster, let OpenCost and the bundled Prometheus collect for a few minutes, push a recent window
# to a mock ingest endpoint, and assert on what arrived. Then break the data pipeline on purpose
# and assert that the agent says so instead of reporting a healthy push of nothing.
#
# Needs docker, kind, kubectl, helm, jq, openssl and curl. Takes about ten minutes.
#
#   KEEP=1                keep the cluster afterwards
#   FOOTPRINT_PODS=100    pause pods to schedule for the footprint measurement (0 skips it)
#   TARGETARCH            architecture to build the agent for (default: the Docker host's, which is the kind node's)
#
# Nothing here talks to Plutus: the ingest URL is a mock inside the cluster.
set -euo pipefail

cd "$(dirname "$0")/../.."

cluster="${CLUSTER:-plutus-e2e}"
release=plutus-collector
ns=plutus-collector
cluster_name=e2e-cluster
footprint_pods="${FOOTPRINT_PODS:-100}"
workdir="$(mktemp -d)"
pf_pids=()

log() { printf '\n== %s\n' "$*"; }
die() { echo "E2E FAILURE: $*" >&2; exit 1; }

cleanup() {
  local rc=$?
  for pid in "${pf_pids[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
  if [ "$rc" -ne 0 ]; then
    echo "---- diagnostics ----" >&2
    kubectl get pods -A -o wide >&2 || true
    for d in "$release" "$release-opencost" "$release-prometheus"; do
      echo "---- logs: $d ----" >&2
      kubectl -n "$ns" logs "deploy/$d" --tail=60 >&2 || true
    done
    kubectl -n e2e logs deploy/mock-ingest --tail=20 >&2 || true
  fi
  if [ "${KEEP:-}" != "1" ]; then kind delete cluster --name "$cluster" >/dev/null 2>&1 || true; fi
  rm -rf "$workdir"
  exit "$rc"
}
trap cleanup EXIT

portforward() { # namespace target local:remote
  kubectl -n "$1" port-forward "$2" "$3" >/dev/null 2>&1 &
  pf_pids+=("$!")
  sleep 3
}

received() { curl -sk https://localhost:18443/received; }

# The newest pusher pod. During a rollout whose new pod is not Ready, the old one is kept, so
# deploy/<name> can name either.
pusher_pod() {
  kubectl -n "$ns" get pods -l "app.kubernetes.io/name=plutus-collector,app.kubernetes.io/instance=$release" \
    --sort-by=.metadata.creationTimestamp -o name | tail -1
}

# ---------------------------------------------------------------------------------------------
log "building images"
# The Dockerfile defaults TARGETARCH to amd64 for plain `docker build`, so on an arm64 host the
# agent would be an amd64 binary and the pod would fail with an exec format error. Build for the
# host's architecture, which is also the kind node's.
if [ -z "${TARGETARCH:-}" ]; then
  case "$(uname -m)" in
    x86_64|amd64) TARGETARCH=amd64 ;;
    aarch64|arm64) TARGETARCH=arm64 ;;
    *) echo "unsupported architecture $(uname -m); set TARGETARCH" >&2; exit 1 ;;
  esac
fi
docker build --build-arg VERSION=e2e --build-arg TARGETOS=linux --build-arg "TARGETARCH=$TARGETARCH" -t plutus-collector:e2e .
docker build -f test/e2e/mockingest/Dockerfile -t plutus-mock-ingest:e2e .

log "creating the kind cluster"
kind get clusters | grep -qx "$cluster" || kind create cluster --name "$cluster" --wait 180s
kind load docker-image --name "$cluster" plutus-collector:e2e plutus-mock-ingest:e2e

# ---------------------------------------------------------------------------------------------
log "mock ingest endpoint (TLS with a private CA, which the agent is told to trust)"
kubectl create namespace e2e --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -keyout "$workdir/tls.key" -out "$workdir/tls.crt" \
  -subj "/CN=mock-ingest" -addext "subjectAltName=DNS:mock-ingest.e2e.svc.cluster.local,DNS:localhost" >/dev/null 2>&1
kubectl -n e2e create secret generic mock-ingest-tls --from-file="$workdir/tls.crt" --from-file="$workdir/tls.key" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$ns" create secret generic mock-ingest-ca --from-file=ca.crt="$workdir/tls.crt" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: mock-ingest, namespace: e2e }
spec:
  replicas: 1
  selector: { matchLabels: { app: mock-ingest } }
  template:
    metadata: { labels: { app: mock-ingest } }
    spec:
      containers:
        - name: mock
          image: plutus-mock-ingest:e2e
          imagePullPolicy: Never
          env:
            - { name: TLS_CERT, value: /tls/tls.crt }
            - { name: TLS_KEY, value: /tls/tls.key }
          ports: [{ containerPort: 8443 }]
          volumeMounts: [{ name: tls, mountPath: /tls, readOnly: true }]
      volumes: [{ name: tls, secret: { secretName: mock-ingest-tls } }]
---
apiVersion: v1
kind: Service
metadata: { name: mock-ingest, namespace: e2e }
spec:
  selector: { app: mock-ingest }
  ports: [{ port: 8443, targetPort: 8443 }]
EOF
kubectl -n e2e rollout status deploy/mock-ingest --timeout=120s

log "sample workloads: a labelled Deployment with requests, and a CPU burner with none"
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata: { name: sample, labels: { team: e2e } }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: web, namespace: sample, labels: { app: web } }
spec:
  replicas: 2
  selector: { matchLabels: { app: web } }
  template:
    metadata: { labels: { app: web, owner: platform } }
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "while true; do :; done"]
          resources:
            requests: { cpu: 50m, memory: 32Mi }
            limits: { cpu: 100m, memory: 64Mi }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: burn, namespace: sample, labels: { app: burn } }
spec:
  replicas: 1
  selector: { matchLabels: { app: burn } }
  template:
    metadata: { labels: { app: burn } }
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "while true; do :; done"]
EOF

# ---------------------------------------------------------------------------------------------
log "installing the chart with prometheus.mode=bundled and nothing else"
helm upgrade --install "$release" chart --namespace "$ns" \
  --set clusterName="$cluster_name" --set currency=USD --set apiKey=e2e-key \
  --set pusher.image.repository=plutus-collector --set pusher.image.tag=e2e --set pusher.image.pullPolicy=Never \
  --set pusher.ingestCA.existingSecret=mock-ingest-ca \
  --set plutusIngestUrl=https://mock-ingest.e2e.svc.cluster.local:8443/api/ingest/kubernetes-cost \
  --set prometheus.bundled.scrapeInterval=15s

kubectl -n "$ns" rollout status "deploy/$release-prometheus" --timeout=180s
kubectl -n "$ns" rollout status "deploy/$release-opencost" --timeout=420s

log "waiting for the preflight to pass (helm test)"
passed=0
for i in $(seq 1 40); do
  if helm test "$release" --namespace "$ns" >"$workdir/helmtest.out" 2>&1; then passed=1; break; fi
  sleep 15
done
cat "$workdir/helmtest.out" || true
[ "$passed" = 1 ] || die "helm test (preflight) did not pass within 10 minutes"

log "preflight output while the store is young (the history check is expected to warn)"
kubectl -n "$ns" exec "deploy/$release" -- /plutus-collector preflight || true

# ---------------------------------------------------------------------------------------------
log "waiting for OpenCost to have a few minutes of data, then pushing the last 15 minutes"
sleep 240
start="$(date -u -d '15 minutes ago' +%Y-%m-%dT%H:%M:%SZ)"
end="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
kubectl -n "$ns" exec "deploy/$release" -- /plutus-collector --once "--window=$start,$end"

portforward e2e svc/mock-ingest 18443:8443
received >"$workdir/received.json"
jq -e '[.[] | select((.rows | length) > 0)] | length > 0' "$workdir/received.json" >/dev/null \
  || die "the mock received no batch with rows"

log "asserting on the rows"
jq -e --arg c "$cluster_name" '[.[] | .rows[]?] as $r
  | ($r | length) > 0
  and ([$r[] | select(.cluster == $c)] | length) == ($r | length)' "$workdir/received.json" >/dev/null \
  || die "rows are not all labelled cluster=$cluster_name"
jq -e '[.[] | .rows[]? | select(.namespace == "sample" and (.controller_name == "web") and ((.controller_kind | ascii_downcase) == "deployment"))] | length > 0' \
  "$workdir/received.json" >/dev/null || die "no row attributes cost to the sample Deployment web"
jq -e '[.[] | .rows[]? | select(.namespace == "sample" and .controller_name == "web" and (.labels.app == "web"))] | length > 0' \
  "$workdir/received.json" >/dev/null || die "the web rows carry no pod labels"
# A pod with no requests is only allocated CPU if cAdvisor usage reaches the store: OpenCost's
# allocation is the larger of the request and the observed usage. Row costs are rounded to cents,
# which is below the resolution of a fifteen-minute window for a pod this small, so the proof is
# the allocation series itself, read from the bundled Prometheus.
jq -e '[.[] | .rows[]? | select(.namespace == "sample" and .controller_name == "burn")] | length > 0' \
  "$workdir/received.json" >/dev/null || die "the burner without requests has no row"
portforward "$ns" "svc/$release-prometheus" 19090:9090
burn_alloc="$(curl -s --data-urlencode 'query=max(container_cpu_allocation{namespace="sample",pod=~"burn-.*"})' http://localhost:19090/api/v1/query | jq -r '.data.result[0].value[1] // "0"')"
awk -v v="$burn_alloc" 'BEGIN { exit !(v > 0) }' || die "the burner without requests is allocated no CPU ($burn_alloc), so cAdvisor usage is not flowing"
jq -e '[.[] | .collector? | select(. != null) | select(.prometheus_mode == "bundled" and .version == "e2e")] | length > 0' \
  "$workdir/received.json" >/dev/null || die "pushes carry no collector block"
echo "rows arrived and carry the expected attribution"

# ---------------------------------------------------------------------------------------------
if [ "$footprint_pods" -gt 0 ]; then
  log "footprint of the bundled Prometheus and OpenCost with $footprint_pods extra pause containers"
  kubectl create namespace pause --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n pause create deployment pause --image=registry.k8s.io/pause:3.9 --replicas="$footprint_pods" --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n pause rollout status deploy/pause --timeout=300s || true
  sleep 180
  for pod in prometheus opencost; do
    q="container_memory_working_set_bytes{namespace=\"$ns\",pod=~\".*-$pod-.*\",container=\"$pod\"}"
    mib="$(curl -s --data-urlencode "query=$q" http://localhost:19090/api/v1/query | jq -r '.data.result[0].value[1] // empty' | awk '{printf "%.0f", $1/1048576}')"
    echo "footprint: $pod working set ${mib:-unknown} MiB with $footprint_pods pause pods (kind, one node; not a sizing guide)"
  done
  series="$(curl -s --data-urlencode 'query=count({__name__=~".+"})' http://localhost:19090/api/v1/query | jq -r '.data.result[0].value[1] // "unknown"')"
  echo "footprint: series in the bundled Prometheus: $series"
  # A kind node schedules at most 110 pods; free the room the negative test below needs.
  kubectl delete namespace pause --wait=true >/dev/null
fi

# ---------------------------------------------------------------------------------------------
log "negative: point OpenCost at a Prometheus that does not scrape it"
kubectl apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: empty-prom, namespace: e2e }
spec:
  replicas: 1
  selector: { matchLabels: { app: empty-prom } }
  template:
    metadata: { labels: { app: empty-prom } }
    spec:
      containers:
        - name: prom
          image: quay.io/prometheus/prometheus:v3.15.1
          ports: [{ containerPort: 9090 }]
---
apiVersion: v1
kind: Service
metadata: { name: empty-prom, namespace: e2e }
spec:
  selector: { app: empty-prom }
  ports: [{ port: 9090, targetPort: 9090 }]
EOF
kubectl -n e2e rollout status deploy/empty-prom --timeout=180s

helm upgrade "$release" chart --namespace "$ns" --reuse-values \
  --set prometheus.mode=external --set prometheus.external.url=http://empty-prom.e2e.svc.cluster.local:9090
# The new pusher pod is not Ready (its preflight fails), so the rollout of the pusher never
# completes and the old pod stays: a broken configuration does not replace a working agent. Wait
# for the new pod to be running instead.
kubectl -n "$ns" rollout status "deploy/$release-opencost" --timeout=420s
for i in $(seq 1 60); do
  pod="$(pusher_pod)"
  [ "$(kubectl -n "$ns" get "$pod" -o jsonpath='{.status.containerStatuses[0].started}' 2>/dev/null)" = "true" ] \
    && kubectl -n "$ns" get "$pod" -o jsonpath='{.spec.containers[0].env[?(@.name=="PROMETHEUS_URL")].value}' | grep -q empty-prom && break
  sleep 5
done
pod="$(pusher_pod)"
echo "new pusher pod: $pod"

if helm test "$release" --namespace "$ns" >"$workdir/negative.out" 2>&1; then
  cat "$workdir/negative.out"; die "helm test passed against a Prometheus that does not scrape OpenCost"
fi
cat "$workdir/negative.out"
kubectl -n "$ns" logs "pod/$release-preflight" 2>/dev/null | tee "$workdir/negative-preflight.out" || true
kubectl -n "$ns" exec "$pod" -- /plutus-collector preflight >"$workdir/negative-preflight.out" 2>&1 || true
grep -q "opencost_series" "$workdir/negative-preflight.out" || die "the preflight did not name opencost_series"
grep -q "nothing is scraping OpenCost" "$workdir/negative-preflight.out" || die "the preflight message does not say what to fix"

portforward "$ns" "$pod" 19100:9100
sleep 5
code="$(curl -s -o /dev/null -w '%{http_code}' http://localhost:19100/readyz)"
[ "$code" = "503" ] || die "/readyz returned $code, expected 503"
curl -s http://localhost:19100/metrics | grep -q 'plutus_collector_preflight_ok{check="opencost_series"} 0' \
  || die "the preflight metric for opencost_series is not 0"

if kubectl -n "$ns" exec "$pod" -- /plutus-collector --once "--window=$start,$end"; then
  die "a one-shot push of an empty window should exit non-zero"
fi
received >"$workdir/received2.json"
jq -e '[.[] | select((.rows | length) == 0 and (.collector.empty_reason // "") != "" and (.collector.preflight.opencost_series == "error"))] | length > 0' \
  "$workdir/received2.json" >/dev/null || die "the mock received no heartbeat with an empty reason and the failing preflight check"

log "end-to-end test passed"
