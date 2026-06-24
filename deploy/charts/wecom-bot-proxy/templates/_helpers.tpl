{{/*
Expand the name of the chart.
*/}}
{{- define "wecom-bot-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "wecom-bot-proxy.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Namespace to use for all namespaced resources.
*/}}
{{- define "wecom-bot-proxy.namespace" -}}
{{ default "costrict-web" .Values.namespace }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "wecom-bot-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "wecom-bot-proxy.labels" -}}
helm.sh/chart: {{ include "wecom-bot-proxy.chart" . }}
{{ include "wecom-bot-proxy.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "wecom-bot-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "wecom-bot-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
