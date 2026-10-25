{{/*
prometheus.yml for the bundled Prometheus. A named template so the ConfigMap and the pod's
checksum annotation are computed from the same text, and the pod restarts when the config changes.
Called with a dict: root, cfg.
*/}}
{{- define "plutus-collector.prometheusConfig" -}}
{{- $b := .cfg.prometheus.bundled -}}
{{- /* The kubelet's serving certificate is verified against this CA unless verification is turned off. */ -}}
{{- $caFile := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt" -}}
{{- if $b.kubeletTLS.caSecret -}}{{- $caFile = printf "/etc/kubelet-ca/%s" $b.kubeletTLS.caKey -}}{{- end -}}
{{- $ocName := printf "%s-opencost" (include "plutus-collector.fullname" .root) -}}
global:
  scrape_interval: {{ $b.scrapeInterval }}
scrape_configs:
  - job_name: opencost
    # OpenCost's series carry the labels the allocation queries join on; keep them as exposed.
    honor_labels: true
    static_configs:
      - targets: ["{{ $ocName }}.{{ .root.Release.Namespace }}.svc.cluster.local:9003"]
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: "go_.*|process_.*|promhttp_.*|kubecost_http_.*"
        action: drop
{{- if $b.scrapeCadvisor }}
  - job_name: kubelet-cadvisor
    scheme: https
    metrics_path: /metrics/cadvisor
    authorization:
      credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    tls_config:
      ca_file: {{ $caFile }}
{{- if $b.kubeletTLS.insecureSkipVerify }}
      insecure_skip_verify: true
{{- end }}
    kubernetes_sd_configs:
      - role: node
    relabel_configs:
      - source_labels: [__meta_kubernetes_node_name]
        target_label: node
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: "container_cpu_usage_seconds_total|container_memory_working_set_bytes|container_network_receive_bytes_total|container_network_transmit_bytes_total|container_fs_usage_bytes"
        action: keep
{{- end }}
{{- if $b.scrapeKubeletVolumeStats }}
  # Persistent volume usage, from the kubelet's own metrics.
  - job_name: kubelet
    scheme: https
    authorization:
      credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    tls_config:
      ca_file: {{ $caFile }}
{{- if $b.kubeletTLS.insecureSkipVerify }}
      insecure_skip_verify: true
{{- end }}
    kubernetes_sd_configs:
      - role: node
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: "kubelet_volume_stats_used_bytes"
        action: keep
{{- end }}
{{- end -}}
