{{- define "scrim.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "scrim.fullname" -}}
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

{{- define "scrim.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Labels on every object. */}}
{{- define "scrim.labels" -}}
helm.sh/chart: {{ include "scrim.chart" . }}
app.kubernetes.io/part-of: scrim
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/*
Selector labels for one component. Call with (dict "ctx" $ "component" "hub").
Immutable on a Deployment, so these deliberately exclude version.
*/}}
{{- define "scrim.selectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-%s" (include "scrim.name" .ctx) .component }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "scrim.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{- define "scrim.hubFullname" -}}
{{- printf "%s-hub" (include "scrim.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "scrim.mcpFullname" -}}
{{- printf "%s-mcp" (include "scrim.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
