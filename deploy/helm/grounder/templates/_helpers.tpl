{{/*
Territory Grounder chart helpers. Every name/label/selector derives from values.yaml so there is one
authoritative configuration contract (spec/009 REQ-907 / INV-15).
*/}}

{{- define "grounder.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fullname: stable, values-overridable. Kept short so the derived per-service names stay within 63 chars.
*/}}
{{- define "grounder.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "grounder.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels stamped on every rendered object. */}}
{{- define "grounder.labels" -}}
helm.sh/chart: {{ include "grounder.chart" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: territory-grounder
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
territory-grounder/spec: spec-009-kubernetes-deploy
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* Per-component selector labels. Call with (dict "root" . "component" "grounder"). */}}
{{- define "grounder.selectorLabels" -}}
app.kubernetes.io/name: {{ include "grounder.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* Per-component object name, e.g. grounder-postgres. Call with (dict "root" . "component" "postgres"). */}}
{{- define "grounder.componentName" -}}
{{- printf "%s-%s" (include "grounder.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Resolve the grounder control-plane image: digest wins over tag (spec/009 REQ-903 — pin by digest).
*/}}
{{- define "grounder.image" -}}
{{- $img := .Values.grounder.image -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository $img.tag -}}
{{- end -}}
{{- end -}}

{{/*
Resolve the Runner worker image (BIN=worker family): digest wins over tag, mirroring grounder.image.
*/}}
{{- define "grounder.workerImage" -}}
{{- $img := .Values.worker.image -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository $img.tag -}}
{{- end -}}
{{- end -}}

{{/*
Emit a container env entry sourced from the credential Secret (spec/009 REQ-904 / INV-13). Never a
literal. Call with (dict "root" . "name" "TG_RUNTIME_DSN" "key" .Values.secrets.keys.runtimeDSN). Pass
"optional" true for a key an operator may legitimately leave out of the Secret (e.g. a provider key for a
model backend they do not use) so kubelet does not fail container creation on its absence.
*/}}
{{- define "grounder.secretEnv" -}}
- name: {{ .name }}
  valueFrom:
    secretKeyRef:
      name: {{ .root.Values.secrets.name }}
      key: {{ .key }}
      {{- if .optional }}
      optional: true
      {{- end }}
{{- end -}}
