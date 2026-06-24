{{/*
Expand the name of the chart.
*/}}
{{- define "gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "gateway.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{/*
Namespace to use for all namespaced resources.
*/}}
{{- define "gateway.namespace" -}}
{{ default "costrict-web" .Values.namespace }}
{{- end }}


{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "gateway.labels" -}}
helm.sh/chart: {{ include "gateway.chart" . }}
{{ include "gateway.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
nginx-router 名称
*/}}
{{- define "gateway.nginxRouter.name" -}}
{{- printf "%s-nginx-router" (include "gateway.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
nginx-router fullname
*/}}
{{- define "gateway.nginxRouter.fullname" -}}
{{- printf "%s-nginx-router" (include "gateway.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
nginx-router selector labels
*/}}
{{- define "gateway.nginxRouter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gateway.nginxRouter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
nginx-router common labels
*/}}
{{- define "gateway.nginxRouter.labels" -}}
helm.sh/chart: {{ include "gateway.chart" . }}
{{ include "gateway.nginxRouter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
