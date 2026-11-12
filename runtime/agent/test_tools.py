# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Pytest coverage for the knowledge-base helpers in :mod:`tools`.

Run via::

    pip install -r requirements.txt pytest
    pytest runtime/agent

The module imports langchain_core at top level, so tests are skipped
when that dependency is not installed.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

pytest.importorskip("langchain_core")
pytest.importorskip("pydantic")

# Allow ``import tools`` when pytest is run from the repo root.
sys.path.insert(0, str(Path(__file__).parent))

from tools import (  # noqa: E402
    _build_kb_search_tool,
    _build_knowledge_search,
    _flatten_kb_chunks,
    _make_http_executor,
    _kb_has_pgvector,
    _pgvector_search,
    ToolDef,
    build_tools,
    _make_gateway_executor,
    build_agent_delegation_tool,
    agent_delegation_runtime_session_capability,
    knowledge_runtime_session_capability,
    tool_runtime_session_capability,
)


# --------------------------------------------------------------------------- #
# _flatten_kb_chunks
# --------------------------------------------------------------------------- #


@pytest.mark.parametrize("backend_output,expected", [({"ok": True}, {"ok": True}), ({"result": "preserved upstream string"}, "preserved upstream string"), ({"result": [{"uid": "owned"}]}, [{"uid": "owned"}])])
def test_reviewed_runtime_alias_preserves_gateway_resource_identity(monkeypatch, backend_output, expected) -> None:
    observed = []

    def post(url, **kwargs):
        observed.append((url, kwargs))
        payload = {"capability": "invocation-proof"} if url.endswith("/capabilities") else {"output": backend_output}
        return type("Response", (), {"status_code": 200, "json": lambda self: payload, "raise_for_status": lambda self: None, "text": ""})()

    monkeypatch.setattr("tools.httpx.post", post)
    tools = build_tools(json.dumps([{"name": "port-query", "runtimeName": "QueryTool", "resourceVersion": 7, "inputSchema": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}}]), "[]", "http://fixture-gateway")
    assert tools[0].name == "QueryTool"
    with tool_runtime_session_capability("session-proof"):
        result = tools[0].invoke({"query": "up"})
        assert (result if isinstance(expected, str) else json.loads(result)) == expected
    assert all(call[1]["json"]["tool_name"] == "port-query" for call in observed)
    assert observed[1][1]["json"]["tool_resource_version"] == 7


@pytest.mark.parametrize("aliases", [["bad/name"], ["QueryTool", "QueryTool"]])
def test_reviewed_runtime_alias_rejects_malformed_or_ambiguous_names(aliases) -> None:
    resolved = [{"name": f"port-tool-{index}", "runtimeName": alias} for index, alias in enumerate(aliases)]
    with pytest.raises(ValueError, match="runtime tool name"):
        build_tools(json.dumps(resolved), "[]", "http://fixture-gateway")


def test_imported_schema_preserves_empty_and_required_nullable_arguments() -> None:
    empty = build_tools(json.dumps([{"name":"api-resources","runtimeName":"get_available_api_resources","inputSchema":{"type":"object","properties":{},"required":[]}}]), "[]", "http://fixture-gateway")[0]
    assert empty.args_schema.model_json_schema().get("properties") == {}
    nullable = build_tools(json.dumps([{"name":"get-resources","runtimeName":"get_resources","inputSchema":{"type":"object","properties":{"name":{"anyOf":[{"type":"string"},{"type":"null"}]}},"required":["name"]}}]), "[]", "http://fixture-gateway")[0]
    assert nullable.args_schema.model_validate({"name":None}).name is None
    with pytest.raises(ValueError):
        nullable.args_schema.model_validate({})


def test_imported_schema_preserves_references_unions_and_typed_arrays() -> None:
    schema = {"type": "object", "$defs": {"Action": {"type": "string", "enum": ["search", "get"]}}, "properties": {"action": {"$ref": "#/$defs/Action"}, "time": {"anyOf": [{"type": "string", "format": "date-time"}, {"type": "number"}, {"type": "null"}], "default": None}, "tags": {"type": "array", "items": {"type": "string"}}, "permissions": {"type": "array", "items": {"type": "object"}}}, "required": ["action", "tags", "permissions"]}
    tool = build_tools(json.dumps([{"name": "port-dashboard", "runtimeName": "DashboardManagementTool", "description": "Pinned description\n    ", "inputSchema": schema}]), "[]", "http://fixture-gateway")[0]
    assert tool.description == "Pinned description\n    "
    assert tool.args_schema.model_json_schema() == schema
    args = tool.args_schema.model_validate({"action": "search", "time": 0.5, "tags": ["owned"], "permissions": [{"role": "Viewer"}]})
    assert args.time == 0.5
    for invalid in ({"action": "other", "tags": [], "permissions": []}, {"action": "get", "tags": [1], "permissions": []}, {"action": "get", "tags": [], "permissions": [1]}):
        with pytest.raises(ValueError):
            tool.args_schema.model_validate(invalid)


def test_flatten_empty_returns_empty() -> None:
    assert _flatten_kb_chunks([]) == []


def test_provider_backed_knowledge_uses_authenticated_gateway(monkeypatch: pytest.MonkeyPatch) -> None:
    observed: dict = {}

    def post(url, **kwargs):
        observed.update(url=url, **kwargs)
        return type("Response", (), {
            "status_code": 200,
            "json": lambda self: {"request_id": "request-1", "matches": [{"kb": "docs", "chunk": "grounded answer", "score": 0.9}]},
            "text": "",
        })()

    monkeypatch.setattr("tools.httpx.post", post)
    monkeypatch.setattr("tools.runtime_tracing.inject_trace_headers", lambda: {"traceparent": "00-test"})
    tool = _build_kb_search_tool(
        [{"name": "docs", "providerBacked": True, "retrieval": {"topK": 3, "filter": {"language": "en"}}}],
        "https://knowledge-gateway:8095",
        "/var/run/kognition/knowledge-gateway/ca.crt",
    )

    with knowledge_runtime_session_capability("knowledge-capability"):
        result = json.loads(tool.invoke({"query": "What is grounded?", "k": 5}))

    assert result == [{"kb": "docs", "chunk": "grounded answer", "score": 0.9}]
    assert observed["url"] == "https://knowledge-gateway:8095/v1/query"
    assert observed["verify"] == "/var/run/kognition/knowledge-gateway/ca.crt"
    assert observed["headers"] == {"traceparent": "00-test", "Authorization": "Bearer knowledge-capability"}
    assert observed["json"]["knowledge_base"] == "docs"
    assert observed["json"]["mode"] == "keyword"
    assert observed["json"]["text"] == "What is grounded?"
    assert observed["json"]["top_k"] == 3
    assert observed["json"]["filters"] == {"metadata.language": "en"}

@pytest.mark.parametrize("mode", ["dense", "hybrid"])
def test_provider_backed_knowledge_declares_text_query_mode(monkeypatch: pytest.MonkeyPatch, mode: str) -> None:
    observed: dict = {}

    def post(url, **kwargs):
        observed.update(kwargs)
        return type("Response", (), {
            "status_code": 200,
            "json": lambda self: {"matches": [{"kb": "docs", "chunk": "retrieved fact"}]},
        })()

    monkeypatch.setattr("tools.httpx.post", post)
    tool = _build_kb_search_tool(
        [{"name": "docs", "providerBacked": True, "retrieval": {"mode": mode}}],
        "https://knowledge-gateway:8095",
        "/trust/ca.crt",
    )
    with knowledge_runtime_session_capability("session-capability"):
        result = json.loads(tool.invoke({"query": "supported regions"}))
    assert result[0]["chunk"] == "retrieved fact"
    assert observed["json"]["mode"] == mode
    assert observed["json"]["query_text"] == "supported regions"
    assert "text" not in observed["json"]
    assert "vector" not in observed["json"]


def test_provider_backed_knowledge_failure_is_not_an_empty_search(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("tools.httpx.post", lambda *args, **kwargs: type("Response", (), {"status_code": 503})())
    tool = _build_kb_search_tool(
        [{"name": "docs", "providerBacked": True}],
        "https://knowledge-gateway:8095",
        "/trust/ca.crt",
    )
    with knowledge_runtime_session_capability("session-capability"), pytest.raises(RuntimeError, match="HTTP 503"):
        tool.invoke({"query": "regions"})


@pytest.mark.parametrize(
    ("gateway_url", "ca_file", "message"),
    [
        ("http://knowledge-gateway:8095", "/trust/ca.crt", "HTTPS Knowledge Gateway URL"),
        ("https://knowledge-gateway:8095", "", "Knowledge Gateway CA file"),
    ],
)
def test_provider_backed_knowledge_fails_closed_without_verified_https(
    gateway_url: str,
    ca_file: str,
    message: str,
) -> None:
    with pytest.raises(ValueError, match=message):
        _build_kb_search_tool([{"name": "docs", "providerBacked": True}], gateway_url, ca_file)


def test_flatten_handles_non_dict_entries() -> None:
    # Non-dict KB entries are silently skipped.
    assert _flatten_kb_chunks(["not-a-dict", 42, None]) == []  # type: ignore[list-item]


def test_flatten_skips_records_without_name() -> None:
    records = [{"name": "   ", "sources": [{"type": "inline", "data": "x"}]}]
    assert _flatten_kb_chunks(records) == []


def test_flatten_inline_splits_on_blank_lines() -> None:
    records = [
        {
            "name": "kb1",
            "sources": [
                {"type": "inline", "data": "first paragraph\n\nsecond paragraph"},
            ],
        }
    ]
    chunks = _flatten_kb_chunks(records)
    assert len(chunks) == 2
    assert chunks[0] == {"kb": "kb1", "source_type": "inline", "chunk": "first paragraph"}
    assert chunks[1] == {"kb": "kb1", "source_type": "inline", "chunk": "second paragraph"}


def test_flatten_inline_falls_back_to_whole_blob() -> None:
    records = [{"name": "kb1", "sources": [{"type": "inline", "data": "single blob"}]}]
    chunks = _flatten_kb_chunks(records)
    assert chunks == [{"kb": "kb1", "source_type": "inline", "chunk": "single blob"}]


def test_flatten_inline_skips_empty_data() -> None:
    records = [{"name": "kb1", "sources": [{"type": "inline", "data": "   "}]}]
    assert _flatten_kb_chunks(records) == []


def test_flatten_non_inline_emits_stub() -> None:
    records = [
        {
            "name": "kb1",
            "sources": [
                {"type": "url", "uri": "https://example.com"},
                {"type": "git"},
            ],
        }
    ]
    chunks = _flatten_kb_chunks(records)
    assert len(chunks) == 2
    assert chunks[0]["source_type"] == "url"
    assert "not ingested" in chunks[0]["chunk"]
    assert chunks[1]["source_type"] == "git"


def test_flatten_skips_non_dict_sources() -> None:
    records = [{"name": "kb1", "sources": ["bad", 1, None, {"type": "inline", "data": "ok"}]}]
    chunks = _flatten_kb_chunks(records)
    assert len(chunks) == 1
    assert chunks[0]["chunk"] == "ok"


def test_flatten_unknown_source_type_uses_unknown_label() -> None:
    records = [{"name": "kb1", "sources": [{"type": ""}]}]
    chunks = _flatten_kb_chunks(records)
    assert len(chunks) == 1
    assert chunks[0]["source_type"] == "unknown"


# --------------------------------------------------------------------------- #
# _build_knowledge_search
# --------------------------------------------------------------------------- #


def test_search_empty_kb_records() -> None:
    search = _build_knowledge_search([])
    assert search("anything", 3) == "No knowledge bases are attached to this agent."


def test_search_none_kb_records() -> None:
    search = _build_knowledge_search(None)
    assert search("anything", 3) == "No knowledge bases are attached to this agent."


def test_search_scores_by_unique_token_hits() -> None:
    # Scoring counts unique query tokens that appear in the chunk (not total
    # occurrences). A chunk that contains 3 of the 3 query tokens beats one
    # that contains 1.
    records = [
        {
            "name": "kb1",
            "sources": [
                {"type": "inline", "data": "alpha only here"},
                {"type": "inline", "data": "delta epsilon"},
                {"type": "inline", "data": "alpha beta gamma combined"},
            ],
        }
    ]
    search = _build_knowledge_search(records)
    result = json.loads(search("alpha beta gamma", 2))
    assert len(result) == 2
    assert result[0]["chunk"] == "alpha beta gamma combined"
    assert result[1]["chunk"] == "alpha only here"


def test_search_chunks_without_any_token_are_excluded() -> None:
    records = [
        {
            "name": "kb1",
            "sources": [
                {"type": "inline", "data": "alpha first"},
                {"type": "inline", "data": "delta epsilon"},
            ],
        }
    ]
    search = _build_knowledge_search(records)
    result = json.loads(search("alpha", 5))
    assert len(result) == 1
    assert result[0]["chunk"] == "alpha first"


def test_search_respects_top_k() -> None:
    records = [
        {
            "name": "kb1",
            "sources": [{"type": "inline", "data": f"foo bar baz item-{i}\n\nfoo bar baz item-{i+1}"} for i in range(5)],
        }
    ]
    search = _build_knowledge_search(records)
    result = json.loads(search("foo", 3))
    assert len(result) == 3


def test_http_executor_injects_current_trace_headers(monkeypatch: pytest.MonkeyPatch) -> None:
    captured: dict[str, str] = {}

    class FakeResponse:
        status_code = 200
        text = "ok"
        headers = {"content-type": "text/plain"}

        def json(self):  # pragma: no cover - text response path
            raise ValueError("not json")

    def fake_post(_endpoint: str, *, json: dict, timeout: float, headers: dict[str, str]):
        del json, timeout
        captured.update(headers)
        return FakeResponse()

    monkeypatch.setattr("tools.httpx.post", fake_post)
    monkeypatch.setattr(
        "tools.runtime_tracing.inject_trace_headers",
        lambda headers=None: {**(headers or {}), "traceparent": "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"},
    )

    executor = _make_http_executor(ToolDef(name="lookup", protocol="http", endpoint="https://tools.example/lookup"))
    assert executor(query="alpha") == "ok"
    assert captured["traceparent"] == "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"


def test_gateway_executor_exchanges_session_for_exact_tool_capability(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, dict, dict[str, str]]] = []

    class FakeResponse:
        status_code = 200
        text = ""

        def __init__(self, body: dict) -> None:
            self.body = body

        def raise_for_status(self) -> None:
            return None

        def json(self) -> dict:
            return self.body

    def fake_post(endpoint: str, *, json: dict, timeout: float, headers: dict[str, str]) -> FakeResponse:
        del timeout
        calls.append((endpoint, json, headers))
        if endpoint.endswith("/capabilities"):
            return FakeResponse({"capability": "exact-capability"})
        return FakeResponse({"output": {"updated": True}})

    monkeypatch.setattr("tools.httpx.post", fake_post)
    monkeypatch.setattr("tools.uuid.uuid4", lambda: "invocation-1")
    executor = _make_gateway_executor(ToolDef(name="crm-update", resource_version=7), "http://tool-gateway:8092")
    with tool_runtime_session_capability("session-capability"):
        assert executor(ticket="42") == '{"updated": true}'
    assert calls[0][0] == "http://tool-gateway:8092/v1/tools/capabilities"
    assert calls[0][1] == {"tool_name": "crm-update", "invocation_id": "invocation-1", "input": {"ticket": "42"}}
    assert calls[0][2]["Authorization"] == "Bearer session-capability"
    assert calls[1][0] == "http://tool-gateway:8092/v1/tools/invoke"
    assert calls[1][1] == {"tool_name": "crm-update", "tool_resource_version": 7, "invocation_id": "invocation-1", "input": {"ticket": "42"}}
    assert calls[1][2]["Authorization"] == "Bearer exact-capability"


def test_gateway_executor_retries_pending_with_exact_request_identity(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, dict, dict[str, str]]] = []
    invocation_ids = iter(["invocation-1", "invocation-2"])

    class FakeResponse:
        text = ""

        def __init__(self, status_code: int, body: dict) -> None:
            self.status_code = status_code
            self.body = body

        def raise_for_status(self) -> None:
            return None

        def json(self) -> dict:
            return self.body

    def fake_post(endpoint: str, *, json: dict, timeout: float, headers: dict[str, str]) -> FakeResponse:
        del timeout
        calls.append((endpoint, json, headers))
        if endpoint.endswith("/capabilities"):
            return FakeResponse(200, {"capability": "exact-capability", "runtime_approval_state": "ready"})
        if len(calls) == 2:
            return FakeResponse(202, {
                "invocation_id": "invocation-1",
                "runtime_approval_state": "pending",
                "runtime_approval": {"name": "approval-1"},
            })
        return FakeResponse(200, {"output": {"updated": True}, "runtime_approval_state": "terminal"})

    monkeypatch.setattr("tools.httpx.post", fake_post)
    monkeypatch.setattr("tools.uuid.uuid4", lambda: next(invocation_ids))
    executor = _make_gateway_executor(ToolDef(name="crm-update", resource_version=7), "http://tool-gateway:8092")
    typed_input = {"ticket": 42, "urgent": True, "labels": ["billing", "vip"], "metadata": {"attempt": 1}}

    with tool_runtime_session_capability("session-capability"):
        pending = json.loads(executor(**typed_input))
        completed = json.loads(executor(**typed_input))

    assert pending["runtime_approval_state"] == "pending"
    assert completed == {"updated": True}
    assert [call[1]["invocation_id"] for call in calls] == ["invocation-1", "invocation-1", "invocation-1", "invocation-1"]
    assert all(call[1]["input"] == typed_input for call in calls)
    assert calls[0][2]["Authorization"] == "Bearer session-capability"
    assert calls[1][2]["Authorization"] == "Bearer exact-capability"
    assert calls[2][2]["Authorization"] == "Bearer session-capability"
    assert calls[3][2]["Authorization"] == "Bearer exact-capability"


def test_gateway_executor_retries_unavailable_invoke_with_exact_request_identity(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, dict, dict[str, str]]] = []
    invocation_ids = iter(["invocation-1", "invocation-2"])

    class FakeResponse:
        text = ""

        def __init__(self, status_code: int, body: dict) -> None:
            self.status_code = status_code
            self.body = body
            self.text = json.dumps(body)

        def raise_for_status(self) -> None:
            return None

        def json(self) -> dict:
            return self.body

    invoke_calls = 0

    def fake_post(endpoint: str, *, json: dict, timeout: float, headers: dict[str, str]) -> FakeResponse:
        nonlocal invoke_calls
        del timeout
        calls.append((endpoint, json, headers))
        if endpoint.endswith("/capabilities"):
            return FakeResponse(200, {"capability": "exact-capability", "runtime_approval_state": "ready"})
        invoke_calls += 1
        if invoke_calls == 1:
            return FakeResponse(500, {"invocation_id": "invocation-1", "runtime_approval_state": "unavailable", "error_code": "ToolSettlementFailed"})
        return FakeResponse(409, {"invocation_id": "invocation-1", "runtime_approval_state": "unavailable", "error_code": "ToolInvocationInProgress"})

    monkeypatch.setattr("tools.httpx.post", fake_post)
    monkeypatch.setattr("tools.uuid.uuid4", lambda: next(invocation_ids))
    executor = _make_gateway_executor(ToolDef(name="crm-update", resource_version=7), "http://tool-gateway:8092")

    with tool_runtime_session_capability("session-capability"):
        executor(ticket="42")
        executor(ticket="42")

    assert [call[1]["invocation_id"] for call in calls] == ["invocation-1"] * 4


def test_agent_delegation_tool_exchanges_and_invokes_exact_capability(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, dict, dict[str, str]]] = []

    class FakeResponse:
        status_code = 200
        text = ""

        def __init__(self, body: dict) -> None:
            self.body = body

        def raise_for_status(self) -> None:
            return None

        def json(self) -> dict:
            return self.body

    def fake_post(endpoint: str, *, json: dict, timeout: float, headers: dict[str, str]) -> FakeResponse:
        del timeout
        calls.append((endpoint, json, headers))
        if endpoint.endswith("/capabilities"):
            return FakeResponse({"capability": "exact-delegation"})
        return FakeResponse({"session_id": "delegation-1", "target_agent": "reviewer"})

    monkeypatch.setattr("tools.httpx.post", fake_post)
    monkeypatch.setattr("tools.uuid.uuid4", lambda: "delegation-1")
    tool = build_agent_delegation_tool("http://agent-gateway:8094")
    assert tool is not None
    with agent_delegation_runtime_session_capability("source-session"):
        assert tool.invoke({"target_namespace": "team-b", "target_agent_service": "review-service", "message": "review this"}) == '{"session_id": "delegation-1", "target_agent": "reviewer"}'
    assert calls[0][0] == "http://agent-gateway:8094/v1/agent-services/capabilities"
    assert calls[0][1] == {"target_namespace": "team-b", "target_agent_service": "review-service", "invocation_id": "delegation-1", "message": "review this"}
    assert calls[0][2]["Authorization"] == "Bearer source-session"
    assert calls[1][0] == "http://agent-gateway:8094/v1/agent-services/invoke"
    assert calls[1][1] == {"target_namespace": "team-b", "target_agent_service": "review-service", "invocation_id": "delegation-1", "message": "review this"}
    assert calls[1][2]["Authorization"] == "Bearer exact-delegation"


def test_agent_delegation_tool_returns_pending_approval_without_invoking(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, dict, dict[str, str]]] = []

    class FakeResponse:
        status_code = 202
        text = ""

        def raise_for_status(self) -> None:
            return None

        def json(self) -> dict:
            return {
                "invocation_id": "delegation-1",
                "runtime_approval_state": "Pending",
                "runtime_approval": {"kind": "RuntimeApproval", "namespace": "team-a", "name": "approval-1"},
            }

    def fake_post(endpoint: str, *, json: dict, timeout: float, headers: dict[str, str]) -> FakeResponse:
        del timeout
        calls.append((endpoint, json, headers))
        return FakeResponse()

    monkeypatch.setattr("tools.httpx.post", fake_post)
    monkeypatch.setattr("tools.uuid.uuid4", lambda: "delegation-1")
    tool = build_agent_delegation_tool("http://agent-gateway:8094")
    assert tool is not None
    with agent_delegation_runtime_session_capability("source-session"):
        result = json.loads(tool.invoke({"target_namespace": "team-b", "target_agent_service": "review-service", "message": "review this"}))

    assert result["runtime_approval_state"] == "Pending"
    assert result["runtime_approval"]["name"] == "approval-1"
    assert len(calls) == 1
    assert calls[0][0] == "http://agent-gateway:8094/v1/agent-services/capabilities"
    assert calls[0][1]["message"] == "review this"


def test_search_empty_query_falls_back_to_first_chunks() -> None:
    records = [
        {
            "name": "kb1",
            "sources": [{"type": "inline", "data": "alpha\n\nbeta\n\ngamma"}],
        }
    ]
    search = _build_knowledge_search(records)
    result = json.loads(search("", 2))
    assert len(result) == 2
    assert result[0]["chunk"] == "alpha"
    assert result[1]["chunk"] == "beta"


def test_search_no_match_falls_back_to_first_chunks() -> None:
    records = [
        {
            "name": "kb1",
            "sources": [{"type": "inline", "data": "alpha\n\nbeta"}],
        }
    ]
    search = _build_knowledge_search(records)
    result = json.loads(search("nothingmatches", 1))
    assert len(result) == 1


def test_search_short_tokens_ignored() -> None:
    # Tokens shorter than 3 chars are filtered out, so query becomes "empty"
    # and the fallback kicks in.
    records = [
        {
            "name": "kb1",
            "sources": [{"type": "inline", "data": "the cat\n\nthe dog"}],
        }
    ]
    search = _build_knowledge_search(records)
    result = json.loads(search("a b c", 2))
    assert len(result) == 2  # fallback to first chunks


# --------------------------------------------------------------------------- #
# _kb_has_pgvector
# --------------------------------------------------------------------------- #


def test_kb_has_pgvector_requires_both_blocks() -> None:
    assert _kb_has_pgvector({}) is False
    assert _kb_has_pgvector({"vectorStore": {"provider": "pgvector", "dsn": "x"}}) is False
    assert _kb_has_pgvector({"embedding": {"model": "m"}}) is False


def test_kb_has_pgvector_requires_pgvector_provider() -> None:
    rec = {
        "vectorStore": {"provider": "other", "dsn": "x"},
        "embedding": {"model": "m"},
    }
    assert _kb_has_pgvector(rec) is False


def test_kb_has_pgvector_requires_non_empty_dsn_and_model() -> None:
    rec = {
        "vectorStore": {"provider": "pgvector", "dsn": ""},
        "embedding": {"model": "m"},
    }
    assert _kb_has_pgvector(rec) is False
    rec2 = {
        "vectorStore": {"provider": "pgvector", "dsn": "x"},
        "embedding": {"model": ""},
    }
    assert _kb_has_pgvector(rec2) is False


def test_kb_has_pgvector_accepts_case_insensitive_provider() -> None:
    rec = {
        "vectorStore": {"provider": "PgVector", "dsn": "x"},
        "embedding": {"model": "m"},
    }
    assert _kb_has_pgvector(rec) is True


# --------------------------------------------------------------------------- #
# _pgvector_search
#
# Guards the ingester-schema contract: SELECT must be a 3-tuple
# (source_uri, content, score) — no `source_type` column. Also verifies the
# table-name whitelist, minScore filter, and graceful degradation on failure.
# --------------------------------------------------------------------------- #


class _FakeCursor:
    def __init__(self, rows: list[tuple], captured: dict) -> None:
        self._rows = rows
        self._captured = captured

    def execute(self, sql: str, params: tuple) -> None:
        self._captured["sql"] = sql
        self._captured["params"] = params

    def fetchall(self) -> list[tuple]:
        return list(self._rows)

    def __enter__(self) -> "_FakeCursor":
        return self

    def __exit__(self, *args: object) -> None:
        return None


class _FakeConn:
    def __init__(self, rows: list[tuple], captured: dict) -> None:
        self._rows = rows
        self._captured = captured

    def cursor(self) -> _FakeCursor:
        return _FakeCursor(self._rows, self._captured)

    def __enter__(self) -> "_FakeConn":
        return self

    def __exit__(self, *args: object) -> None:
        return None


def _install_fake_psycopg(monkeypatch: pytest.MonkeyPatch, rows: list[tuple], captured: dict) -> None:
    """Inject fake ``psycopg`` and ``pgvector.psycopg`` modules for _pgvector_search."""
    import types

    fake_psycopg = types.ModuleType("psycopg")

    def _connect(dsn: str, autocommit: bool = False) -> _FakeConn:
        captured["dsn"] = dsn
        captured["autocommit"] = autocommit
        return _FakeConn(rows, captured)

    fake_psycopg.connect = _connect  # type: ignore[attr-defined]

    fake_pgvector = types.ModuleType("pgvector")
    fake_pgvector_psycopg = types.ModuleType("pgvector.psycopg")
    fake_pgvector_psycopg.register_vector = lambda _conn: None  # type: ignore[attr-defined]

    monkeypatch.setitem(sys.modules, "psycopg", fake_psycopg)
    monkeypatch.setitem(sys.modules, "pgvector", fake_pgvector)
    monkeypatch.setitem(sys.modules, "pgvector.psycopg", fake_pgvector_psycopg)


def _stub_embed(monkeypatch: pytest.MonkeyPatch, vector: list[float] | None = None) -> None:
    import tools as tools_mod

    monkeypatch.setattr(tools_mod, "_embed_query", lambda _emb, _q: vector or [0.1, 0.2, 0.3])


def _base_rec() -> dict:
    return {
        "namespace": "default",
        "name": "kognition-docs",
        "vectorStore": {
            "provider": "pgvector",
            "dsn": "postgresql://user:pass@localhost/db",
            "table": "kognition_docs_chunks",
            "dimension": 1536,
        },
        "embedding": {
            "provider": "azure_openai",
            "model": "text-embedding-3-small",
            "baseUrl": "https://example/",
        },
    }


def test_pgvector_search_returns_hits_with_hardcoded_blob_source_type(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict = {}
    rows = [
        ("README.md", "first chunk body", 0.55),
        ("PROJECT.md", "second chunk body", 0.42),
    ]
    _install_fake_psycopg(monkeypatch, rows, captured)
    _stub_embed(monkeypatch, vector=[0.1] * 4)

    hits = _pgvector_search(_base_rec(), "what is kognition", 5)

    assert len(hits) == 2
    assert hits[0] == {
        "kb": "kognition-docs",
        "source_type": "blob",
        "source_uri": "README.md",
        "chunk": "first chunk body",
        "score": pytest.approx(0.55),
    }
    assert hits[1]["source_uri"] == "PROJECT.md"
    # All hits must carry hardcoded "blob" — the ingester schema has no
    # source_type column, so we normalize on egress.
    assert all(h["source_type"] == "blob" for h in hits)


def test_pgvector_search_sql_does_not_reference_source_type(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Regression: SELECT must match ingester schema (no source_type column)."""
    captured: dict = {}
    _install_fake_psycopg(monkeypatch, [], captured)
    _stub_embed(monkeypatch)

    _pgvector_search(_base_rec(), "q", 3)

    sql = captured["sql"]
    assert "source_type" not in sql
    assert "source_uri" in sql
    assert "content" in sql
    assert "embedding <=>" in sql
    # Params: (vector, ns, kb_name, vector, top_k)
    params = captured["params"]
    assert len(params) == 5
    assert params[1] == "default"
    assert params[2] == "kognition-docs"
    assert params[4] == 3


def test_pgvector_search_respects_top_k_from_retrieval(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict = {}
    _install_fake_psycopg(monkeypatch, [], captured)
    _stub_embed(monkeypatch)

    rec = _base_rec()
    rec["retrieval"] = {"topK": 7}
    _pgvector_search(rec, "q", 3)

    assert captured["params"][4] == 7


def test_pgvector_search_drops_rows_below_min_score(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict = {}
    rows = [
        ("a", "keep", 0.6),
        ("b", "drop", 0.1),
        ("c", "keep", 0.5),
    ]
    _install_fake_psycopg(monkeypatch, rows, captured)
    _stub_embed(monkeypatch)

    rec = _base_rec()
    rec["retrieval"] = {"minScore": 0.4}
    hits = _pgvector_search(rec, "q", 5)

    assert [h["source_uri"] for h in hits] == ["a", "c"]


def test_pgvector_search_rejects_unsafe_table_name(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict = {}
    _install_fake_psycopg(monkeypatch, [("x", "y", 0.9)], captured)
    _stub_embed(monkeypatch)

    rec = _base_rec()
    rec["vectorStore"]["table"] = "kb; DROP TABLE users;--"
    hits = _pgvector_search(rec, "q", 5)

    assert hits == []
    # SQL must never have been issued.
    assert "sql" not in captured


def test_pgvector_search_returns_empty_on_query_exception(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import types

    fake_psycopg = types.ModuleType("psycopg")

    def _boom(*_args: object, **_kwargs: object) -> None:
        raise RuntimeError("connection refused")

    fake_psycopg.connect = _boom  # type: ignore[attr-defined]
    fake_pgvector_psycopg = types.ModuleType("pgvector.psycopg")
    fake_pgvector_psycopg.register_vector = lambda _c: None  # type: ignore[attr-defined]

    monkeypatch.setitem(sys.modules, "psycopg", fake_psycopg)
    monkeypatch.setitem(sys.modules, "pgvector.psycopg", fake_pgvector_psycopg)
    _stub_embed(monkeypatch)

    hits = _pgvector_search(_base_rec(), "q", 5)
    assert hits == []


def test_pgvector_search_returns_empty_when_embedding_fails(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict = {}
    _install_fake_psycopg(monkeypatch, [("x", "y", 0.9)], captured)

    import tools as tools_mod

    def _explode(_emb: dict, _q: str) -> list[float]:
        raise RuntimeError("no embedding backend")

    monkeypatch.setattr(tools_mod, "_embed_query", _explode)

    hits = _pgvector_search(_base_rec(), "q", 5)
    assert hits == []
    # Cursor must not have been touched — embedding failed before query.
    assert "sql" not in captured


def test_pgvector_search_handles_null_score(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict = {}
    _install_fake_psycopg(monkeypatch, [("a", "body", None)], captured)
    _stub_embed(monkeypatch)

    hits = _pgvector_search(_base_rec(), "q", 5)
    assert len(hits) == 1
    assert hits[0]["score"] is None
