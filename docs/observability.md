# Observability

Status: OpenTelemetry trace export is opt-in.

Kognition keeps its semantic trace model in `internal/observability` and exports completed spans to OpenTelemetry through OTLP. Use an OpenTelemetry Collector as the stable target, then route traces from the collector to Tempo, Jaeger, Azure Monitor, or another backend.

## Prometheus Metrics

Enable `observability.metrics.serviceMonitor.enabled` to create Prometheus Operator scrape targets. Set `observability.metrics.serviceMonitor.additionalLabels` to match the Prometheus installation's ServiceMonitor selector. Application `namespace` labels identify Kognition workspaces; the chart uses `honorLabels: true` to preserve them.

### Standalone API Listener

The standalone API exports `/metrics` on a separate HTTP listener. `KOGNITION_METRICS_ADDR` defaults to `127.0.0.1:9090` for native processes. The public API listener has no metrics route, including when API authentication is disabled.

With ServiceMonitors enabled, Helm binds the metrics listener to `:9090` and creates a dedicated internal ClusterIP Service. The public API Service and Ingress do not expose this port. API TLS settings apply only to the API listener; metrics uses HTTP.

Metrics has no application authentication. Enable `networkPolicy.enabled` with a NetworkPolicy-capable CNI to restrict port 9090 to pods labeled `app.kubernetes.io/name: prometheus`. This selector permits matching pods in any namespace, so control who can create or label those pods. Without NetworkPolicy enforcement, the metrics Service is reachable throughout the cluster. Keep native bindings on loopback or protect remote scraping with a firewall or authenticated proxy.

### Retained Workflow Projections

Controllers reconstruct workflow metrics from retained WorkflowRuns and owned AgentJobs on every scrape. This recovers state after restarts and committed writes whose readback failed.

| Gauge | Meaning |
| --- | --- |
| `kognition_workflow_active_runs` | Started, nonterminal runs excluding runs being deleted, grouped by namespace and workflow. |
| `kognition_workflow_retained_runs` | Retained terminal runs grouped by namespace, workflow, and phase. |
| `kognition_workflow_retained_step_duration_seconds_sum` | Total duration of retained terminal steps with valid start and owned-job completion evidence. |
| `kognition_workflow_retained_step_duration_observations` | Number of steps contributing to the duration sum. |

Durations use matching WorkflowRun owner name, UID, and namespace. Missing or negative timestamp evidence contributes no duration sample. Store read failures fail the scrape instead of publishing zero-valued populations.

Retention can decrease these gauges. Use direct values and duration sum divided by observation count; avoid `rate`, `increase`, and histogram quantiles. The Workflow 360 dashboard shows retained populations and weighted means, deduplicating controller replicas with `max by` on the semantic labels before aggregation. Its selected time range displays historical snapshots of the retained population. No duration value means evidence is unavailable. The former `kognition_workflow_runs_total` and `kognition_workflow_step_duration_seconds_*` event metrics are removed.

### Model Call Metrics And Passive Health

Every component that calls a model provider through a CPI driver (the API server, the controllers and the Model Gateway) counts and times those calls.

| Metric | Meaning |
| --- | --- |
| `kognition_cpi_calls_total{provider,operation,outcome,reason}` | Completed model calls. `operation` is `complete` or `embed`. `outcome` is `success`, `provider_failure` (the provider or its configuration is at fault: rejected credentials, exhausted quota, rate limiting, 5xx or 404, timeouts, unreachable endpoint) or `request_failure` (invalid request, cancellation or an undecodable response). `reason` is `none` on success. |
| `kognition_cpi_call_duration_seconds{provider,operation,outcome}` | Duration histogram of the same calls. |

The model is deliberately not a label, because Model resources are user defined. A provider error rate is `provider_failure` divided by all calls.

In standalone mode the API server and the controller manager also publish a short summary of their recent calls to PostgreSQL, and the Model controller uses it to degrade a Model whose real traffic is failing even though its active probe still passes. Each reporter publishes the evidence under two kinds of opaque keys, hashes that never contain the endpoint or the secret:

- **Destination:** the provider, endpoint and model ID. Failures that concern everyone who calls the destination, such as timeouts, an unreachable endpoint, 5xx, 404 or rate limiting, count here, so Models that share a destination share them.
- **Credential:** the destination together with a fingerprint of the secret the call presented. Rejected credentials and exhausted quota or billing count only here, so one account's revoked key or empty balance degrades the Models that use that secret and not those of another account that calls the same destination. A success counts for both.

The Model controller derives a Model's credential from its `credentialRef` the way the callers present it, so a Model's evidence is the same whichever component made the calls. A Model whose secret the controller cannot resolve, such as an environment variable that only the calling process has, is never blamed for credential failures; it is left to its active probe and to the destination's evidence. The fingerprint is a one-way hash of the secret material combined with the destination, which is as sensitive as the secret's entropy: use high-entropy API keys.

A Model is degraded when, within the last five minutes, its destination or its credential has three consecutive provider failures (two for rejected credentials or exhausted quota) or at least half of at least five calls failed while the latest call failed. Replicas publish when each run began, and the evidence of all of them is merged so that a run only counts for the part that no other replica's opposite outcome interrupted: one replica's successes around another's failure never add up to a recovery, and failures from before another replica's success never degrade a Model. When the order of outcomes is not certain, a run counts as one call. The Model reports `Ready=False` with the failure reason (for example `RateLimited`), a message starting with `passive health:`, `status.available=false` and phase `Failed`. A degraded Model recovers after three consecutive successes or once the failures leave the five minute window. An active probe failure takes precedence over a passive one, and an unavailable health store keeps the previous verdict.

The controller manager's health watcher queues a Model when its status no longer matches the evidence. It does not queue a Model that is unready for another reason, such as a failing probe, because reconciling it would keep that failure and only repeat the probe, and it queues a Model for the same outcome at most once a minute. A recovery or a change of reason is queued at once. Outcomes of calls that finish while the API server or the controller manager shuts down are still published, because the reporter stops only after requests and controllers have drained.

Passive health reports and drives fallback selection; it does not block calls to a degraded Model. It is not shared in Kubernetes mode yet, so Models there keep reporting only their active probe.

## Exported Spans

When tracing is enabled, Kognition exports spans for workflow orchestration and tool invocation.

| Span name | Producer | Coverage |
| --- | --- | --- |
| `kognition.workflow.run` | WorkflowRun reconciler | One root span for each WorkflowRun reconcile attempt. The span records workflow name, run name, namespace, status, completed step count, and active step count. |
| `kognition.workflow.step` | Workflow reconciler | Tool step execution and AgentJob dispatch. Cyclic workflow steps include the iteration attribute. |
| `kognition.tool.call` | Invocation tracer | Synchronous tool calls, including tool name, digest, grant reference, sandbox tier, latency, result status, and cost units. |

Workflow step spans inherit the current WorkflowRun trace context. AgentJob annotations continue to point at the WorkflowRun root span so downstream AgentJob execution can join the workflow trace.

## Data Redaction

The Go semantic OTLP trace pipeline installs the default redactor before export.
Redaction applies to string values and structured values for these attributes:

| Attribute | Content |
| --- | --- |
| `kognition.tool.args` | Tool arguments. |
| `kognition.tool.result` | Tool result payloads. |
| `kognition.memory.value` | Memory values. |
| `kognition.reasoning.content` | Reasoning content. |

Structured values are serialized before redaction. Matching email addresses,
SSNs, credit card numbers, phone numbers, IP addresses and configured credential
patterns are replaced in these fields. Pattern matching does not guarantee
removal of every sensitive value.

This Go redactor does not cover every application log, SDK callback or Python
exception event. The Python agent can log tool arguments at INFO and include
exception text in enabled tracing. Disabling OTLP export does not disable those
application logs or functional requests to model, search and memory providers.
Treat logs and traces as potentially sensitive; apply access, minimization,
retention and deletion controls for the actual deployment. Verify SDK/collector
behavior and retained data before making privacy claims.

## Configuration

Tracing is disabled unless `KOGNITION_OTEL_TRACES_ENABLED=true` is set.

| Environment variable | Description |
| --- | --- |
| `KOGNITION_OTEL_TRACES_ENABLED` | Enables OTLP trace export when set to `true`. |
| `KOGNITION_OTEL_TRACES_ENDPOINT` | Collector endpoint. Required when tracing is enabled. |
| `KOGNITION_OTEL_TRACES_PROTOCOL` | `grpc` or `http/protobuf`. Defaults to `grpc`. |
| `KOGNITION_OTEL_TRACES_SAMPLE_RATE` | Sampling rate from `0.0` to `1.0`. Defaults to `1.0` when unset. An explicit `0.0` is preserved and produces no sampled spans. |
| `KOGNITION_OTEL_TRACES_SERVICE_NAME` | OTel service name. Defaults to `kognition`. |
| `KOGNITION_OTEL_TRACES_SERVICE_VERSION` | Optional service version resource attribute. |
| `KOGNITION_OTEL_TRACES_ENVIRONMENT` | Optional deployment environment resource attribute. |
| `KOGNITION_OTEL_TRACES_HEADERS` | Optional JSON object of exporter headers. |
| `KOGNITION_OTEL_TRACES_RESOURCE_ATTRIBUTES` | Optional JSON object of additional resource attributes. |

## Kubernetes And Helm

The Helm chart renders the same variables for standalone mode workloads and Kubernetes mode workloads.

```yaml
observability:
  openTelemetry:
    enabled: true
    endpoint: otel-collector.observability.svc.cluster.local:4317
    protocol: grpc
    sampleRate: 1.0
    serviceName: kognition
    serviceVersion: ""
    environment: production
    headers: {}
    resourceAttributes:
      k8s.cluster.name: prod-east
```

For HTTP/protobuf collectors, use the traces path in the endpoint.

```yaml
observability:
  openTelemetry:
    enabled: true
    endpoint: http://otel-collector.observability.svc.cluster.local:4318/v1/traces
    protocol: http/protobuf
```

## Native Processes

Native binaries use the same environment contract.

```powershell
$env:KOGNITION_OTEL_TRACES_ENABLED = "true"
$env:KOGNITION_OTEL_TRACES_ENDPOINT = "localhost:4317"
$env:KOGNITION_OTEL_TRACES_PROTOCOL = "grpc"
$env:KOGNITION_OTEL_TRACES_SERVICE_NAME = "kognition-dev"
.\bin\kognition-controller-manager.exe
```

**Bash (Linux/macOS):**

```bash
export KOGNITION_OTEL_TRACES_ENABLED=true
export KOGNITION_OTEL_TRACES_ENDPOINT=localhost:4317
export KOGNITION_OTEL_TRACES_PROTOCOL=grpc
export KOGNITION_OTEL_TRACES_SERVICE_NAME=kognition-dev
./bin/kognition-controller-manager
```

For HTTP/protobuf:

```powershell
$env:KOGNITION_OTEL_TRACES_ENABLED = "true"
$env:KOGNITION_OTEL_TRACES_ENDPOINT = "http://localhost:4318/v1/traces"
$env:KOGNITION_OTEL_TRACES_PROTOCOL = "http/protobuf"
.\bin\kognition-apiserver.exe
```

**Bash (Linux/macOS):**

```bash
export KOGNITION_OTEL_TRACES_ENABLED=true
export KOGNITION_OTEL_TRACES_ENDPOINT=http://localhost:4318/v1/traces
export KOGNITION_OTEL_TRACES_PROTOCOL=http/protobuf
./bin/kognition-apiserver
```

## Local Jaeger Verification

For local Kubernetes verification, install a disposable Jaeger all-in-one instance and point the current Helm release at its OTLP gRPC endpoint.

```bash
make jaeger
make jaeger-ui
```

On Windows, use the PowerShell shim:

```powershell
.\make.ps1 jaeger
.\make.ps1 jaeger-ui
```

Then open `http://localhost:16686` and select the `kognition` service after running a workflow that invokes a tool.

The helper installs Jaeger in `kognition-observability` by default and configures the existing Kognition Helm release with:

```yaml
observability:
  openTelemetry:
    enabled: true
    endpoint: jaeger-collector.kognition-observability.svc.cluster.local:4317
    protocol: grpc
    environment: dev
```

Remove the local backend with:

```bash
make jaeger-uninstall
```

**PowerShell (Windows):**

```powershell
.\make.ps1 jaeger-uninstall
```

## Collector Target

A minimal collector must expose an OTLP receiver for the protocol you configure.

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch: {}

exporters:
  debug: {}

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [debug]
```

## Trace Propagation

Kubernetes resources can project trace context through annotations:

| Annotation | Description |
| --- | --- |
| `kognition.dev/trace-id` | Current trace identifier. |
| `kognition.dev/root-span-id` | Root workflow span identifier. |
| `kognition.dev/span-id` | Current span identifier. |
| `kognition.dev/parent-span-id` | Parent span identifier. |
| `kognition.dev/traceparent` | W3C traceparent value. |

Native runtime code uses `ContextWithSpanContext` and `SpanContextFromContext` to carry the same identifiers without depending on Kubernetes annotations.

Invocation audit entries use the trace ID and span ID from the actual `kognition.tool.call` span. If a caller provides parent trace context, the audit entry follows that parented span instead of the tracer instance default trace.