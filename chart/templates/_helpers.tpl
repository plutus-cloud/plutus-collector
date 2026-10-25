{{/*
Chart name, truncated/sanitized for use in resource names.
*/}}
{{- define "plutus-collector.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified resource name.
*/}}
{{- define "plutus-collector.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "plutus-collector.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "plutus-collector.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "plutus-collector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "plutus-collector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The name of the Secret holding PLUTUS_API_KEY — either the chart-managed one or a
customer-supplied existingSecret.
*/}}
{{- define "plutus-collector.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "plutus-collector.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
The key within that Secret holding the API key.
*/}}
{{- define "plutus-collector.secretKey" -}}
{{- .Values.existingSecretKey | default "api-key" -}}
{{- end -}}

{{/*
The OpenCost base URL the pusher queries: the bundled subchart's in-cluster Service DNS name
when opencost.enabled=true, otherwise the customer-supplied opencost.endpoint.
*/}}
{{- define "plutus-collector.openCostUrl" -}}
{{- if .Values.opencost.enabled -}}
{{- printf "http://%s-opencost.%s.svc.cluster.local:9003" .Release.Name .Release.Namespace -}}
{{- else -}}
{{- required "opencost.endpoint is required when opencost.enabled=false — set it to your existing OpenCost instance's URL" .Values.opencost.endpoint -}}
{{- end -}}
{{- end -}}

{{/*
The Prometheus-compatible store the preflight checks query: the same one OpenCost is configured
to read. preflight.prometheusUrl wins when set; otherwise it is derived from the OpenCost
subchart's own values (opencost.opencost.prometheus.*), using that chart's defaults, so the
checks look where OpenCost looks. Empty when OpenCost is not bundled and no URL was given, in
which case the agent skips the store checks and says so.
*/}}
{{- define "plutus-collector.prometheusUrl" -}}
{{- if .Values.preflight.prometheusUrl -}}
{{- .Values.preflight.prometheusUrl -}}
{{- else if .Values.opencost.enabled -}}
{{- $oc := .Values.opencost.opencost | default dict -}}
{{- $prom := $oc.prometheus | default dict -}}
{{- $ext := $prom.external | default dict -}}
{{- $int := $prom.internal | default dict -}}
{{- if $ext.enabled -}}
{{- $ext.url -}}
{{- else -}}
{{- printf "%s://%s.%s.svc.cluster.local:%v%s" ($int.scheme | default "http") ($int.serviceName | default "prometheus-server") ($int.namespaceName | default "prometheus-system") ($int.port | default 80) ($int.path | default "") -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Environment shared by the pusher and the helm test pod: where OpenCost and its metrics store
are, and how to authenticate to the store. Credentials are read from the same Secret OpenCost is
given (opencost.opencost.prometheus.existingSecretName) and never copied into the chart.
*/}}
{{- define "plutus-collector.preflightEnv" -}}
{{- $oc := .Values.opencost.opencost | default dict -}}
{{- $prom := $oc.prometheus | default dict -}}
{{- $url := include "plutus-collector.prometheusUrl" . -}}
- name: OPENCOST_URL
  value: {{ include "plutus-collector.openCostUrl" . | quote }}
- name: CLUSTER_NAME
  value: {{ required "clusterName is required — set it to this cluster's name, e.g. \"prod-us-east-1\"" .Values.clusterName | quote }}
- name: CATCH_UP_DAYS
  value: {{ .Values.pusher.catchUpDays | quote }}
{{- if $url }}
- name: PROMETHEUS_URL
  value: {{ $url | quote }}
{{- end }}
{{- if $prom.insecureSkipVerify }}
- name: PROMETHEUS_INSECURE_SKIP_VERIFY
  value: "true"
{{- end }}
{{- if $prom.existingSecretName }}
- name: PROMETHEUS_USERNAME
  valueFrom:
    secretKeyRef:
      name: {{ $prom.existingSecretName }}
      key: {{ $prom.username_key | default "DB_BASIC_AUTH_USERNAME" }}
      optional: true
- name: PROMETHEUS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $prom.existingSecretName }}
      key: {{ $prom.password_key | default "DB_BASIC_AUTH_PW" }}
      optional: true
{{- if $prom.bearer_token_key }}
- name: PROMETHEUS_BEARER_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $prom.existingSecretName }}
      key: {{ $prom.bearer_token_key }}
      optional: true
{{- end }}
{{- end }}
{{- if .Values.preflight.clusterLabel }}
- name: PROMETHEUS_CLUSTER_LABEL
  value: {{ .Values.preflight.clusterLabel | quote }}
{{- end }}
{{- if ($oc.customPricing | default dict).enabled }}
- name: PRICING_CUSTOM_CONFIGURED
  value: "true"
{{- end }}
{{- end -}}
