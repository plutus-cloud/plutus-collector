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
Common labels. Components add app.kubernetes.io/component through plutus-collector.componentLabels.
*/}}
{{- define "plutus-collector.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "plutus-collector.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels for the pusher. These are the v1 selector labels unchanged: a Deployment's
selector is immutable, so they must not change across an upgrade.
*/}}
{{- define "plutus-collector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "plutus-collector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Labels and selector labels for the OpenCost and Prometheus components. Called with a dict:
root (the chart context) and component.
*/}}
{{- define "plutus-collector.componentSelectorLabels" -}}
{{- /* The name label is the component's own, never the pusher's: the pusher's Deployment and
       Service select on name and instance alone, so a component that shared its name label
       would be matched by them. For OpenCost this is also the v1 chart's selector, unchanged,
       which an in-place upgrade from v1 needs because a Deployment's selector is immutable. */ -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{/*
Labels for an object (and a pod template) of a component: the selector labels plus a component
label that is not part of any selector.
*/}}
{{- define "plutus-collector.componentLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" }}
{{ include "plutus-collector.componentSelectorLabels" . }}
app.kubernetes.io/component: {{ .component }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- end -}}

{{/*
The name of the Secret holding PLUTUS_API_KEY.
*/}}
{{- define "plutus-collector.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "plutus-collector.fullname" . -}}
{{- end -}}
{{- end -}}

{{- define "plutus-collector.secretKey" -}}
{{- .Values.existingSecretKey | default "api-key" -}}
{{- end -}}

{{/*
An image reference. Called with a dict: img (repository, optional registry, tag, digest),
registry (the global override, may be empty) and defaultTag.

A registry in img.registry, or the first path segment of img.repository when it looks like a
host, is replaced by the global registry when one is set, so one setting points every image at a
private mirror. A digest, when present, is appended and takes precedence over the tag at pull
time.
*/}}
{{- define "plutus-collector.image" -}}
{{- $img := .img -}}
{{- $repo := $img.repository -}}
{{- $registry := $img.registry | default "" -}}
{{- if not $registry -}}
{{- $parts := splitList "/" $repo -}}
{{- if and (gt (len $parts) 1) (or (contains "." (first $parts)) (contains ":" (first $parts))) -}}
{{- $registry = first $parts -}}
{{- $repo = join "/" (rest $parts) -}}
{{- end -}}
{{- end -}}
{{- if .registry -}}{{- $registry = .registry -}}{{- end -}}
{{- $tag := $img.tag | default .defaultTag -}}
{{- $ref := printf "%s:%s" $repo $tag -}}
{{- if $registry -}}{{- $ref = printf "%s/%s" (trimSuffix "/" $registry) $ref -}}{{- end -}}
{{- if $img.digest -}}{{- $ref = printf "%s@%s" $ref $img.digest -}}{{- end -}}
{{- $ref -}}
{{- end -}}

{{/*
The effective configuration: the chart's values after the v1 translation shim, validated, as
YAML. Every template reads this rather than .Values, so v1 values files keep working for one
release and a v1 key the shim cannot honour fails the install with a pointer to MIGRATING.md
instead of being silently ignored. Usage:
  {{- $c := include "plutus-collector.cfg" . | fromYaml -}}
*/}}
{{- define "plutus-collector.cfg" -}}
{{- $v := deepCopy .Values -}}
{{- $dep := list -}}
{{- $moved := "see MIGRATING.md" -}}

{{- if .Values.pushIntervalMinutes -}}
{{- if not $v.pusher.pushIntervalMinutes -}}{{- $_ := set $v.pusher "pushIntervalMinutes" .Values.pushIntervalMinutes -}}{{- end -}}
{{- $dep = append $dep "pushIntervalMinutes is deprecated: the agent now pushes daily on a UTC schedule (pusher.schedule); pusher.pushIntervalMinutes keeps a fixed interval" -}}
{{- end -}}
{{- if .Values.imagePullSecrets -}}
{{- if not $v.global.imagePullSecrets -}}{{- $_ := set $v.global "imagePullSecrets" .Values.imagePullSecrets -}}{{- end -}}
{{- $dep = append $dep "imagePullSecrets is deprecated: use global.imagePullSecrets" -}}
{{- end -}}
{{- if .Values.image -}}
{{- range $k, $x := .Values.image -}}
{{- if not (has $k (list "repository" "tag" "pullPolicy")) -}}{{- fail (printf "image.%s is not a valid key: use pusher.image (%s)" $k $moved) -}}{{- end -}}
{{- $_ := set $v.pusher.image $k $x -}}
{{- end -}}
{{- $dep = append $dep "image is deprecated: use pusher.image" -}}
{{- end -}}
{{- if .Values.resources -}}
{{- $_ := set $v.pusher "resources" .Values.resources -}}
{{- $dep = append $dep "resources is deprecated: use pusher.resources" -}}
{{- end -}}

{{- /* v1 values passed through to the OpenCost subchart */ -}}
{{- $v1 := .Values.opencost.opencost | default dict -}}
{{- if $v1 -}}
{{- range $k, $x := $v1 -}}
{{- if not (has $k (list "exporter" "prometheus" "customPricing")) -}}
{{- fail (printf "opencost.opencost.%s is no longer passed through: v2 does not use the OpenCost chart (%s). Use opencost.extraEnv, opencost.extraContainers or opencost.extraVolumes for anything the chart does not expose." $k $moved) -}}
{{- end -}}
{{- end -}}

{{- $ex := $v1.exporter | default dict -}}
{{- range $k, $x := $ex -}}
{{- if ne $k "defaultClusterId" -}}{{- fail (printf "opencost.opencost.exporter.%s is no longer passed through (%s)" $k $moved) -}}{{- end -}}
{{- end -}}
{{- if and $ex.defaultClusterId (ne $ex.defaultClusterId $v.clusterName) -}}
{{- $dep = append $dep (printf "opencost.opencost.exporter.defaultClusterId (%q) differs from clusterName (%q) and is ignored: OpenCost's cluster id is now clusterName" $ex.defaultClusterId $v.clusterName) -}}
{{- end -}}

{{- $p := $v1.prometheus | default dict -}}
{{- range $k, $x := $p -}}
{{- if not (has $k (list "existingSecretName" "username_key" "password_key" "bearer_token_key" "insecureSkipVerify" "internal" "external")) -}}
{{- fail (printf "opencost.opencost.prometheus.%s is not supported in v2 (%s)" $k $moved) -}}
{{- end -}}
{{- end -}}
{{- $ext := $p.external | default dict -}}
{{- range $k, $x := $ext -}}
{{- if not (has $k (list "enabled" "url")) -}}{{- fail (printf "opencost.opencost.prometheus.external.%s is not supported in v2 (%s)" $k $moved) -}}{{- end -}}
{{- end -}}
{{- $int := $p.internal | default dict -}}
{{- range $k, $x := $int -}}
{{- if not (has $k (list "enabled" "serviceName" "namespaceName" "port" "path" "scheme")) -}}{{- fail (printf "opencost.opencost.prometheus.internal.%s is not supported in v2 (%s)" $k $moved) -}}{{- end -}}
{{- end -}}

{{- if $p -}}
{{- if $ext.enabled -}}
{{- $_ := set $v.prometheus "mode" "external" -}}
{{- $_ := set $v.prometheus.external "url" ($ext.url | default "") -}}
{{- else if and (hasKey $int "enabled") (not $int.enabled) -}}
{{- fail (printf "opencost.opencost.prometheus.internal.enabled=false needs opencost.opencost.prometheus.external.enabled=true and a url (%s)" $moved) -}}
{{- else if or $int.serviceName $int.namespaceName $int.port $int.path $int.scheme -}}
{{- $_ := set $v.prometheus "mode" "external" -}}
{{- $_ := set $v.prometheus.external "url" (printf "%s://%s.%s.svc.cluster.local:%v%s" ($int.scheme | default "http") ($int.serviceName | default "prometheus-server") ($int.namespaceName | default "prometheus-system") ($int.port | default 80) ($int.path | default "")) -}}
{{- end -}}
{{- if $p.existingSecretName -}}
{{- $_ := set $v.prometheus.external.auth "existingSecret" $p.existingSecretName -}}
{{- if $p.bearer_token_key -}}
{{- $_ := set $v.prometheus.external.auth "type" "bearer" -}}
{{- $_ := set $v.prometheus.external.auth "tokenKey" $p.bearer_token_key -}}
{{- else -}}
{{- $_ := set $v.prometheus.external.auth "type" "basic" -}}
{{- $_ := set $v.prometheus.external.auth "usernameKey" ($p.username_key | default "DB_BASIC_AUTH_USERNAME") -}}
{{- $_ := set $v.prometheus.external.auth "passwordKey" ($p.password_key | default "DB_BASIC_AUTH_PW") -}}
{{- end -}}
{{- end -}}
{{- if $p.insecureSkipVerify -}}{{- $_ := set $v.prometheus.external "insecureSkipVerify" true -}}{{- end -}}
{{- end -}}

{{- $cp := $v1.customPricing | default dict -}}
{{- range $k, $x := $cp -}}
{{- if not (has $k (list "enabled" "provider" "costModel")) -}}{{- fail (printf "opencost.opencost.customPricing.%s is not supported in v2 (%s)" $k $moved) -}}{{- end -}}
{{- end -}}
{{- if and $cp.provider (ne $cp.provider "custom") -}}{{- fail (printf "opencost.opencost.customPricing.provider %q is not supported in v2: only custom rates are (%s)" $cp.provider $moved) -}}{{- end -}}
{{- if $cp.enabled -}}
{{- $_ := set $v.opencost.pricing "provider" "custom" -}}
{{- range $k, $x := ($cp.costModel | default dict) -}}
{{- if eq $k "description" -}}
{{- else if hasKey $v.opencost.pricing.custom $k -}}
{{- $_ := set $v.opencost.pricing.custom $k (toString $x) -}}
{{- else -}}
{{- fail (printf "opencost.opencost.customPricing.costModel.%s is not supported in v2 (%s)" $k $moved) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $dep = append $dep "opencost.opencost.* (values for the OpenCost subchart) is deprecated: use opencost.pricing, prometheus.external and clusterName; the translation is removed in the next minor release" -}}
{{- end -}}
{{- $_ := unset $v.opencost "opencost" -}}

{{- /* validation, after translation */ -}}
{{- if and $v.opencost.enabled (eq $v.prometheus.mode "external") (not $v.prometheus.external.url) -}}
{{- fail "prometheus.external.url is required when prometheus.mode=external" -}}
{{- end -}}
{{- if and $v.opencost.enabled (eq $v.prometheus.mode "none") (not $v.prometheus.none.acknowledgeNodesProxy) -}}
{{- fail "prometheus.mode=none is experimental and renders the nodes/proxy permission, which can be used to run commands in any pod on a node: set prometheus.none.acknowledgeNodesProxy=true to accept that" -}}
{{- end -}}
{{- if not $v.opencost.enabled -}}
{{- if not $v.opencost.endpoint -}}{{- fail "opencost.endpoint is required when opencost.enabled=false: set it to your existing OpenCost instance's URL" -}}{{- end -}}
{{- end -}}
{{- if and (eq $v.prometheus.mode "external") (ne $v.prometheus.external.auth.type "none") (not $v.prometheus.external.auth.existingSecret) -}}
{{- fail "prometheus.external.auth.existingSecret is required when prometheus.external.auth.type is not none: credentials are read from a Secret you create, never from values" -}}
{{- end -}}
{{- if and $v.opencost.enabled (eq $v.opencost.pricing.provider "custom") -}}
{{- range $k := (list "CPU" "RAM" "storage") -}}
{{- if not (get $v.opencost.pricing.custom $k) -}}{{- fail (printf "opencost.pricing.custom.%s is required when opencost.pricing.provider=custom" $k) -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- if and (gt (int $v.pusher.catchUpDays) 0) (eq $v.prometheus.mode "bundled") (lt (int (trimSuffix "d" (regexFind "^[0-9]+d$" $v.prometheus.bundled.retention))) (add1 (int $v.pusher.catchUpDays))) -}}
{{- fail (printf "prometheus.bundled.retention (%s) must be at least pusher.catchUpDays + 1 days (%d) in whole days, or catch-up days will have no data" $v.prometheus.bundled.retention (add1 (int $v.pusher.catchUpDays))) -}}
{{- end -}}

{{- $_ := set $v "deprecations" $dep -}}
{{- toYaml $v -}}
{{- end -}}

{{/*
The OpenCost API URL the pusher queries.
*/}}
{{- define "plutus-collector.openCostUrl" -}}
{{- $c := .cfg -}}
{{- if $c.opencost.enabled -}}
{{- printf "http://%s-opencost.%s.svc.cluster.local:9003" (include "plutus-collector.fullname" .root) .root.Release.Namespace -}}
{{- else -}}
{{- $c.opencost.endpoint -}}
{{- end -}}
{{- end -}}

{{/*
The metrics store OpenCost reads and the preflight checks query. Empty for mode none, and for an
external OpenCost with no store configured.
*/}}
{{- define "plutus-collector.prometheusUrl" -}}
{{- $c := .cfg -}}
{{- if and $c.opencost.enabled (eq $c.prometheus.mode "bundled") -}}
{{- printf "http://%s-prometheus.%s.svc.cluster.local:9090" (include "plutus-collector.fullname" .root) .root.Release.Namespace -}}
{{- else if eq $c.prometheus.mode "external" -}}
{{- $c.prometheus.external.url -}}
{{- end -}}
{{- end -}}

{{/*
Prometheus credentials for a container, as env vars. Called with a dict: cfg and style, either
"opencost" (the variable names OpenCost reads) or "agent" (the names the pusher reads).
*/}}
{{- define "plutus-collector.prometheusAuthEnv" -}}
{{- $c := .cfg -}}
{{- $a := $c.prometheus.external.auth -}}
{{- if eq $c.prometheus.mode "external" -}}
{{- if eq $a.type "basic" }}
- name: {{ ternary "DB_BASIC_AUTH_USERNAME" "PROMETHEUS_USERNAME" (eq .style "opencost") }}
  valueFrom:
    secretKeyRef:
      name: {{ $a.existingSecret }}
      key: {{ $a.usernameKey }}
- name: {{ ternary "DB_BASIC_AUTH_PW" "PROMETHEUS_PASSWORD" (eq .style "opencost") }}
  valueFrom:
    secretKeyRef:
      name: {{ $a.existingSecret }}
      key: {{ $a.passwordKey }}
{{- else if eq $a.type "bearer" }}
- name: {{ ternary "DB_BEARER_TOKEN" "PROMETHEUS_BEARER_TOKEN" (eq .style "opencost") }}
  valueFrom:
    secretKeyRef:
      name: {{ $a.existingSecret }}
      key: {{ $a.tokenKey }}
{{- else if eq $a.type "mtls" }}
- name: {{ ternary "PROM_MTLS_AUTH_CA_FILE" "PROMETHEUS_CA_FILE" (eq .style "opencost") }}
  value: /etc/prometheus-mtls/{{ $a.caKey }}
- name: {{ ternary "PROM_MTLS_AUTH_CRT_FILE" "PROMETHEUS_CLIENT_CERT_FILE" (eq .style "opencost") }}
  value: /etc/prometheus-mtls/{{ $a.certKey }}
- name: {{ ternary "PROM_MTLS_AUTH_KEY_FILE" "PROMETHEUS_CLIENT_KEY_FILE" (eq .style "opencost") }}
  value: /etc/prometheus-mtls/{{ $a.keyKey }}
{{- end }}
{{- if $c.prometheus.external.tenantId }}
- name: {{ ternary "PROMETHEUS_HEADER_X_SCOPE_ORGID" "PROMETHEUS_TENANT_ID" (eq .style "opencost") }}
  value: {{ $c.prometheus.external.tenantId | quote }}
{{- end }}
{{- if $c.prometheus.external.insecureSkipVerify }}
- name: {{ ternary "INSECURE_SKIP_VERIFY" "PROMETHEUS_INSECURE_SKIP_VERIFY" (eq .style "opencost") }}
  value: "true"
{{- end }}
{{- end -}}
{{- end -}}

{{/*
The volume and mount that deliver mTLS material from the customer's Secret, when that auth type is
used. Called with cfg.
*/}}
{{- define "plutus-collector.mtlsVolume" -}}
{{- if and (eq .prometheus.mode "external") (eq .prometheus.external.auth.type "mtls") }}
- name: prometheus-mtls
  secret:
    secretName: {{ .prometheus.external.auth.existingSecret }}
{{- end -}}
{{- end -}}

{{- define "plutus-collector.mtlsMount" -}}
{{- if and (eq .prometheus.mode "external") (eq .prometheus.external.auth.type "mtls") }}
- name: prometheus-mtls
  mountPath: /etc/prometheus-mtls
  readOnly: true
{{- end -}}
{{- end -}}

{{/*
Environment shared by the pusher and the helm test pod: where OpenCost and its metrics store are
and how to authenticate to the store. Called with a dict: root and cfg.
*/}}
{{- define "plutus-collector.preflightEnv" -}}
{{- $c := .cfg -}}
{{- $url := include "plutus-collector.prometheusUrl" . -}}
- name: OPENCOST_URL
  value: {{ include "plutus-collector.openCostUrl" . | quote }}
- name: CLUSTER_NAME
  value: {{ $c.clusterName | quote }}
- name: CATCH_UP_DAYS
  value: {{ $c.pusher.catchUpDays | quote }}
- name: PROMETHEUS_MODE
  value: {{ $c.prometheus.mode | quote }}
{{- if $url }}
- name: PROMETHEUS_URL
  value: {{ $url | quote }}
{{- end }}
{{- include "plutus-collector.prometheusAuthEnv" (dict "cfg" $c "style" "agent") }}
{{- if and (eq $c.prometheus.mode "external") $c.prometheus.external.clusterLabel }}
- name: PROMETHEUS_CLUSTER_LABEL
  value: {{ $c.prometheus.external.clusterLabel | quote }}
{{- end }}
{{- if eq $c.opencost.pricing.provider "custom" }}
- name: PRICING_CUSTOM_CONFIGURED
  value: "true"
{{- end }}
{{- end -}}
