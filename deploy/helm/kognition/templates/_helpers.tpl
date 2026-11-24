{{/* Copyright (c) Microsoft Corporation.
     Licensed under the MIT License. */}}

{{- define "kognition.artifactSigningEnv" -}}
{{- if .Values.artifactSigning.existingSecret }}
- name: KOGNITION_ARTIFACT_SIGNING_KEYS_FILE
  value: /etc/kognition/artifact-signers/public-keys.json
{{- end }}
{{- end }}

{{- define "kognition.workloadIdentityStatusEnv" -}}
{{- if .Values.workloadIdentityStatus.enabled }}
{{- $domain := required "workloadIdentityStatus.trustDomain is required" .Values.workloadIdentityStatus.trustDomain }}
{{- $identity := required "workloadIdentityStatus.controllerID is required" .Values.workloadIdentityStatus.controllerID }}
{{- if not (hasPrefix (printf "spiffe://%s/" $domain) $identity) }}
{{- fail "workloadIdentityStatus.controllerID must match the configured trust domain" }}
{{- end }}
{{- $_ := required "workloadIdentityStatus.existingRegistryConfigMap is required" .Values.workloadIdentityStatus.existingRegistryConfigMap }}
{{- $_ := required "workloadIdentityStatus.runtimeNamespace is required" .Values.workloadIdentityStatus.runtimeNamespace }}
- name: KOGNITION_SPIFFE_TRUST_DOMAIN
  value: {{ $domain | quote }}
- name: KOGNITION_SPIFFE_ID
  value: {{ $identity | quote }}
- name: KOGNITION_SPIFFE_ENDPOINT_SOCKET
  value: unix:///spiffe-workload-api/spire-agent.sock
- name: KOGNITION_WORKLOAD_STATUS_REGISTRY_FILE
  value: /etc/kognition/workload-status/registry.json
{{- if .Values.workloadIdentityStatus.managedWorkloads.enabled }}
{{- if or (ne .Values.mode "kubernetes") (not .Values.networkPolicy.enabled) }}
{{- fail "managed SPIRE workloads require kubernetes mode and enforced NetworkPolicies" }}
{{- end }}
{{- if ne .Values.k8sController.agentNamespace .Values.workloadIdentityStatus.runtimeNamespace }}
{{- fail "managed SPIRE runtimeNamespace must equal k8sController.agentNamespace" }}
{{- end }}
{{- $image := required "managed SPIRE proxyImage is required" .Values.workloadIdentityStatus.managedWorkloads.proxyImage }}
{{- if not (regexMatch "^[^[:space:]]+@sha256:[a-f0-9]{64}$" $image) }}
{{- fail "managed SPIRE proxyImage must be digest pinned" }}
{{- end }}
{{- $gateways := .Values.workloadIdentityStatus.managedWorkloads.gateways }}
{{- if ne (len $gateways) 4 }}{{- fail "managed SPIRE workloads require four gateway bindings" }}{{- end }}
{{- range $plane := list "model" "tool" "memory" "communication" }}
{{- $gateway := required (printf "managed SPIRE %s gateway binding is required" $plane) (get $gateways $plane) }}
{{- if or (not (hasPrefix "https://" (required "gateway URL is required" $gateway.url))) (not (hasPrefix (printf "spiffe://%s/" $domain) (required "gateway peerID is required" $gateway.peerID))) }}
{{- fail "managed SPIRE gateways require HTTPS and same-domain pinned peers" }}
{{- end }}
{{- end }}
- name: KOGNITION_WORKLOAD_IDENTITY_MODE
  value: spire
- name: KOGNITION_WORKLOAD_PROXY_IMAGE
  value: {{ $image | quote }}
- name: KOGNITION_SPIFFE_GATEWAY_ENDPOINTS
  value: {{ $gateways | toJson | quote }}
{{- end }}
{{- end }}
{{- if and .Values.workloadIdentityStatus.managedWorkloads.enabled (not .Values.workloadIdentityStatus.enabled) }}
{{- fail "managed SPIRE workloads require workloadIdentityStatus.enabled" }}
{{- end }}
{{- end }}

{{- define "kognition.workloadIdentityStatusMounts" -}}
{{- if .Values.workloadIdentityStatus.enabled }}
- name: workload-api
  mountPath: /spiffe-workload-api
  readOnly: true
- name: workload-status-registry
  mountPath: /etc/kognition/workload-status
  readOnly: true
{{- end }}
{{- end }}

{{- define "kognition.workloadIdentityStatusVolumes" -}}
{{- if .Values.workloadIdentityStatus.enabled }}
- name: workload-api
  csi:
    driver: csi.spiffe.io
    readOnly: true
- name: workload-status-registry
  configMap:
    name: {{ required "workloadIdentityStatus.existingRegistryConfigMap is required" .Values.workloadIdentityStatus.existingRegistryConfigMap | quote }}
    items:
      - key: {{ required "workloadIdentityStatus.registryKey is required" .Values.workloadIdentityStatus.registryKey | quote }}
        path: registry.json
{{- end }}
{{- end }}

{{- define "kognition.artifactSigningMount" -}}
{{- if .Values.artifactSigning.existingSecret }}
- name: artifact-signers
  mountPath: /etc/kognition/artifact-signers
  readOnly: true
{{- end }}
{{- end }}

{{- define "kognition.artifactSigningVolume" -}}
{{- if .Values.artifactSigning.existingSecret }}
- name: artifact-signers
  secret:
    secretName: {{ .Values.artifactSigning.existingSecret | quote }}
    items:
      - key: {{ required "artifactSigning.publicKeysKey is required" .Values.artifactSigning.publicKeysKey | quote }}
        path: public-keys.json
{{- end }}
{{- end }}

{{- define "kognition.runtimeRecoveryEnv" -}}
{{- if .Values.runtimeRecovery.existingSecret }}
- name: KOGNITION_RUNTIME_RECOVERY_KEYS_FILE
  value: /etc/kognition/runtime-recovery/keys.json
- name: KOGNITION_RUNTIME_RECOVERY_RETENTION
  value: {{ .Values.runtimeRecovery.retention | quote }}
{{- end }}
{{- end }}

{{- define "kognition.runtimeRecoveryMount" -}}
{{- if .Values.runtimeRecovery.existingSecret }}
- name: runtime-recovery-keys
  mountPath: /etc/kognition/runtime-recovery
  readOnly: true
{{- end }}
{{- end }}

{{- define "kognition.runtimeRecoveryVolume" -}}
{{- if .Values.runtimeRecovery.existingSecret }}
- name: runtime-recovery-keys
  secret:
    secretName: {{ .Values.runtimeRecovery.existingSecret | quote }}
    items:
      - key: {{ required "runtimeRecovery.secretKey is required" .Values.runtimeRecovery.secretKey | quote }}
        path: keys.json
{{- end }}
{{- end }}

{{- define "kognition.controllerNamespaces" -}}
{{- $namespaces := concat (list .Release.Namespace) (.Values.k8sController.managedNamespaces | default list) -}}
{{- if .Values.k8sController.agentNamespace -}}
{{- if eq .Values.k8sController.agentNamespace .Release.Namespace -}}
{{- fail "k8sController.agentNamespace must differ from the installation namespace; use a separate enrolled workload namespace" -}}
{{- end -}}
{{- $namespaces = append $namespaces .Values.k8sController.agentNamespace -}}
{{- end -}}
{{- range $namespace := $namespaces -}}
{{- if or (not (kindIs "string" $namespace)) (gt (len $namespace) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $namespace)) -}}
{{- fail "controller namespaces must be explicit DNS-label namespace names (no wildcards or empty entries)" -}}
{{- end -}}
{{- end -}}
{{- uniq $namespaces | toJson -}}
{{- end -}}

{{/*
Expand the name of the chart.
*/}}
{{- define "kognition.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kognition.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Render a port-scoped ingress rule for kubelet HTTP/TCP probes. Native
NetworkPolicy cannot select node hosts, so enforcing-CNI installations supply
their node CIDRs through networkPolicy.kubeletProbeIngressCIDRs.
*/}}
{{- define "kognition.kubeletProbeIngress" -}}
{{- if gt (len .cidrs) 0 }}
- from:
  {{- range .cidrs }}
  - ipBlock:
      cidr: {{ . | quote }}
  {{- end }}
  ports:
    - port: {{ .port }}
      protocol: TCP
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kognition.labels" -}}
helm.sh/chart: {{ include "kognition.chart" . }}
app.kubernetes.io/name: {{ include "kognition.name" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kognition
{{- end }}

{{- define "kognition.resourceWebhookCABundle" -}}
{{- if .Values.webhook.caBundle }}
{{- if .Values.webhook.certManager.enabled }}
{{- fail "webhook.caBundle requires operator-provisioned TLS with webhook.certManager.enabled=false" }}
{{- end }}
caBundle: {{ .Values.webhook.caBundle | quote }}
{{- end }}
{{- end }}

{{/*
Chart label
*/}}
{{- define "kognition.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Service account name
*/}}
{{- define "kognition.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kognition.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "kognition.controllerServiceAccountName" -}}
{{- $account := default (dict) .Values.k8sController.serviceAccount -}}
{{- $name := default (printf "%s-controller" (include "kognition.fullname" . | trunc 52 | trimSuffix "-")) $account.name -}}
{{- if or (eq $name "default") (eq $name (include "kognition.serviceAccountName" .)) -}}
{{- fail "controller ServiceAccount must be distinct from the shared platform and default ServiceAccounts" -}}
{{- end -}}
{{- $name -}}
{{- end -}}

{{/*
Workflow collaboration settings. Member tasks run as ordinary AgentJobs, so enabling
it needs only the native executor (which already requires fail-closed admission).
*/}}
{{- define "kognition.workflowCollaboration" -}}
{{- $values := fromYaml (toYaml .Values) -}}
{{- $executor := dig "k8sController" "workflowExecutor" (dict) $values -}}
{{- $collaboration := dig "collaboration" (dict) $executor -}}
{{- if dig "enabled" false $collaboration -}}
{{- if not (dig "enabled" false $executor) -}}
{{- fail "workflowExecutor.collaboration requires k8sController.workflowExecutor.enabled" -}}
{{- end -}}
{{- end -}}
{{- $collaboration | toJson -}}
{{- end -}}

{{/*
PostgreSQL DSN
*/}}
{{- define "kognition.databaseSecretName" -}}
{{- default (printf "%s-db" (include "kognition.fullname" .)) .Values.postgres.auth.existingSecret -}}
{{- end }}

{{- define "kognition.databaseDSN" -}}
{{- $username := .Values.postgres.auth.username | urlquery | replace "+" "%20" -}}
{{- $password := .Values.postgres.auth.password | urlquery | replace "+" "%20" -}}
{{- $database := .Values.postgres.auth.database | urlquery | replace "+" "%20" -}}
{{- if .Values.postgres.enabled }}
{{- printf "postgres://%s:%s@%s-postgres:5432/%s?sslmode=disable" $username $password (include "kognition.fullname" .) $database }}
{{- else }}
{{- printf "postgres://%s:%s@%s:%v/%s?sslmode=%s" $username $password .Values.postgres.external.host (int .Values.postgres.external.port) $database (.Values.postgres.external.sslmode | urlquery) }}
{{- end }}
{{- end }}

{{/*
Image helper. First-party images use bare repository names ("apiserver",
"memory-service", ...) and get the optional global.imageRegistry prefix.
Third-party images use namespaced repositories ("pgvector/pgvector",
"library/busybox", ...) and are passed through unchanged so the registry
override never accidentally remaps them.
*/}}
{{- define "kognition.image" -}}
{{- $registry := .global.imageRegistry | default "" -}}
{{- $repo := .image.repository -}}
{{- $tag := .image.tag | default .defaultTag -}}
{{- if and $registry (not (contains "/" $repo)) -}}
{{- $repo = printf "%s/%s" $registry $repo -}}
{{- end -}}
{{- $digest := .image.digest | default "" -}}
{{- if $digest -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" $digest) -}}
{{- fail "first-party image digest must be a lowercase sha256 digest" -}}
{{- end -}}
{{- printf "%s@%s" $repo $digest -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Third-party image helper. Production registries must contain a mirror at the
declared mirrorRepository with the exact digest retained from upstream.
*/}}
{{- define "kognition.thirdPartyImage" -}}
{{- $repo := required "third-party image repository is required" .image.repository -}}
{{- $digest := required "third-party image digest is required" .image.digest -}}
{{- $registry := .global.thirdPartyImageRegistry | default "" -}}
{{- if $registry -}}
{{- $mirrorRepository := required "third-party mirrorRepository is required when global.thirdPartyImageRegistry is set" .image.mirrorRepository -}}
{{- $repo = printf "%s/%s" $registry $mirrorRepository -}}
{{- end -}}
{{- printf "%s@%s" $repo $digest -}}
{{- end }}

{{/*
OpenTelemetry environment variables for Kognition control-plane containers.
*/}}
{{- define "kognition.openTelemetryEnv" -}}
{{- $root := . -}}
{{- with .Values.observability.openTelemetry }}
{{- if .enabled }}
- name: KOGNITION_OTEL_TRACES_ENABLED
  value: "true"
- name: KOGNITION_OTEL_TRACES_ENDPOINT
  value: {{ required "observability.openTelemetry.endpoint is required when OpenTelemetry is enabled" .endpoint | quote }}
- name: KOGNITION_OTEL_TRACES_PROTOCOL
  value: {{ default "grpc" .protocol | quote }}
- name: KOGNITION_OTEL_TRACES_SAMPLE_RATE
  value: {{ default 1 .sampleRate | toString | quote }}
- name: KOGNITION_OTEL_TRACES_SERVICE_NAME
  value: {{ default "kognition" .serviceName | quote }}
- name: KOGNITION_OTEL_TRACES_SERVICE_VERSION
  value: {{ default $root.Chart.AppVersion .serviceVersion | quote }}
{{- with .environment }}
- name: KOGNITION_OTEL_TRACES_ENVIRONMENT
  value: {{ . | quote }}
{{- end }}
{{- with .headers }}
- name: KOGNITION_OTEL_TRACES_HEADERS
  value: {{ toJson . | quote }}
{{- end }}
{{- with .resourceAttributes }}
- name: KOGNITION_OTEL_TRACES_RESOURCE_ATTRIBUTES
  value: {{ toJson . | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
