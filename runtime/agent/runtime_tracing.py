# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""OpenTelemetry helpers for the Kognition Python agent runtime."""

from __future__ import annotations

import json
import logging
import os
from contextlib import contextmanager
from typing import Iterator, Mapping, Optional

from opentelemetry import context as otel_context
from opentelemetry import propagate, trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.trace.sampling import ParentBased, TraceIdRatioBased
from opentelemetry.trace import Span, SpanContext, SpanKind, Status, StatusCode, set_span_in_context, NonRecordingSpan

logger = logging.getLogger(__name__)

_TRACER_NAME = "kognition.runtime.agent"
_tracer = trace.get_tracer(_TRACER_NAME)
_provider: Optional[TracerProvider] = None


def configure_tracing(config) -> None:
    """Configure the runtime OTLP exporter from KOGNITION_OTEL_TRACES_* env vars."""
    global _provider, _tracer
    if _provider is not None:
        return
    if os.getenv("KOGNITION_OTEL_TRACES_ENABLED", "").lower() not in ("true", "1", "yes"):
        return

    endpoint = os.getenv("KOGNITION_OTEL_TRACES_ENDPOINT", "").strip()
    if not endpoint:
        logger.warning("OpenTelemetry enabled but KOGNITION_OTEL_TRACES_ENDPOINT is empty")
        return

    sample_rate = _parse_sample_rate(os.getenv("KOGNITION_OTEL_TRACES_SAMPLE_RATE", "1"))
    resource_attrs = {
        "service.name": os.getenv("KOGNITION_OTEL_TRACES_SERVICE_NAME", "kognition-agent-runtime"),
        "service.version": os.getenv("KOGNITION_OTEL_TRACES_SERVICE_VERSION", ""),
        "kognition.agent.name": config.agent_name,
        "kognition.instance.name": config.instance_name,
        "kognition.namespace": config.namespace,
    }
    environment = os.getenv("KOGNITION_OTEL_TRACES_ENVIRONMENT", "")
    if environment:
        resource_attrs["deployment.environment.name"] = environment
    resource_attrs.update(_json_string_map(os.getenv("KOGNITION_OTEL_TRACES_RESOURCE_ATTRIBUTES", "")))
    resource_attrs = {k: v for k, v in resource_attrs.items() if v}

    provider = TracerProvider(
        resource=Resource.create(resource_attrs),
        sampler=ParentBased(TraceIdRatioBased(sample_rate)),
    )
    headers = _json_string_map(os.getenv("KOGNITION_OTEL_TRACES_HEADERS", ""))
    exporter = _new_exporter(endpoint, headers)
    provider.add_span_processor(BatchSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    _provider = provider
    _tracer = provider.get_tracer(_TRACER_NAME)
    logger.info("OpenTelemetry tracing enabled for agent runtime: endpoint=%s", endpoint)


def shutdown_tracing() -> None:
    """Flush and shut down the runtime tracer provider if it was configured."""
    if _provider is not None:
        _provider.shutdown()


@contextmanager
def attach_traceparent(traceparent: str = "", tracestate: str = "") -> Iterator[None]:
    """Attach an incoming W3C trace context for child spans in this block."""
    carrier = {}
    if traceparent:
        carrier["traceparent"] = traceparent
    if tracestate:
        carrier["tracestate"] = tracestate
    if not carrier:
        yield
        return

    token = otel_context.attach(propagate.extract(carrier))
    try:
        yield
    finally:
        otel_context.detach(token)


@contextmanager
def attach_span_context(span_context: SpanContext) -> Iterator[None]:
    """Attach a known span context as the parent for child spans in this block."""
    token = otel_context.attach(set_span_in_context(NonRecordingSpan(span_context)))
    try:
        yield
    finally:
        otel_context.detach(token)


@contextmanager
def start_span(name: str, *, kind: SpanKind = SpanKind.INTERNAL, attributes: Optional[Mapping[str, object]] = None) -> Iterator[Span]:
    """Start a runtime span and mark it failed when an exception escapes."""
    with _tracer.start_as_current_span(name, kind=kind, attributes=dict(attributes or {})) as span:
        try:
            yield span
        except Exception as exc:
            record_exception(span, exc)
            raise


def record_exception(span: Span, exc: Exception) -> None:
    """Record an exception and mark the span status as error."""
    span.record_exception(exc)
    span.set_status(Status(StatusCode.ERROR, str(exc)))


def inject_trace_headers(headers: Optional[dict[str, str]] = None) -> dict[str, str]:
    """Inject the current trace context into outbound HTTP headers."""
    carrier = dict(headers or {})
    propagate.inject(carrier)
    return carrier


def _new_exporter(endpoint: str, headers: dict[str, str]):
    protocol = os.getenv("KOGNITION_OTEL_TRACES_PROTOCOL", "grpc").lower()
    if protocol in ("http", "http/protobuf"):
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter

        if ":4318" in endpoint and not endpoint.endswith("/v1/traces"):
            endpoint = endpoint.rstrip("/") + "/v1/traces"
        return OTLPSpanExporter(endpoint=endpoint, headers=headers or None)

    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter

    insecure = not endpoint.startswith("https://")
    if endpoint.startswith("http://") or endpoint.startswith("https://"):
        return OTLPSpanExporter(endpoint=endpoint, headers=headers or None, insecure=insecure)
    return OTLPSpanExporter(endpoint=endpoint, headers=headers or None, insecure=True)


def _json_string_map(raw: str) -> dict[str, str]:
    if not raw:
        return {}
    try:
        decoded = json.loads(raw)
    except json.JSONDecodeError:
        logger.warning("ignoring invalid JSON map in OpenTelemetry env var")
        return {}
    if not isinstance(decoded, dict):
        return {}
    return {str(k): str(v) for k, v in decoded.items() if v is not None}


def _parse_sample_rate(raw: str) -> float:
    try:
        value = float(raw)
    except ValueError:
        return 1.0
    if value < 0:
        return 0.0
    if value > 1:
        return 1.0
    return value