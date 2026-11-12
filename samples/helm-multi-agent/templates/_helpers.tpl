{{/* Copyright (c) Microsoft Corporation.
     Licensed under the MIT License. */}}

{{/*
Common labels for all resources.
*/}}
{{- define "kognition-multi-agent.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- if .Values.sampleBundleLabel }}
kognition.dev/sample-bundle: {{ .Values.sampleBundleLabel | quote }}
{{- end }}
{{- end -}}
