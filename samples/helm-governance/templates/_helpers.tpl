{{/* Copyright (c) Microsoft Corporation.
     Licensed under the MIT License. */}}

{{/*
Expand the name of the chart.
*/}}
{{- define "kognition-governance.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kognition-governance.labels" -}}
app.kubernetes.io/name: {{ include "kognition-governance.name" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- if .Values.sampleBundleLabel }}
kognition.dev/sample-bundle: {{ .Values.sampleBundleLabel | quote }}
{{- end }}
{{- end }}
