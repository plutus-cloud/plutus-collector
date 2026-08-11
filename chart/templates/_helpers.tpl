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
