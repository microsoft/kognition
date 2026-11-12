{{/* Copyright (c) Microsoft Corporation.
     Licensed under the MIT License. */}}

{{- define "kognition-knowledge-azuresearch.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kognition-knowledge-azuresearch.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "kognition-knowledge-azuresearch.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kognition-knowledge-azuresearch.labels" -}}
app.kubernetes.io/name: {{ include "kognition-knowledge-azuresearch.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kognition-knowledge-azuresearch.serviceAccountName" -}}
{{- if .Values.runtime.serviceAccount.create -}}
{{- default (include "kognition-knowledge-azuresearch.fullname" .) .Values.runtime.serviceAccount.name -}}
{{- else -}}
{{- required "runtime.serviceAccount.name is required when service account creation is disabled" .Values.runtime.serviceAccount.name -}}
{{- end -}}
{{- end -}}