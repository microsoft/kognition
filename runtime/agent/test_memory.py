# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Pytest coverage for the agent memory tools in :mod:`memory`.

Run via::

    pip install -r requirements.txt pytest
    pytest runtime/agent

The module imports langchain_core at top level, so tests are skipped when that
dependency is not installed.
"""

from __future__ import annotations

import json
import sys
import uuid
from pathlib import Path

import pytest

pytest.importorskip("langchain_core")
pytest.importorskip("pydantic")

# Allow ``import memory`` when pytest is run from the repo root.
sys.path.insert(0, str(Path(__file__).parent))

import httpx  # noqa: E402

import memory as memory_mod  # noqa: E402
from config import AgentConfig  # noqa: E402


class _Resp:
    def __init__(self, status_code: int, payload=None, text: str = "") -> None:
        self.status_code = status_code
        self._payload = payload
        self.text = text

    def json(self):
        if self._payload is None:
            raise ValueError("no json body")
        return self._payload


def _patch_post(monkeypatch, captured: dict, resp=None, exc: Exception | None = None):
    def _post(url, json=None, timeout=None, headers=None):  # noqa: A002 - mirror httpx.post signature
        captured["url"] = url
        captured["json"] = json
        captured["headers"] = headers
        if exc is not None:
            raise exc
        return resp

    monkeypatch.setattr(memory_mod.httpx, "post", _post)


def _config(**overrides) -> AgentConfig:
    base = dict(
        agent_name="agent-a",
        memory_enabled=True,
        memory_service_url="http://mem:8090",
        memory_volume="vol-1",
    )
    base.update(overrides)
    return AgentConfig(**base)


def _tools_by_name(config: AgentConfig) -> dict:
    return {t.name: t for t in memory_mod.build_memory_tools(config)}


# --------------------------------------------------------------------------- #
# build_memory_tools gating
# --------------------------------------------------------------------------- #


def test_disabled_when_memory_off() -> None:
    assert memory_mod.build_memory_tools(_config(memory_enabled=False)) == []


def test_disabled_when_no_url() -> None:
    assert memory_mod.build_memory_tools(_config(memory_service_url="")) == []


def test_disabled_when_no_volume() -> None:
    cfg = _config(memory_volume="", agent_name="")
    assert memory_mod.build_memory_tools(cfg) == []


def test_volume_falls_back_to_agent_name() -> None:
    assert memory_mod._resolve_volume(_config(memory_volume="", agent_name="agent-a")) == "agent-a"


def test_builds_both_tools() -> None:
    names = set(_tools_by_name(_config()))
    assert names == {"memory_recall", "memory_store"}


# --------------------------------------------------------------------------- #
# memory_recall
# --------------------------------------------------------------------------- #

def test_store_supplies_stable_gateway_invocation_id_until_terminal(monkeypatch) -> None:
    attempts = []

    def post(url, json=None, timeout=None, headers=None):
        attempts.append(dict(headers))
        if len(attempts) == 1:
            return _Resp(202, {"runtime_approval_state": "Pending"})
        return _Resp(201, {"id": "stored-record"})

    monkeypatch.setattr(memory_mod.httpx, "post", post)
    store = _tools_by_name(_config())["memory_store"]
    with memory_mod.memory_runtime_capability("fixture-session-capability"):
        assert json.loads(store.invoke({"content": "note", "tag": "fixture"}))["runtime_approval_state"] == "Pending"
        assert store.invoke({"content": "note", "tag": "fixture"}) == "Stored."
        assert store.invoke({"content": "note", "tag": "fixture"}) == "Stored."
    invocation_ids = [attempt.get("X-Kognition-Memory-Invocation-Id", "") for attempt in attempts]
    assert uuid.UUID(invocation_ids[0]).version == 4
    assert invocation_ids[0] == invocation_ids[1]
    assert invocation_ids[2] != invocation_ids[1]
    assert all(attempt["Authorization"] == "Bearer fixture-session-capability" for attempt in attempts)

@pytest.mark.parametrize("outcome", ["pending", "unavailable", "http_failure", "transport_failure", "mismatched_id"])
def test_store_preserves_identity_for_unconfirmed_gateway_writes(monkeypatch, outcome) -> None:
    attempts = []

    def post(url, json=None, timeout=None, headers=None):
        attempts.append(dict(headers))
        if outcome == "transport_failure":
            raise httpx.ReadTimeout("fixture timeout")
        if outcome == "http_failure":
            return _Resp(503, {"error": "fixture unknown outcome"})
        if outcome == "mismatched_id":
            return _Resp(202, {"runtime_approval_state": "Pending", "invocation_id": "unrelated"})
        return _Resp(202, {"runtime_approval_state": outcome.title()})

    monkeypatch.setattr(memory_mod.httpx, "post", post)
    store = _tools_by_name(_config())["memory_store"]
    with memory_mod.memory_runtime_capability("fixture-session-capability"):
        for _ in range(2):
            result = json.loads(store.invoke({"content": "note"}))
            assert result["runtime_approval_state"] == ("Pending" if outcome == "pending" else "Unavailable")
    assert attempts[0]["X-Kognition-Memory-Invocation-Id"] == attempts[1]["X-Kognition-Memory-Invocation-Id"]


def test_store_does_not_share_pending_identity_across_payloads_or_sessions(monkeypatch) -> None:
    attempts = []

    def post(url, json=None, timeout=None, headers=None):
        attempts.append(dict(headers))
        return _Resp(202, {"runtime_approval_state": "Pending"})

    monkeypatch.setattr(memory_mod.httpx, "post", post)
    store = _tools_by_name(_config())["memory_store"]
    with memory_mod.memory_runtime_capability("fixture-session-one"):
        store.invoke({"content": "note"})
        store.invoke({"content": "different"})
    with memory_mod.memory_runtime_capability("fixture-session-two"):
        store.invoke({"content": "note"})
    assert len({attempt["X-Kognition-Memory-Invocation-Id"] for attempt in attempts}) == 3


def test_memory_requests_propagate_runtime_trace_headers(monkeypatch) -> None:
    captured = {}
    _patch_post(monkeypatch, captured, _Resp(201, {"id": "stored-record"}))
    monkeypatch.setattr(memory_mod.runtime_tracing, "inject_trace_headers", lambda: {"traceparent": "fixture-trace"})
    tools = _tools_by_name(_config())
    with memory_mod.memory_runtime_capability("fixture-session-capability"):
        assert tools["memory_store"].invoke({"content": "note"}) == "Stored."
        assert captured["headers"]["traceparent"] == "fixture-trace"
        tools["memory_recall"].invoke({"query": "note"})
        assert captured["headers"] == {"Authorization": "Bearer fixture-session-capability", "traceparent": "fixture-trace"}


def test_recall_no_results_returns_no_memories(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(200, payload=[]))
    recall = _tools_by_name(_config())["memory_recall"]

    out = recall.invoke({"query": "shared policy"})
    assert out == "No relevant memories found."
    assert captured["url"] == "http://mem:8090/v1/memory/search"
    assert captured["json"]["volume_handle"] == "vol-1"
    assert captured["json"]["query"] == "shared policy"
    assert captured["json"]["min_score"] == 0.5
    assert "metadata" not in captured["json"]
    assert captured["headers"] == {}


def test_memory_capability_is_forwarded_only_inside_scope(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(200, payload=[]))
    recall = _tools_by_name(_config())["memory_recall"]

    with memory_mod.memory_runtime_capability("capability-token"):
        recall.invoke({"query": "policy"})
    assert captured["headers"] == {"Authorization": "Bearer capability-token"}

    captured.clear()
    recall.invoke({"query": "policy"})
    assert captured["headers"] == {}


def test_recall_returns_memories_and_honors_tag(monkeypatch) -> None:
    captured: dict = {}
    payload = [
        {
            "record": {"content": "Every release must include an SBOM.", "metadata": {"tag": "shared"}},
            "score": 0.91,
        }
    ]
    _patch_post(monkeypatch, captured, _Resp(200, payload=payload))
    recall = _tools_by_name(_config())["memory_recall"]

    out = recall.invoke({"query": "shared policy", "k": 3, "tag": "shared"})
    parsed = json.loads(out)
    assert parsed[0]["content"] == "Every release must include an SBOM."
    assert parsed[0]["tag"] == "shared"
    # T-0323: the tag is threaded through as a metadata filter.
    assert captured["json"]["metadata"] == {"tag": "shared"}
    assert captured["json"]["top_k"] == 3


def test_recall_fail_closed_on_400(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(400, text="text query requires a configured embedder"))
    recall = _tools_by_name(_config())["memory_recall"]

    out = recall.invoke({"query": "anything"})
    assert "unavailable" in out.lower()
    assert "No relevant memories found." not in out


def test_recall_network_error(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, exc=httpx.ConnectError("refused"))
    recall = _tools_by_name(_config())["memory_recall"]

    out = recall.invoke({"query": "anything"})
    assert out.startswith("Memory recall failed:")


def test_recall_server_error(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(500))
    recall = _tools_by_name(_config())["memory_recall"]

    out = recall.invoke({"query": "anything"})
    assert out == "Memory recall failed: HTTP 500"


# --------------------------------------------------------------------------- #
# memory_store
# --------------------------------------------------------------------------- #


def test_store_posts_content_and_tag(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(201, payload={"id": "r1"}))
    store = _tools_by_name(_config())["memory_store"]

    out = store.invoke({"content": "remember this", "tag": "shared"})
    assert out == "Stored."
    assert captured["url"] == "http://mem:8090/v1/memory/store"
    assert captured["json"]["content"] == "remember this"
    assert captured["json"]["metadata"] == {"tag": "shared"}


def test_store_empty_content(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(201))
    store = _tools_by_name(_config())["memory_store"]

    out = store.invoke({"content": "   "})
    assert "Nothing to store" in out
    assert captured == {}  # no HTTP call made


def test_store_unconfirmed_acceptance_is_not_reported_as_stored(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(202, payload={"accepted": True}))
    store = _tools_by_name(_config())["memory_store"]

    result = json.loads(store.invoke({"content": "requested note"}))
    assert result["runtime_approval_state"] == "Unavailable"
    assert captured["json"]["content"] == "requested note"


def test_store_server_error(monkeypatch) -> None:
    captured: dict = {}
    _patch_post(monkeypatch, captured, _Resp(503))
    store = _tools_by_name(_config())["memory_store"]

    out = store.invoke({"content": "x"})
    assert out == "Memory store failed: HTTP 503"
