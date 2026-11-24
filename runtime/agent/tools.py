# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Tool registry and executors for the agent runtime.

This module turns the Tool specs injected by the dispatcher (via
``KOGNITION_TOOLS_RESOLVED``) into LangChain ``StructuredTool`` instances
that the LLM can actually call.

Every LLM-invokable capability is a Tool CR with an executable endpoint.
The Tool CR's ``protocol`` field selects the executor:

* ``OpenAPI`` / ``http`` — POSTs the LLM's tool-call arguments as a JSON body
  to ``tool.endpoint`` and returns the response body (JSON re-serialized, or
  plain text). Response size is capped at 1 MiB.
* ``MCP`` — POSTs a JSON-RPC 2.0 ``tools/call`` request to ``tool.endpoint``
  and extracts ``result.content[0].text`` (falling back to serializing the
  whole ``result`` object).

Tools with any other protocol (or no protocol at all) are still registered
so the LLM sees the tool in its schema, but calling them returns a stub
error rather than executing anything.
"""

from __future__ import annotations

import json
import logging
import re
import os
import uuid
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass
from hashlib import sha256
from typing import Any, Callable
from urllib.parse import urlparse

import httpx
from langchain_core.tools import StructuredTool
from pydantic import BaseModel, Field, create_model

import runtime_tracing
from jsonschema import validators
from pydantic import model_validator

logger = logging.getLogger(__name__)


# --------------------------------------------------------------------------- #
# Argument schema construction
# --------------------------------------------------------------------------- #

_JSON_TYPE_TO_PY: dict[str, type] = {
    "string": str,
    "integer": int,
    "number": float,
    "boolean": bool,
    "array": list,
    "object": dict,
}


def _args_model_from_schema(tool_name: str, schema: dict[str, Any], preserve_empty: bool = False) -> type[BaseModel]:
    """Build a Pydantic model from a JSON schema's ``properties`` block.

    Falls back to a single-field ``input: str`` model when the schema is
    empty or unparseable so the LLM still has a contract to fill in."""
    if preserve_empty:
        return _reviewed_args_model_from_schema(tool_name, schema)
    props = (schema or {}).get("properties") or {}
    required = set((schema or {}).get("required") or [])
    fields: dict[str, tuple[type, Any]] = {}
    for name, spec in props.items():
        if not isinstance(spec, dict):
            continue
        variants = spec.get("anyOf") or []
        nullable = any(isinstance(variant, dict) and variant.get("type") == "null" for variant in variants)
        concrete = next((variant for variant in variants if isinstance(variant, dict) and variant.get("type") not in (None, "null")), spec)
        py_type = _JSON_TYPE_TO_PY.get(str(concrete.get("type", "string")), str)
        if nullable:
            py_type = py_type | None
        default = spec.get("default", ... if name in required else None)
        description = spec.get("description", "")
        fields[name] = (py_type, Field(default=default, description=description))
    if not fields and not preserve_empty:
        fields["input"] = (str, Field(default="", description="Free-form input"))
    return create_model(f"{tool_name}_Args", **fields)  # type: ignore[call-overload]


def _reviewed_args_model_from_schema(tool_name: str, schema: dict[str, Any]) -> type[BaseModel]:
    reviewed_schema = json.loads(json.dumps(schema))
    pending = [reviewed_schema]
    while pending:
        node = pending.pop()
        if isinstance(node, dict):
            for reference in ("$ref", "$dynamicRef", "$recursiveRef"):
                if reference in node and (not isinstance(node[reference], str) or not node[reference].startswith("#")):
                    raise ValueError("imported tool schema requires local references")
            pending.extend(node.values())
        elif isinstance(node, list):
            pending.extend(node)
    validator_type = validators.validator_for(reviewed_schema)
    validator_type.check_schema(reviewed_schema)
    validator = validator_type(reviewed_schema)

    class ReviewedArgs(BaseModel):
        @classmethod
        def model_json_schema(cls, *args: Any, **kwargs: Any) -> dict[str, Any]:
            return json.loads(json.dumps(reviewed_schema))

        @model_validator(mode="before")
        @classmethod
        def validate_reviewed_arguments(cls, value: Any) -> Any:
            if not validator.is_valid(value):
                raise ValueError("arguments do not match reviewed imported tool schema")
            return value

    required = set(reviewed_schema.get("required") or [])
    fields = {name: (Any, Field(default=spec.get("default", ... if name in required else None))) for name, spec in (reviewed_schema.get("properties") or {}).items()}
    return create_model(f"{tool_name}_Args", __base__=ReviewedArgs, **fields)


@dataclass
class ToolDef:
    """Resolved tool definition received from the dispatcher."""

    name: str
    resource_version: int = 0
    description: str = ""
    protocol: str = ""
    endpoint: str = ""
    input_schema: dict[str, Any] | None = None
    class_name: str = ""
    runtime_name: str = ""

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> "ToolDef":
        return cls(
            name=str(raw.get("name", "")).strip(),
            resource_version=int(raw.get("resourceVersion", 0) or 0),
            description=str(raw.get("description", "")),
            protocol=str(raw.get("protocol", "")).lower(),
            endpoint=str(raw.get("endpoint", "")),
            input_schema=raw.get("inputSchema"),
            class_name=str(raw.get("className", "")),
            runtime_name=str(raw.get("runtimeName", "")),
        )


# --------------------------------------------------------------------------- #
# Executors
# --------------------------------------------------------------------------- #

_MAX_RESPONSE_BYTES = 1 * 1024 * 1024  # 1 MiB
_HTTP_TIMEOUT_SECONDS = 30.0
_tool_session_capability: ContextVar[str] = ContextVar("tool_session_capability", default="")
_agent_delegation_session_capability: ContextVar[str] = ContextVar("agent_delegation_session_capability", default="")
_agent_delegation_invocations: ContextVar[dict[str, tuple[bytes, str]] | None] = ContextVar(
    "agent_delegation_invocations", default=None
)
_knowledge_session_capability: ContextVar[str] = ContextVar("knowledge_session_capability", default="")


@contextmanager
def tool_runtime_session_capability(capability: str):
    token = _tool_session_capability.set(capability)
    try:
        yield
    finally:
        _tool_session_capability.reset(token)


@contextmanager
def agent_delegation_runtime_session_capability(capability: str):
    invocations = None
    if capability != _agent_delegation_session_capability.get() or _agent_delegation_invocations.get() is None:
        invocations = _agent_delegation_invocations.set({})
    token = _agent_delegation_session_capability.set(capability)
    try:
        yield
    finally:
        _agent_delegation_session_capability.reset(token)
        if invocations is not None:
            _agent_delegation_invocations.reset(invocations)


@contextmanager
def knowledge_runtime_session_capability(capability: str):
    token = _knowledge_session_capability.set(capability)
    try:
        yield
    finally:
        _knowledge_session_capability.reset(token)


def _truncate(text: str) -> str:
    if len(text) <= _MAX_RESPONSE_BYTES:
        return text
    return text[:_MAX_RESPONSE_BYTES] + f"\n... [truncated, response exceeded {_MAX_RESPONSE_BYTES} bytes]"


def _format_http_body(resp: httpx.Response) -> str:
    ctype = (resp.headers.get("content-type") or "").lower()
    if "application/json" in ctype:
        try:
            return _truncate(json.dumps(resp.json()))
        except ValueError:
            pass
    return _truncate(resp.text)


def _make_http_executor(tool: ToolDef) -> Callable[..., str]:
    """Executor for ``OpenAPI`` / ``http`` protocol tools.

    POSTs the LLM's arguments as JSON to ``tool.endpoint`` and returns the
    response body. HTTP errors and network failures surface as plain error
    strings so the LLM can retry or apologize."""

    endpoint = tool.endpoint

    def http_executor(**kwargs: Any) -> str:
        if not endpoint:
            return f"Tool '{tool.name}' has no endpoint configured."
        try:
            resp = httpx.post(endpoint, json=kwargs, timeout=_HTTP_TIMEOUT_SECONDS, headers=runtime_tracing.inject_trace_headers())
        except httpx.HTTPError as exc:
            logger.warning("tool '%s' HTTP call failed: %s", tool.name, exc)
            return f"Tool '{tool.name}' HTTP call failed: {exc}"
        if resp.status_code >= 400:
            return (
                f"Tool '{tool.name}' returned HTTP {resp.status_code}: "
                f"{_truncate(resp.text)}"
            )
        return _format_http_body(resp)

    return http_executor


def _make_mcp_executor(tool: ToolDef) -> Callable[..., str]:
    """Executor for ``MCP`` protocol tools.

    Speaks JSON-RPC 2.0 ``tools/call`` against ``tool.endpoint`` using the
    tool's canonical (snake_case) name. Extracts ``result.content[0].text``
    when present, otherwise serializes ``result``."""

    endpoint = tool.endpoint
    canonical = _canonical_name(tool.name)

    def mcp_executor(**kwargs: Any) -> str:
        if not endpoint:
            return f"Tool '{tool.name}' has no endpoint configured."
        payload = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": {"name": canonical, "arguments": kwargs},
        }
        try:
            resp = httpx.post(endpoint, json=payload, timeout=_HTTP_TIMEOUT_SECONDS, headers=runtime_tracing.inject_trace_headers())
        except httpx.HTTPError as exc:
            logger.warning("tool '%s' MCP call failed: %s", tool.name, exc)
            return f"Tool '{tool.name}' MCP call failed: {exc}"
        if resp.status_code >= 400:
            return (
                f"Tool '{tool.name}' MCP endpoint returned HTTP "
                f"{resp.status_code}: {_truncate(resp.text)}"
            )
        try:
            body = resp.json()
        except ValueError:
            return _truncate(resp.text)
        if isinstance(body, dict) and "error" in body:
            return f"Tool '{tool.name}' MCP error: {_truncate(json.dumps(body['error']))}"
        result = body.get("result") if isinstance(body, dict) else None
        if isinstance(result, dict):
            content = result.get("content")
            if isinstance(content, list) and content:
                first = content[0]
                if isinstance(first, dict) and isinstance(first.get("text"), str):
                    return _truncate(first["text"])
            return _truncate(json.dumps(result))
        return _truncate(json.dumps(body))

    return mcp_executor


def _make_stub_executor(tool: ToolDef) -> Callable[..., str]:
    """Executor for tools with an unrecognized (or empty) protocol.

    The tool schema is still exposed to the LLM so the model can decide
    whether it's relevant; calling it just returns an error string."""

    def stub(**kwargs: Any) -> str:
        logger.info(
            "tool '%s' (protocol=%r) invoked with %s — no executor wired",
            tool.name, tool.protocol, kwargs,
        )
        return (
            f"Tool '{tool.name}' has protocol '{tool.protocol}' which is "
            "not runnable in this runtime image. The agent should explain "
            "to the user that this tool is not yet wired."
        )

    return stub


def _make_executor(tool: ToolDef) -> Callable[..., str]:
    protocol = tool.protocol.lower()
    if protocol in ("openapi", "http"):
        return _make_http_executor(tool)
    if protocol == "mcp":
        return _make_mcp_executor(tool)
    return _make_stub_executor(tool)


def _make_gateway_executor(tool: ToolDef, gateway_url: str) -> Callable[..., str]:
    """Exchange the current session capability and invoke the selected Tool."""

    base_url = gateway_url.rstrip("/")
    pending_invocations: dict[tuple[str, str], tuple[str, dict[str, Any]]] = {}

    def gateway_executor(**kwargs: Any) -> str:
        session_capability = _tool_session_capability.get()
        if not session_capability:
            logger.warning("Tool Gateway session capability is missing for tool '%s'", tool.name)
            return f"Tool '{tool.name}' cannot run because its session capability is missing."
        input_key = json.dumps(kwargs, sort_keys=True, separators=(",", ":"))
        pending_key = (session_capability, input_key)
        pending = pending_invocations.get(pending_key)
        if pending is None:
            invocation_id = str(uuid.uuid4())
            invocation_input = json.loads(json.dumps(kwargs))
        else:
            invocation_id, invocation_input = pending
        headers = {"Authorization": f"Bearer {session_capability}", **runtime_tracing.inject_trace_headers()}
        try:
            exchange = httpx.post(
                f"{base_url}/v1/tools/capabilities",
                json={"tool_name": tool.name, "invocation_id": invocation_id, "input": invocation_input},
                timeout=_HTTP_TIMEOUT_SECONDS,
                headers=headers,
            )
            exchange_body = exchange.json()
            exchange_state = str(exchange_body.get("runtime_approval_state", "")).lower()
            if exchange_state == "pending":
                returned_id = exchange_body.get("invocation_id")
                if returned_id not in (None, invocation_id):
                    return f"Tool '{tool.name}' capability exchange returned a mismatched invocation ID."
                pending_invocations[pending_key] = (invocation_id, invocation_input)
                return _truncate(json.dumps(exchange_body))
            if exchange_state in ("terminal", "unavailable"):
                pending_invocations.pop(pending_key, None)
                return _truncate(json.dumps(exchange_body))
            exchange.raise_for_status()
            capability = exchange_body.get("capability", "")
            if not isinstance(capability, str) or not capability:
                return f"Tool '{tool.name}' capability exchange returned no capability."
            pending_invocations[pending_key] = (invocation_id, invocation_input)
            response = httpx.post(
                f"{base_url}/v1/tools/invoke",
                json={"tool_name": tool.name, "tool_resource_version": _tool_resource_version(tool), "invocation_id": invocation_id, "input": invocation_input},
                timeout=_HTTP_TIMEOUT_SECONDS,
                headers={"Authorization": f"Bearer {capability}", **runtime_tracing.inject_trace_headers()},
            )
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("Tool Gateway call for '%s' failed: %s", tool.name, exc)
            return f"Tool '{tool.name}' gateway call failed: {exc}"
        try:
            response_body = response.json()
        except ValueError:
            response_body = None
        response_state = str(response_body.get("runtime_approval_state", "")).lower() if isinstance(response_body, dict) else ""
        if response_state in ("pending", "unavailable"):
            returned_id = response_body.get("invocation_id") if isinstance(response_body, dict) else None
            if returned_id not in (None, invocation_id):
                return f"Tool '{tool.name}' gateway returned a mismatched invocation ID."
            pending_invocations[pending_key] = (invocation_id, invocation_input)
            return _truncate(json.dumps(response_body))
        if response_state != "unavailable":
            pending_invocations.pop(pending_key, None)
        if response.status_code >= 400:
            return f"Tool '{tool.name}' gateway returned HTTP {response.status_code}: {_truncate(response.text)}"
        if not isinstance(response_body, dict):
            return _truncate(response.text)
        output = response_body.get("output")
        if tool.runtime_name and isinstance(output, dict) and set(output) == {"result"} and not isinstance(output["result"], dict):
            output = output["result"]
            if isinstance(output, str):
                return _truncate(output)
        return _truncate(json.dumps(output)) if output is not None else "{}"

    return gateway_executor


def build_agent_delegation_tool(gateway_url: str) -> StructuredTool | None:
    """Build the capability-bound AgentService delegation tool when enabled."""

    if not gateway_url:
        return None
    base_url = gateway_url.rstrip("/")

    def delegate_to_agent(target_namespace: str, target_agent_service: str, message: str, invocation_id: str = "") -> str:
        session_capability = _agent_delegation_session_capability.get()
        invocations = _agent_delegation_invocations.get()
        if not session_capability or invocations is None:
            return "Agent delegation cannot run because its session capability is missing."
        fingerprint = sha256(json.dumps([target_namespace, target_agent_service, message]).encode()).digest()
        capability = ""
        if invocation_id:
            previous = invocations.get(invocation_id)
            if previous is None or previous[0] != fingerprint:
                return "Agent delegation continuation does not match an outstanding request in this session."
            capability = previous[1]
        else:
            if len(invocations) >= 128:
                return "Too many outstanding agent delegations in this session."
            invocation_id = str(uuid.uuid4())
            invocations[invocation_id] = (fingerprint, "")
        body = {"target_namespace": target_namespace, "target_agent_service": target_agent_service, "invocation_id": invocation_id, "message": message}
        headers = {"Authorization": f"Bearer {session_capability}", **runtime_tracing.inject_trace_headers()}
        try:
            if not capability:
                exchange = httpx.post(
                    f"{base_url}/v1/agent-services/capabilities", json=body, timeout=_HTTP_TIMEOUT_SECONDS, headers=headers,
                )
                exchange_body = exchange.json()
                if isinstance(exchange_body, dict):
                    approval_state = str(exchange_body.get("runtime_approval_state", "")).lower()
                    if approval_state in ("pending", "terminal", "unavailable"):
                        if approval_state == "terminal":
                            invocations.pop(invocation_id, None)
                        return _truncate(json.dumps({**exchange_body, "invocation_id": invocation_id}))
                exchange.raise_for_status()
                capability = exchange_body.get("capability", "") if isinstance(exchange_body, dict) else ""
                if not isinstance(capability, str) or not capability:
                    raise ValueError("Agent delegation capability exchange returned no capability.")
                invocations[invocation_id] = (fingerprint, capability)
            response = httpx.post(
                f"{base_url}/v1/agent-services/invoke",
                json=body,
                timeout=_HTTP_TIMEOUT_SECONDS,
                headers={"Authorization": f"Bearer {capability}", **runtime_tracing.inject_trace_headers()},
            )
        except (httpx.HTTPError, ValueError) as exc:
            logger.warning("Agent delegation gateway call failed: %s", exc)
            if isinstance(exc, httpx.HTTPStatusError) and 400 <= exc.response.status_code < 500 and exc.response.status_code not in (408, 429):
                invocations.pop(invocation_id, None)
                return f"Agent delegation capability exchange returned HTTP {exc.response.status_code}."
            return json.dumps({"invocation_id": invocation_id, "error": "Agent delegation outcome unavailable. Continue the same invocation, not a new one."})
        try:
            response_body = response.json()
        except ValueError:
            response_body = None
        if isinstance(response_body, dict):
            approval_state = str(response_body.get("runtime_approval_state", "")).lower()
            if approval_state in ("pending", "terminal", "unavailable"):
                if approval_state == "terminal":
                    invocations.pop(invocation_id, None)
                return _truncate(json.dumps({**response_body, "invocation_id": invocation_id}))
        if response.status_code in (408, 409, 429) or response.status_code >= 500:
            return json.dumps({"invocation_id": invocation_id, "error": "Agent delegation outcome unavailable. Continue the same invocation, not a new one."})
        invocations.pop(invocation_id, None)
        if response.status_code >= 400:
            return f"Agent delegation gateway returned HTTP {response.status_code}: {_truncate(response.text)}"
        return _truncate(json.dumps(response_body)) if response_body is not None else _truncate(response.text)

    args_model = _args_model_from_schema(
        "delegate_to_agent",
        {
            "type": "object",
            "properties": {
                "target_namespace": {"type": "string"},
                "target_agent_service": {"type": "string"},
                "message": {"type": "string"},
                "invocation_id": {"type": "string", "default": "", "description": "Continue an outstanding delegation using its returned invocation ID and unchanged arguments. Omit for a new action."},
            },
            "required": ["target_namespace", "target_agent_service", "message"],
        },
    )
    return StructuredTool.from_function(
        func=delegate_to_agent,
        name="delegate_to_agent",
        description="Delegate a message to an authorized AgentService. Continue a pending or uncertain delegation with its invocation_id rather than starting it again.",
        args_schema=args_model,
    )


# --------------------------------------------------------------------------- #
# Public API
# --------------------------------------------------------------------------- #


def build_tools(resolved_json: str, fallback_json: str, tool_gateway_url: str = "") -> list[StructuredTool]:
    """Construct the agent's tool list from dispatcher-provided JSON.

    ``resolved_json`` is the preferred input. It includes endpoint metadata
    only for direct execution; with a configured Tool Gateway the runtime uses
    the schema and resource version while the gateway retains the target URL.
    ``fallback_json`` is the legacy ``[]AgentToolRef`` shape used when the
    dispatcher could not resolve a Tool CR; those get a stub executor
    because we don't know the endpoint.
    """
    raw_resolved = _safe_load(resolved_json)
    raw_fallback = _safe_load(fallback_json)

    defs: list[ToolDef] = []
    seen: set[str] = set()
    for raw in raw_resolved:
        if not isinstance(raw, dict):
            continue
        td = ToolDef.from_dict(raw)
        if not td.name or td.name in seen:
            continue
        defs.append(td)
        seen.add(td.name)

    for raw in raw_fallback:
        if not isinstance(raw, dict):
            continue
        ref = raw.get("ref") or {}
        name = str(ref.get("name") or raw.get("name") or "").strip()
        if not name or name in seen:
            continue
        # No endpoint / protocol known — stub executor will explain the gap.
        defs.append(ToolDef(name=name))
        seen.add(name)

    tools: list[StructuredTool] = []
    runtime_names: set[str] = set()
    for td in defs:
        canonical = td.runtime_name or _canonical_name(td.name)
        if td.runtime_name and not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]{0,63}", canonical):
            raise ValueError("Invalid reviewed runtime tool name")
        if canonical in runtime_names:
            raise ValueError("Duplicate runtime tool name")
        runtime_names.add(canonical)
        executor = _make_gateway_executor(td, tool_gateway_url) if tool_gateway_url else _make_executor(td)
        schema = td.input_schema or {}
        args_model = _args_model_from_schema(canonical, schema, preserve_empty=bool(td.runtime_name))
        description = td.description or f"Tool {canonical}"
        registered_tool = StructuredTool.from_function(
            func=executor,
            name=canonical,
            description=description,
            args_schema=args_model,
        )
        if td.runtime_name:
            registered_tool.description = description
        tools.append(registered_tool)

    if tools:
        logger.info(
            "built %d tools: %s",
            len(tools),
            ", ".join(f"{t.name}({_protocol_for(t, defs)})" for t in tools),
        )
    return tools


def _tool_resource_version(tool: ToolDef) -> int:
    return tool.resource_version


def _protocol_for(tool: StructuredTool, defs: list[ToolDef]) -> str:
    for d in defs:
        if _canonical_name(d.name) == tool.name:
            return d.protocol or "stub"
    return "stub"


def _safe_load(payload: str) -> list[Any]:
    if not payload or payload == "[]":
        return []
    try:
        data = json.loads(payload)
        if isinstance(data, list):
            return data
    except json.JSONDecodeError as exc:
        logger.warning("failed to parse tools JSON: %s", exc)
    return []


def _canonical_name(name: str) -> str:
    # K8s resource names disallow underscores; LLM tool names use them.
    # Map kebab-case Tool CR names to the snake_case identifier the LLM sees.
    return name.replace("-", "_") if name else name


# --------------------------------------------------------------------------- #
# Knowledge-base search
# --------------------------------------------------------------------------- #
#
# The dispatcher injects the agent's KnowledgeBases as JSON in the
# ``KOGNITION_KB_RESOLVED`` env var. Each record has the shape::
#
#     {
#       "name": "kb-a",
#       "className": "docs",
#       "sources": [
#         {"type": "inline", "data": "..."},
#         {"type": "url",    "uri":  "https://..."},
#         {"type": "blob",   "provider": "azure|s3", ...}
#       ],
#       "vectorStore": {           # optional; present when the class binds a vector store
#         "provider": "pgvector",
#         "dsn":      "postgres://...",
#         "table":    "kognition_kb_chunks",
#         "dimension": 1536
#       },
#       "embedding": {              # optional; required when vectorStore is set
#         "provider":  "openai|azure_openai",
#         "model":     "text-embedding-3-small",
#         "baseUrl":   "https://api.openai.com/v1",
#         "apiKey":    "sk-...",
#         "apiVersion":"2024-06-01"
#       },
#       "retrieval": {"topK": 5, "minScore": 0.0, "filter": "..."}   # optional
#     }
#
# ``_flatten_kb_chunks`` + ``_build_knowledge_search`` handle the legacy
# inline / keyword-search path (used when the KB has no ``vectorStore``).
# ``_build_kb_search_tool`` composes those with pgvector-backed retrieval
# and returns a single ``knowledge_search`` StructuredTool for the agent.

import re  # noqa: E402 - placed near KB helpers to keep the top clean

_KB_NO_KB_MESSAGE = "No knowledge bases are attached to this agent."
_KB_TOKEN_MIN_LEN = 3
_KB_INLINE_SPLIT_RE = re.compile(r"\n\s*\n")
_KB_WORD_RE = re.compile(r"\w+")


def _flatten_kb_chunks(records: Any) -> list[dict[str, str]]:
    """Flatten resolved KB records into an in-memory chunk list.

    The runtime keeps its keyword-search fallback deliberately tiny: only
    ``inline`` sources contribute real content; other source types are
    passed through as a single ``"not ingested"`` stub so the LLM knows the
    source exists but its content isn't reachable from this pod. Records
    with a bound vector store are handled elsewhere.
    """
    chunks: list[dict[str, str]] = []
    if not isinstance(records, list):
        return chunks

    for rec in records:
        if not isinstance(rec, dict):
            continue
        kb_name = str(rec.get("name") or "").strip()
        if not kb_name:
            continue
        sources = rec.get("sources") or []
        if not isinstance(sources, list):
            continue
        for src in sources:
            if not isinstance(src, dict):
                continue
            raw_type = str(src.get("type") or "").strip().lower()
            source_type = raw_type or "unknown"
            if raw_type == "inline":
                data = str(src.get("data") or "").strip()
                if not data:
                    continue
                paragraphs = [
                    p.strip() for p in _KB_INLINE_SPLIT_RE.split(data) if p.strip()
                ]
                if not paragraphs:
                    paragraphs = [data]
                for chunk in paragraphs:
                    chunks.append(
                        {"kb": kb_name, "source_type": "inline", "chunk": chunk}
                    )
            else:
                chunks.append(
                    {
                        "kb": kb_name,
                        "source_type": source_type,
                        "chunk": (
                            f"[{source_type} source from {kb_name} is not ingested "
                            "into this runtime's local index]"
                        ),
                    }
                )
    return chunks


def _build_knowledge_search(records: Any) -> Callable[[str, int], str]:
    """Return a keyword-search closure over the flattened KB records.

    The closure signature is ``search(query, k) -> str``. It always returns
    a string (JSON-encoded list on hit; canned message on empty input) so
    it can be handed to a ``StructuredTool`` without further wrapping.
    """
    if not records:
        def _empty(_query: str, _k: int) -> str:
            return _KB_NO_KB_MESSAGE
        return _empty

    chunks = _flatten_kb_chunks(records)

    def search(query: str, k: int) -> str:
        top_k = max(1, int(k) if k else 1)
        if not chunks:
            return _KB_NO_KB_MESSAGE

        query_tokens = {
            tok for tok in (t.lower() for t in _KB_WORD_RE.findall(query or ""))
            if len(tok) >= _KB_TOKEN_MIN_LEN
        }

        if not query_tokens:
            return json.dumps(chunks[:top_k])

        scored: list[tuple[int, int, dict[str, str]]] = []
        for idx, chunk in enumerate(chunks):
            body = chunk["chunk"].lower()
            score = sum(1 for tok in query_tokens if tok in body)
            if score > 0:
                scored.append((score, idx, chunk))

        if not scored:
            return json.dumps(chunks[:top_k])

        scored.sort(key=lambda triple: (-triple[0], triple[1]))
        return json.dumps([entry for _, _, entry in scored[:top_k]])

    return search


# --- pgvector-backed retrieval -------------------------------------------- #


def _kb_has_pgvector(rec: dict[str, Any]) -> bool:
    vs = rec.get("vectorStore")
    emb = rec.get("embedding")
    if not isinstance(vs, dict) or not isinstance(emb, dict):
        return False
    if str(vs.get("provider") or "").lower() != "pgvector":
        return False
    return bool(vs.get("dsn")) and bool(emb.get("model"))


def _embed_query(embedding: dict[str, Any], query: str) -> list[float]:
    """Embed ``query`` using the record's embedding provider.

    Lazily imports ``openai`` so agents that never touch pgvector KBs don't
    pay the import cost. Raises on any error — callers catch and log so a
    single KB failing doesn't take the whole tool down.
    """
    provider = str(embedding.get("provider") or "openai").lower()
    model = str(embedding.get("model") or "").strip()
    if not model:
        raise ValueError("embedding.model is empty")

    if provider == "azure_openai":
        from openai import AzureOpenAI  # type: ignore[import-not-found]

        api_key = str(embedding.get("apiKey") or "")
        endpoint = _embedding_azure_root(str(embedding.get("baseUrl") or ""))
        api_version = str(embedding.get("apiVersion") or "2024-06-01")
        if not api_key and os.getenv("AZURE_TENANT_ID"):
            client = AzureOpenAI(
                azure_ad_token_provider=_embedding_azure_ad_token_provider(),
                azure_endpoint=endpoint,
                api_version=api_version,
            )
        else:
            client = AzureOpenAI(
                api_key=api_key,
                azure_endpoint=endpoint,
                api_version=api_version,
            )
    else:
        from openai import OpenAI  # type: ignore[import-not-found]

        client = OpenAI(
            api_key=str(embedding.get("apiKey") or ""),
            base_url=str(embedding.get("baseUrl") or "https://api.openai.com/v1"),
        )
    options: dict[str, Any] = {}
    if "dimensions" in embedding:
        dimensions = embedding["dimensions"]
        if isinstance(dimensions, bool) or not isinstance(dimensions, int) or dimensions < 1:
            raise ValueError("embedding dimensions must be a positive integer")
        options["dimensions"] = dimensions
    resp = client.embeddings.create(model=model, input=query, **options)
    return list(resp.data[0].embedding)


def _embedding_azure_root(endpoint: str) -> str:
    """Same normalisation as runtime/agent/providers.py._azure_resource_root."""
    if not endpoint:
        return endpoint
    from urllib.parse import urlparse, urlunparse

    parsed = urlparse(endpoint)
    if not parsed.scheme or not parsed.netloc:
        return endpoint
    path = re.sub(r"(?i)/openai(/.*)?$", "", parsed.path or "")
    path = re.sub(r"(?i)/api/projects(/[^/]+)?$", "", path)
    return urlunparse((parsed.scheme, parsed.netloc, path, "", "", ""))


def _embedding_azure_ad_token_provider(
    scope: str = "https://cognitiveservices.azure.com/.default",
):
    """Token provider for Azure OpenAI embeddings.

    Supports client-secret, managed identity, and workload-identity federation
    (see azure_creds.build_azure_credential)."""
    from azure_creds import azure_ad_token_provider

    return azure_ad_token_provider(scope)


def _pgvector_search(
    rec: dict[str, Any],
    query: str,
    k: int,
) -> list[dict[str, Any]]:
    """Run a nearest-neighbour search against the pgvector store for ``rec``.

    Returns a list of ``{"kb", "source_type", "source_uri", "chunk", "score"}``
    dicts sorted by descending score. Rows below ``retrieval.minScore`` are
    dropped. On failure logs and returns ``[]``.
    """
    vs = rec.get("vectorStore") or {}
    retrieval = rec.get("retrieval") or {}
    kb_name = str(rec.get("name") or "")
    dsn = str(vs.get("dsn") or "")
    table = str(vs.get("table") or "kognition_kb_chunks")
    top_k = int(retrieval.get("topK") or k or 5)
    min_score = float(retrieval.get("minScore") or 0.0)

    try:
        import psycopg  # type: ignore[import-not-found]
        from pgvector.psycopg import register_vector  # type: ignore[import-not-found]
    except ImportError as exc:
        logger.warning("pgvector search skipped for kb=%s: %s", kb_name, exc)
        return []

    try:
        vector = _embed_query(rec.get("embedding") or {}, query)
    except Exception as exc:  # noqa: BLE001 - degrade to no hits
        logger.warning("embedding failed for kb=%s: %s", kb_name, exc)
        return []

    # Whitelist the table identifier since we splice it into SQL. Only allow
    # the same subset pg accepts unquoted so an operator can pick any table
    # name they like without opening SQL-injection.
    if not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", table):
        logger.warning("pgvector table name rejected for kb=%s: %r", kb_name, table)
        return []

    sql = (
        f"SELECT source_uri, content, "
        f"       1 - (embedding <=> %s::vector) AS score "
        f"FROM {table} "
        f"WHERE kb_namespace = %s AND kb_name = %s "
        f"ORDER BY embedding <=> %s::vector "
        f"LIMIT %s"
    )
    ns = str(rec.get("namespace") or "")
    try:
        with psycopg.connect(dsn, autocommit=True) as conn:
            register_vector(conn)
            with conn.cursor() as cur:
                cur.execute(sql, (vector, ns, kb_name, vector, top_k))
                rows = cur.fetchall()
    except Exception as exc:  # noqa: BLE001 - degrade to no hits
        logger.warning("pgvector query failed for kb=%s: %s", kb_name, exc)
        return []

    hits: list[dict[str, Any]] = []
    for source_uri, content, score in rows:
        if score is not None and float(score) < min_score:
            continue
        hits.append(
            {
                "kb": kb_name,
                "source_type": "blob",
                "source_uri": str(source_uri or ""),
                "chunk": str(content or ""),
                "score": float(score) if score is not None else None,
            }
        )
    return hits


def _build_kb_search_tool(
    records: Any,
    knowledge_gateway_url: str = "",
    knowledge_gateway_ca_file: str = "",
) -> StructuredTool | None:
    """Return a ``knowledge_search`` StructuredTool bound to ``records``.

    Records with a bound pgvector store use vector search; the rest fall
    through to the in-memory keyword search built by
    :func:`_build_knowledge_search`. Returns ``None`` when there are no
    records at all — the caller should just skip attaching the tool.
    """
    if not records or not isinstance(records, list):
        return None

    provider_records = [rec for rec in records if isinstance(rec, dict) and rec.get("providerBacked") is True]
    if provider_records:
        parsed_gateway_url = urlparse(knowledge_gateway_url)
        if parsed_gateway_url.scheme != "https" or not parsed_gateway_url.netloc:
            raise ValueError("provider-backed knowledge requires an HTTPS Knowledge Gateway URL")
        if not knowledge_gateway_ca_file:
            raise ValueError("provider-backed knowledge requires a Knowledge Gateway CA file")
        for record in provider_records:
            mode = str((record.get("retrieval") or {}).get("mode") or "keyword")
            if mode not in {"keyword", "dense", "hybrid"}:
                raise ValueError("provider-backed knowledge retrieval mode is unsupported")
    vector_records = [rec for rec in records if isinstance(rec, dict) and not rec.get("providerBacked") and _kb_has_pgvector(rec)]
    keyword_records = [rec for rec in records if not (isinstance(rec, dict) and (rec.get("providerBacked") or _kb_has_pgvector(rec)))]
    keyword_search = _build_knowledge_search(keyword_records) if keyword_records else None

    class _KBSearchArgs(BaseModel):
        query: str = Field(default="", description="Question or keywords to search the knowledge base for.")
        k: int = Field(default=5, description="Maximum number of chunks to return.")

    def _search(query: str = "", k: int = 5) -> str:
        top_k = max(1, int(k) if k else 1)
        aggregated: list[dict[str, Any]] = []

        if provider_records:
            capability = _knowledge_session_capability.get()
            if not knowledge_gateway_url or not capability:
                raise RuntimeError("Knowledge Gateway is not available for the active session.")
            endpoint = knowledge_gateway_url.rstrip("/") + "/v1/query"
            for rec in provider_records:
                retrieval = rec.get("retrieval") or {}
                filters = {
                    str(key) if str(key).startswith("metadata.") else f"metadata.{key}": str(value)
                    for key, value in (retrieval.get("filter") or {}).items()
                }
                payload = {
                    "knowledge_base": str(rec.get("name") or ""),
                    "request_id": str(uuid.uuid4()),
                    "mode": str(retrieval.get("mode") or "keyword"),
                    "top_k": int(retrieval.get("topK") or top_k),
                    "filters": filters,
                }
                if payload["mode"] == "keyword":
                    payload["text"] = query
                else:
                    payload["query_text"] = query
                headers = runtime_tracing.inject_trace_headers()
                headers["Authorization"] = f"Bearer {capability}"
                try:
                    response = httpx.post(
                        endpoint,
                        json=payload,
                        headers=headers,
                        timeout=_HTTP_TIMEOUT_SECONDS,
                        verify=knowledge_gateway_ca_file,
                    )
                except httpx.HTTPError as exc:
                    logger.warning("Knowledge Gateway query failed for kb=%s: %s", payload["knowledge_base"], type(exc).__name__)
                    raise RuntimeError("Knowledge Gateway query failed.") from None
                if response.status_code >= 400:
                    logger.warning("Knowledge Gateway returned HTTP %d for kb=%s", response.status_code, payload["knowledge_base"])
                    raise RuntimeError(f"Knowledge Gateway returned HTTP {response.status_code}.")
                try:
                    body = response.json()
                except ValueError:
                    raise RuntimeError("Knowledge Gateway returned an invalid response.") from None
                matches = body.get("matches") if isinstance(body, dict) else None
                if not isinstance(matches, list):
                    raise RuntimeError("Knowledge Gateway returned an invalid response.")
                aggregated.extend(match for match in matches if isinstance(match, dict))

        for rec in vector_records:
            per_kb_k = int(((rec.get("retrieval") or {}).get("topK")) or top_k)
            aggregated.extend(_pgvector_search(rec, query, per_kb_k))

        if keyword_search is not None:
            raw = keyword_search(query, top_k)
            if raw and raw != _KB_NO_KB_MESSAGE:
                try:
                    aggregated.extend(json.loads(raw))
                except json.JSONDecodeError:
                    logger.warning("keyword search returned non-JSON payload; ignored")

        if not aggregated:
            if provider_records:
                return "No matching passages were found in the attached knowledge bases."
            return _KB_NO_KB_MESSAGE

        # Prefer scored hits (pgvector) first, then keyword hits in the order
        # the keyword search returned them. Stable sort keeps that ordering.
        aggregated.sort(
            key=lambda entry: (0 if isinstance(entry.get("score"), (int, float)) else 1,
                               -float(entry.get("score") or 0.0)),
        )
        return json.dumps(aggregated[:top_k])

    return StructuredTool.from_function(
        func=_search,
        name="knowledge_search",
        description=(
            "Search the agent's knowledge bases for chunks relevant to the "
            "user's question. Returns a JSON list of chunks with their source "
            "KB and source type."
        ),
        args_schema=_KBSearchArgs,
    )
