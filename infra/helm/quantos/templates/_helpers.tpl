{{/*
Helpers shared by every template.

The interesting ones are quantos.serviceValues and quantos.env: the first merges
a service's overrides onto .Values.defaults so a values file only has to state
what differs, and the second is the single place the QUANTOS_* variable names
appear. Those names must match internal/config.applyEnv exactly - a name that
does not exist there is read by nothing.
*/}}

{{- define "quantos.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "quantos.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s" (include "quantos.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "quantos.labels" -}}
app.kubernetes.io/part-of: quantos
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
quantos.io/mode: {{ .Values.mode }}
{{- end -}}

{{/*
Selector labels. Deliberately narrower than quantos.labels: a Deployment's
selector is immutable, so anything that changes between releases - the chart
version, the app version - must not be in it.
*/}}
{{- define "quantos.selectorLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end -}}

{{/*
Merge a service's overrides onto the defaults. Call as:
  {{- $svc := include "quantos.serviceValues" (dict "root" $ "name" $name) | fromYaml }}
*/}}
{{- define "quantos.serviceValues" -}}
{{- $svc := index .root.Values.services .name -}}
{{- $merged := mergeOverwrite (deepCopy .root.Values.defaults) $svc -}}
{{- toYaml $merged -}}
{{- end -}}

{{/*
Image reference. registry may be empty for a local build.
*/}}
{{- define "quantos.image" -}}
{{- $tag := default .root.Chart.AppVersion .root.Values.image.tag -}}
{{- if .root.Values.image.registry -}}
{{ .root.Values.image.registry }}/{{ .root.Values.image.repository }}/{{ .name }}:{{ $tag }}
{{- else -}}
{{ .root.Values.image.repository }}/{{ .name }}:{{ $tag }}
{{- end -}}
{{- end -}}

{{/*
The config file name, which is also the QUANTOS_CONFIG value. One file per mode
so a release cannot accidentally load another mode's sheet.
*/}}
{{- define "quantos.configFile" -}}
quantos.{{ .Values.mode }}.yaml
{{- end -}}

{{/*
Pod security context. Distroless nonroot is uid 65532 (deploy/Dockerfile).
*/}}
{{- define "quantos.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "quantos.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}

{{/*
Non-secret environment. Every name here exists in internal/config.applyEnv.
*/}}
{{- define "quantos.env" -}}
- name: QUANTOS_CONFIG
  value: /etc/quantos/{{ include "quantos.configFile" .root }}
- name: QUANTOS_MODE
  value: {{ .root.Values.mode }}
- name: QUANTOS_LOG_LEVEL
  value: {{ .root.Values.config.log.level }}
- name: QUANTOS_LOG_FORMAT
  value: {{ .root.Values.config.log.format }}
- name: QUANTOS_SERVICE
  value: {{ .name }}
- name: QUANTOS_HTTP_ADDR
  value: ":{{ .port }}"
- name: QUANTOS_MARKET_PROVIDER
  value: {{ .root.Values.config.market.provider }}
- name: QUANTOS_STRATEGY_DIR
  value: /app/strategies
{{- if eq .root.Values.mode "embedded" }}
{{- /*
  Embedded mode is the in-process stack: memory bus, memory store, no secrets.
  It is quantos run in a container, and it is the only mode where auth may be
  off - config.Validate refuses that in cluster mode.
*/}}
- name: QUANTOS_BUS_DRIVER
  value: memory
- name: QUANTOS_STORE_DRIVER
  value: memory
{{- else }}
- name: QUANTOS_BUS_DRIVER
  value: kafka
- name: QUANTOS_STORE_DRIVER
  value: sql
- name: QUANTOS_CLICKHOUSE_URL
  value: {{ .root.Values.backing.clickhouseUrl | quote }}
- name: QUANTOS_ARTIFACT_DIR
  value: /var/lib/quantos/models
{{- end }}
{{- if eq .root.Values.mode "compose" }}
{{- /*
  compose mode points at in-cluster dependencies the operator supplies. The DSN
  still comes from a Secret; only the non-secret addresses are values.
*/}}
- name: QUANTOS_REDIS_ADDR
  value: {{ .root.Values.backing.redisAddr | quote }}
- name: QUANTOS_KAFKA_BROKERS
  value: {{ .root.Values.backing.kafkaBrokers | quote }}
{{- end }}
{{- if .llm }}
- name: QUANTOS_LLM_PROVIDER
  value: {{ .root.Values.config.llm.provider }}
{{- else }}
{{- /*
  Every other service is pinned to the mock provider. It holds no key (see the
  envFrom below and the IRSA grant), so this is belt and braces - but a service
  that cannot call is a service that cannot leak a key in a stack trace.
*/}}
- name: QUANTOS_LLM_PROVIDER
  value: mock
{{- end }}
{{- end -}}

{{/*
Secret-derived environment. Names are the QUANTOS_* keys the projected Secrets
carry, which are the ones internal/config.applyEnv reads.
*/}}
{{- define "quantos.envFrom" -}}
{{- if ne .root.Values.mode "embedded" }}
- secretRef:
    name: quantos-store
{{- if eq .root.Values.mode "cluster" }}
- secretRef:
    name: quantos-bus
{{- end }}
{{- if .llm }}
- secretRef:
    name: quantos-llm
{{- end }}
{{- end }}
{{- end -}}

{{/*
Probes. Identical for every service because every binary serves them
(cli.Spec.WithHTTP).
*/}}
{{- define "quantos.probes" -}}
livenessProbe:
  {{- /*
    /healthz reports degraded subsystems in the body but still returns 200. A
    degraded service must not be killed: degradation is a state this platform is
    designed to run in.
  */}}
  httpGet: {path: /healthz, port: http}
  initialDelaySeconds: 10
  periodSeconds: 15
  timeoutSeconds: 3
  failureThreshold: 3
readinessProbe:
  {{- /*
    /readyz returns 503 when the universe is empty, no strategies are loaded, or
    App.CanEmitSignals is false. That last case is the platform suspending
    emission because provenance cannot be persisted (ADR-003); removing the pod
    from the Service is what makes the suppression visible.
  */}}
  httpGet: {path: /readyz, port: http}
  initialDelaySeconds: 5
  periodSeconds: 10
  timeoutSeconds: 3
  failureThreshold: 3
startupProbe:
  {{- /* Feature warm-up reads history before the first bar. */}}
  httpGet: {path: /healthz, port: http}
  periodSeconds: 5
  failureThreshold: 24
{{- end -}}
