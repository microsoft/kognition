# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Tests for the FastAPI handlers in :mod:`main`.

These cover the session-scoping rules that keep one bad invocation from
bricking the shared agent Deployment for every subsequent AgentInstance.
"""
from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

import pytest

pytest.importorskip("fastapi")
pytest.importorskip("langchain_core")
pytest.importorskip("pydantic")

from fastapi.testclient import TestClient  # noqa: E402
from langchain_core.messages import AIMessage  # noqa: E402
from langgraph.graph import END, StateGraph  # noqa: E402
from opentelemetry.sdk.trace import TracerProvider  # noqa: E402
from opentelemetry.sdk.trace.export import SimpleSpanProcessor  # noqa: E402
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter  # noqa: E402

sys.path.insert(0, str(Path(__file__).parent))

import main  # noqa: E402
from main import RuntimePhase  # noqa: E402
from graph import AgentState  # noqa: E402
import providers  # noqa: E402
import tools  # noqa: E402


@pytest.fixture
def client(monkeypatch: pytest.MonkeyPatch):
    """A TestClient with a fresh state and a stub graph so /invoke is allowed."""
    main.state.__init__()
    main.state.phase = RuntimePhase.READY
    main.agent_graph = object()

    # Stub out the background runner so /invoke doesn't try to call a real LLM.
    async def _noop(*args, **kwargs):  # pragma: no cover - trivial stub
        return None

    monkeypatch.setattr(main, "_run_agent", _noop)
    return TestClient(main.app)


@pytest.fixture
def external_binding(monkeypatch):
    configuration = {
        "KOGNITION_NAMESPACE": "tenant",
        "KOGNITION_AGENT_NAME": "agent",
        "KOGNITION_MODEL_GATEWAY_URL": "http://model:8093",
        "KOGNITION_SYSTEM_PROMPT": "Configured prompt",
    }
    monkeypatch.setattr(main, "external_runtime", True)
    monkeypatch.setattr(main, "loaded_configuration", configuration)
    monkeypatch.setattr(main.config, "runtime_auth_token", "test-runtime-token")
    return configuration


def test_external_status_rejects_lost_session(client, external_binding):
    headers = {"Authorization": "Bearer test-runtime-token"}
    response = client.get("/status?session_id=lost-session", headers=headers)
    assert response.status_code == 404
    main.state.current_session_id = "different-session"
    main.state.output = "Foreign result"
    response = client.get("/status?session_id=lost-session", headers=headers)
    assert response.status_code == 404
    assert "Foreign result" not in response.text
    assert client.get("/status", headers=headers).status_code == 200


def test_external_configuration_requires_authentication(client, external_binding):
    response = client.post("/configuration-check", json=external_binding)
    assert response.status_code == 401


@pytest.mark.parametrize("key", ["KOGNITION_NAMESPACE", "KOGNITION_AGENT_NAME", "KOGNITION_MODEL_GATEWAY_URL", "KOGNITION_SYSTEM_PROMPT"])
def test_external_configuration_rejects_different_runtime(client, external_binding, key):
    headers = {"Authorization": "Bearer test-runtime-token"}
    assert client.post("/configuration-check", json=external_binding, headers=headers).status_code == 204
    mismatched = dict(external_binding, **{key: "different"})
    response = client.post("/configuration-check", json=mismatched, headers=headers)
    assert response.status_code == 412
    assert "Configured prompt" not in response.text


@pytest.mark.parametrize("missing", ["runtime_configuration", "session_id", "model_session_capability"])
def test_external_invoke_requires_bound_session(client, external_binding, missing):
    payload = {"message": "hello", "session_id": "session-1", "model_session_capability": "test-capability",
               "runtime_configuration": json.dumps(external_binding)}
    del payload[missing]
    response = client.post("/invoke", json=payload, headers={"Authorization": "Bearer test-runtime-token"})
    assert response.status_code == 412
    assert main.state.phase == RuntimePhase.READY
    assert main.state.current_session_id == ""


def test_external_invoke_accepts_bound_session(client, external_binding):
    response = client.post("/invoke", json={"message": "hello", "session_id": "session-1",
        "model_session_capability": "test-capability", "runtime_configuration": json.dumps(external_binding)},
        headers={"Authorization": "Bearer test-runtime-token"})
    assert response.status_code == 202
    assert main.state.current_session_id == "session-1"


def test_status_foreign_session_after_failed_returns_running_placeholder(client):
    """A FAILED prior session must not bleed into a foreign caller's /status."""
    main.state.current_session_id = "old-session"
    main.state.phase = RuntimePhase.FAILED
    main.state.error = "model timeout"
    main.state.output = "leftover bytes"

    resp = client.get("/status", params={"session_id": "new-session"})

    assert resp.status_code == 200
    body = resp.json()
    assert body["phase"] == "Running"
    assert body["session_id"] == "new-session"
    assert body["output"] == ""
    assert body["error"] == ""


def test_status_same_session_after_failed_surfaces_failure(client):
    """The session that actually failed still sees the FAILED phase."""
    main.state.current_session_id = "doomed"
    main.state.phase = RuntimePhase.FAILED
    main.state.error = "boom"

    resp = client.get("/status", params={"session_id": "doomed"})

    body = resp.json()
    assert body["phase"] == "Failed"
    assert body["error"] == "boom"


def test_invoke_new_session_after_failed_resets_runtime(client):
    """A fresh session_id must be accepted even when the pod is in FAILED."""
    main.state.current_session_id = "old-session"
    main.state.phase = RuntimePhase.FAILED
    main.state.error = "prior crash"

    resp = client.post(
        "/invoke",
        json={"message": "hello", "session_id": "fresh-session"},
    )

    assert resp.status_code == 202
    body = resp.json()
    assert body["accepted"] is True
    assert body["session_id"] == "fresh-session"
    assert main.state.phase == RuntimePhase.RUNNING
    assert main.state.error == ""


@pytest.mark.asyncio
async def test_run_agent_span_is_child_of_incoming_traceparent(monkeypatch: pytest.MonkeyPatch):
    main.state.__init__()
    main.state.phase = RuntimePhase.RUNNING
    main.state.current_session_id = "session-1"
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(main.runtime_tracing, "_tracer", provider.get_tracer("test-runtime"))

    class FakeGraph:
        def invoke(self, _state):
            return {
                "messages": [AIMessage(content="done")],
                "tool_calls_count": 0,
                "tool_calls_detail": [],
                "token_usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
            }

    monkeypatch.setattr(main, "agent_graph", FakeGraph())

    await main._run_agent(
        "hello",
        "conversation-1",
        "00-11111111111111111111111111111111-2222222222222222-01",
    )

    spans = exporter.get_finished_spans()
    assert len(spans) == 1
    span = spans[0]
    assert span.name == "kognition.runtime.invoke"
    assert not main.state.conversation_trace_contexts
    assert f"{span.context.trace_id:032x}" == "11111111111111111111111111111111"
    assert f"{span.parent.span_id:016x}" == "2222222222222222"
    assert span.attributes["kognition.session.id"] == "session-1"
    assert span.attributes["kognition.conversation.id"] == "conversation-1"
    assert span.attributes["gen_ai.usage.input_tokens"] == 3
    assert main.state.phase == RuntimePhase.COMPLETED


@pytest.mark.asyncio
async def test_run_agent_forwards_tool_session_capability_to_worker_thread(monkeypatch: pytest.MonkeyPatch):
    main.state.__init__()
    main.state.phase = RuntimePhase.RUNNING
    main.state.current_session_id = "session-tool-capability"
    observed: dict[str, str] = {}

    class FakeGraph:
        def invoke(self, _state):
            observed["capability"] = tools._tool_session_capability.get()
            return {
                "messages": [AIMessage(content="done")],
                "tool_calls_count": 0,
                "tool_calls_detail": [],
                "token_usage": {},
            }

    monkeypatch.setattr(main, "agent_graph", FakeGraph())
    await main._run_agent("hello", "", tool_session_capability="session-capability")
    assert observed["capability"] == "session-capability"


@pytest.mark.asyncio
async def test_run_agent_forwards_delegation_session_capability_to_worker_thread(monkeypatch: pytest.MonkeyPatch):
    observed: dict[str, str] = {}

    def fake_invoke(_state: dict):
        from tools import _agent_delegation_session_capability

        observed["capability"] = _agent_delegation_session_capability.get()
        return {"messages": [AIMessage(content="done")], "token_usage": {}, "tool_calls_count": 0, "tool_calls_detail": []}

    monkeypatch.setattr(main, "agent_graph", type("Graph", (), {"invoke": staticmethod(fake_invoke)})())
    await main._run_agent("hello", "", agent_delegation_session_capability="delegation-session")
    assert observed["capability"] == "delegation-session"
    assert main.state.output == "done"


@pytest.mark.asyncio
async def test_run_agent_forwards_and_resets_model_session_capability(monkeypatch: pytest.MonkeyPatch):
    main.state.__init__()
    main.state.phase = RuntimePhase.RUNNING
    main.state.current_session_id = "session-model-capability"
    observed: dict[str, str] = {}

    class FakeGraph:
        def invoke(self, _state):
            observed["capability"] = providers._model_session_capability.get()
            return {
                "messages": [AIMessage(content="done")],
                "tool_calls_count": 0,
                "tool_calls_detail": [],
                "token_usage": {},
            }

    monkeypatch.setattr(main, "agent_graph", FakeGraph())
    await main._run_agent("hello", "", model_session_capability="model-session-capability")
    assert observed["capability"] == "model-session-capability"
    assert providers._model_session_capability.get() == ""


@pytest.mark.asyncio
async def test_run_agent_groups_turns_under_conversation_root(monkeypatch: pytest.MonkeyPatch):
    main.state.__init__()
    main.state.phase = RuntimePhase.RUNNING
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(main.runtime_tracing, "_tracer", provider.get_tracer("test-runtime"))

    class FakeGraph:
        def invoke(self, state):
            message_count = len(state["messages"])
            return {
                "messages": [AIMessage(content=f"turn saw {message_count} messages")],
                "tool_calls_count": 0,
                "tool_calls_detail": [],
                "token_usage": {"prompt_tokens": message_count, "completion_tokens": 1, "total_tokens": message_count + 1},
            }

    monkeypatch.setattr(main, "agent_graph", FakeGraph())

    main.state.current_session_id = "session-1"
    await main._run_agent("hello", "conversation-1")
    main.state.current_session_id = "session-2"
    await main._run_agent("again", "conversation-1")

    spans = exporter.get_finished_spans()
    conversation_spans = [span for span in spans if span.name == "kognition.runtime.conversation"]
    invoke_spans = [span for span in spans if span.name == "kognition.runtime.invoke"]

    assert len(conversation_spans) == 1
    assert len(invoke_spans) == 2
    root = conversation_spans[0]
    assert root.parent is None
    assert root.attributes["kognition.conversation.id"] == "conversation-1"
    assert all(span.context.trace_id == root.context.trace_id for span in invoke_spans)
    assert {span.attributes["kognition.session.id"] for span in invoke_spans} == {"session-1", "session-2"}
    assert all(span.attributes["kognition.conversation.id"] == "conversation-1" for span in invoke_spans)
    assert all(span.parent is not None and span.parent.span_id == root.context.span_id for span in invoke_spans)


@pytest.mark.asyncio
async def test_run_agent_keeps_different_conversations_in_different_traces(monkeypatch: pytest.MonkeyPatch):
    main.state.__init__()
    main.state.phase = RuntimePhase.RUNNING
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(main.runtime_tracing, "_tracer", provider.get_tracer("test-runtime"))

    class FakeGraph:
        def invoke(self, _state):
            return {
                "messages": [AIMessage(content="done")],
                "tool_calls_count": 0,
                "tool_calls_detail": [],
                "token_usage": {},
            }

    monkeypatch.setattr(main, "agent_graph", FakeGraph())

    main.state.current_session_id = "session-1"
    await main._run_agent("hello", "conversation-1")
    main.state.current_session_id = "session-2"
    await main._run_agent("hello", "conversation-2")

    root_by_conversation = {
        span.attributes["kognition.conversation.id"]: span
        for span in exporter.get_finished_spans()
        if span.name == "kognition.runtime.conversation"
    }
    invoke_by_conversation = {
        span.attributes["kognition.conversation.id"]: span
        for span in exporter.get_finished_spans()
        if span.name == "kognition.runtime.invoke"
    }

    assert set(root_by_conversation) == {"conversation-1", "conversation-2"}
    assert root_by_conversation["conversation-1"].context.trace_id != root_by_conversation["conversation-2"].context.trace_id
    for conversation_id, invoke_span in invoke_by_conversation.items():
        root = root_by_conversation[conversation_id]
        assert invoke_span.context.trace_id == root.context.trace_id
        assert invoke_span.parent is not None
        assert invoke_span.parent.span_id == root.context.span_id


def test_invoke_propagates_spiffe_uri_to_status(client):
    """The controller-provided workload URI remains scoped to its session."""
    spiffe_id = "spiffe://kognition.dev/workspace/team-a/agent/researcher"
    resp = client.post(
        "/invoke",
        json={"message": "hello", "session_id": "identity-session", "spiffe_id": spiffe_id},
    )

    assert resp.status_code == 202
    assert main.state.current_spiffe_id == spiffe_id
    status = client.get("/status", params={"session_id": "identity-session"})
    assert status.status_code == 200
    assert status.json()["spiffe_id"] == spiffe_id


def test_runtime_endpoints_require_controller_token_when_configured(client, monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(main.config, "runtime_auth_token", "controller-token")
    invoke = client.post("/invoke", json={"message": "hello", "session_id": "authorized-session"})
    assert invoke.status_code == 401
    status = client.get("/status")
    assert status.status_code == 401
    cancel = client.post("/cancel", json={"session_id": "authorized-session"})
    assert cancel.status_code == 401
    invoke = client.post(
        "/invoke",
        headers={"Authorization": "Bearer controller-token"},
        json={"message": "hello", "session_id": "authorized-session"},
    )
    assert invoke.status_code == 202
    status = client.get("/status", headers={"Authorization": "Bearer controller-token"})
    assert status.status_code == 200


def test_cancel_signals_matching_running_session(client):
    class FakeTask:
        def __init__(self):
            self.cancelled = False

        def done(self):
            return False

        def cancel(self):
            self.cancelled = True
            main.state.phase = RuntimePhase.FAILED
            main.state.completed_sessions["quota-session"] = {
                "phase": RuntimePhase.FAILED.value,
            }

        def __await__(self):
            async def completed():
                return None

            return completed().__await__()

    task = FakeTask()
    main.state.current_session_id = "quota-session"
    main.state.phase = RuntimePhase.RUNNING
    main.state.current_task = task
    main.state.current_task_cancellable = True

    response = client.post(
        "/cancel",
        json={"session_id": "quota-session", "reason": "token budget exceeded"},
    )

    assert response.status_code == 200
    assert response.json() == {"accepted": True, "session_id": "quota-session"}
    assert task.cancelled is True


def test_cancel_rejects_foreign_session(client):
    main.state.current_session_id = "active-session"
    main.state.phase = RuntimePhase.RUNNING

    response = client.post("/cancel", json={"session_id": "foreign-session"})

    assert response.status_code == 404


def test_cancel_rejects_non_cancellable_runtime(client):
    class RunningTask:
        def done(self):
            return False

    main.state.current_session_id = "fallback-session"
    main.state.phase = RuntimePhase.RUNNING
    main.state.current_task = RunningTask()
    main.state.current_task_cancellable = False

    response = client.post("/cancel", json={"session_id": "fallback-session"})

    assert response.status_code == 409
    assert "does not support cooperative cancellation" in response.json()["detail"]


def test_invoke_resets_usage_for_new_session(client):
    main.state.current_session_id = "completed-session"
    main.state.phase = RuntimePhase.COMPLETED
    main.state.token_usage = {"prompt_tokens": 80, "completion_tokens": 20, "total_tokens": 100}
    main.state.tool_calls = 3

    response = client.post(
        "/invoke",
        json={"message": "hello", "session_id": "new-session"},
    )

    assert response.status_code == 202
    assert main.state.token_usage == {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
    assert main.state.tool_calls == 0


@pytest.mark.asyncio
async def test_cancelled_run_archives_failed_session(monkeypatch: pytest.MonkeyPatch):
    main.state.__init__()
    main.state.phase = RuntimePhase.RUNNING
    main.state.current_session_id = "quota-session"
    started = asyncio.Event()

    class BlockingGraph:
        async def astream(self, _state, stream_mode):
            started.set()
            await asyncio.Event().wait()
            yield {}

    monkeypatch.setattr(main, "agent_graph", BlockingGraph())
    task = asyncio.create_task(main._run_agent("hello", ""))
    main.state.current_task = task
    main.state.current_task_cancellable = True
    await started.wait()

    response = await main.cancel(
        main.CancelRequest(session_id="quota-session", reason="token budget exceeded")
    )
    assert response.accepted is True
    with pytest.raises(asyncio.CancelledError):
        await task

    assert main.state.phase == RuntimePhase.FAILED
    assert main.state.error == "session cancelled by controller"
    assert main.state.completed_sessions["quota-session"]["phase"] == "Failed"


@pytest.mark.asyncio
async def test_running_usage_is_visible_and_cancel_prevents_later_graph_node(monkeypatch: pytest.MonkeyPatch):
    main.state.__init__()
    main.state.phase = RuntimePhase.READY
    second_node_started = asyncio.Event()
    release_second_node = asyncio.Event()
    later_node_ran = False

    async def model_turn(_state):
        return {
            "messages": [AIMessage(content="first turn")],
            "token_usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6},
        }

    async def blocking_tool(_state):
        second_node_started.set()
        await release_second_node.wait()
        return {}

    async def later_model_turn(_state):
        nonlocal later_node_ran
        later_node_ran = True
        return {"messages": [AIMessage(content="too late")]}

    builder = StateGraph(AgentState)
    builder.add_node("model_turn", model_turn)
    builder.add_node("blocking_tool", blocking_tool)
    builder.add_node("later_model_turn", later_model_turn)
    builder.set_entry_point("model_turn")
    builder.add_edge("model_turn", "blocking_tool")
    builder.add_edge("blocking_tool", "later_model_turn")
    builder.add_edge("later_model_turn", END)
    monkeypatch.setattr(main, "agent_graph", builder.compile())

    ack = await main.invoke(
        main.InvokeRequest(message="hello", session_id="quota-session"),
        authorization=None,
        traceparent="",
        tracestate="",
    )
    assert ack.accepted is True
    progress = asyncio.create_task(second_node_started.wait())
    done, _ = await asyncio.wait(
        {progress, main.state.current_task}, timeout=1, return_when=asyncio.FIRST_COMPLETED
    )
    assert progress in done, main.state.error

    running = main.status(session_id="quota-session")
    assert running.phase == RuntimePhase.RUNNING.value
    assert running.token_usage == {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}

    cancelled = await main.cancel(main.CancelRequest(session_id="quota-session", reason="token budget exceeded"))
    assert cancelled.accepted is True
    assert later_node_ran is False


@pytest.mark.asyncio
async def test_cancel_ack_uses_archived_session_after_runtime_handoff():
    main.state.__init__()
    main.state.phase = RuntimePhase.RUNNING
    main.state.current_session_id = "quota-session"

    class HandoffTask:
        def done(self):
            return False

        def cancel(self):
            main.state.completed_sessions["quota-session"] = {
                "phase": RuntimePhase.FAILED.value,
            }

        def __await__(self):
            async def handoff():
                main.state.current_session_id = "next-session"
                main.state.phase = RuntimePhase.RUNNING

            return handoff().__await__()

    main.state.current_task = HandoffTask()
    main.state.current_task_cancellable = True

    response = await main.cancel(
        main.CancelRequest(session_id="quota-session", reason="token budget exceeded")
    )

    assert response.accepted is True
    assert response.session_id == "quota-session"
    assert main.state.current_session_id == "next-session"


def test_invoke_same_session_after_failed_returns_503(client):
    """Retrying the dead session is still rejected so the controller gives up."""
    main.state.current_session_id = "doomed"
    main.state.phase = RuntimePhase.FAILED
    main.state.error = "boom"

    resp = client.post(
        "/invoke",
        json={"message": "retry", "session_id": "doomed"},
    )

    assert resp.status_code == 503
    assert "boom" in resp.json()["detail"]


def test_invoke_busy_with_other_session_returns_409(client):
    """While one session is RUNNING, a different session_id must 409."""
    main.state.current_session_id = "in-flight"
    main.state.phase = RuntimePhase.RUNNING

    resp = client.post(
        "/invoke",
        json={"message": "me too", "session_id": "newcomer"},
    )

    assert resp.status_code == 409


def test_invoke_same_session_running_is_idempotent(client):
    """Reconciler retries with the same session_id must not double-invoke."""
    main.state.current_session_id = "in-flight"
    main.state.phase = RuntimePhase.RUNNING
    main.state.invocation_count = 1

    resp = client.post(
        "/invoke",
        json={"message": "retry", "session_id": "in-flight"},
    )

    assert resp.status_code == 202
    body = resp.json()
    assert body["accepted"] is False
    assert body["invocation_count"] == 1


@pytest.mark.parametrize("archived_phase", [RuntimePhase.COMPLETED, RuntimePhase.FAILED])
@pytest.mark.parametrize("current_phase", [RuntimePhase.RUNNING, RuntimePhase.COMPLETED, RuntimePhase.FAILED])
def test_external_invoke_archived_session_does_not_execute_again(client, external_binding, archived_phase, current_phase):
    headers = {"Authorization": "Bearer test-runtime-token"}
    main.state.current_session_id = "worker-a"
    main.state.phase = archived_phase
    main.state.output = "retained output"
    main.state.error = "retained failure" if archived_phase == RuntimePhase.FAILED else ""
    main.state.invocation_count = 1
    main._archive_completed_session("worker-a")
    archived_before = client.get("/status?session_id=worker-a", headers=headers).json()

    main.state.current_session_id = "worker-b"
    main.state.phase = current_phase
    main.state.output = "newer output"
    main.state.error = "newer failure"
    main.state.invocation_count = 2
    current_before = client.get("/status?session_id=worker-b", headers=headers).json()

    response = client.post("/invoke", headers=headers, json={
        "message": "retry", "session_id": "worker-a",
        "model_session_capability": "test-capability",
        "runtime_configuration": json.dumps(external_binding),
    })

    if archived_phase == RuntimePhase.FAILED:
        assert response.status_code == 503
        assert "retained failure" in response.json()["detail"]
    else:
        assert response.status_code == 202
        assert response.json()["accepted"] is False
        assert response.json()["phase"] == "Completed"
        assert response.json()["session_id"] == "worker-a"
        assert response.json()["invocation_count"] == 1
    for session_id, before in [("worker-b", current_before), ("worker-a", archived_before)]:
        after = client.get("/status", params={"session_id": session_id}, headers=headers).json()
        before.pop("uptime_seconds", None)
        after.pop("uptime_seconds", None)
        assert after == before


def test_status_returns_archived_completion_for_foreign_session(client):
    """After a session completes and another takes over, /status on the older
    session_id must return the archived result rather than the placeholder."""
    # Archive a previously completed session.
    main.state.current_session_id = "worker-a"
    main.state.phase = RuntimePhase.COMPLETED
    main.state.output = "hello from A"
    main.state.token_usage = {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
    main.state.tool_calls = 2
    main.state.tool_calls_detail = [{"name": "t1"}]
    main.state.invocation_count = 1
    main._archive_completed_session("worker-a")

    # A newer session takes over the runtime.
    main.state.current_session_id = "worker-b"
    main.state.phase = RuntimePhase.RUNNING
    main.state.output = ""
    main.state.error = ""

    resp = client.get("/status", params={"session_id": "worker-a"})

    assert resp.status_code == 200
    body = resp.json()
    assert body["phase"] == "Completed"
    assert body["output"] == "hello from A"
    assert body["session_id"] == "worker-a"
    assert body["token_usage"]["total_tokens"] == 15
    assert body["tool_calls"] == 2
    assert body["tool_calls_detail"] == [{"name": "t1"}]


def test_status_returns_archived_failure_for_foreign_session(client):
    """Archived FAILED sessions are also served back on later /status polls."""
    main.state.current_session_id = "worker-x"
    main.state.phase = RuntimePhase.FAILED
    main.state.output = ""
    main.state.error = "model refused"
    main.state.invocation_count = 1
    main._archive_completed_session("worker-x")

    main.state.current_session_id = "worker-y"
    main.state.phase = RuntimePhase.RUNNING
    main.state.error = ""

    resp = client.get("/status", params={"session_id": "worker-x"})

    body = resp.json()
    assert body["phase"] == "Failed"
    assert body["error"] == "model refused"
    assert body["session_id"] == "worker-x"


def test_status_foreign_unknown_session_still_returns_placeholder(client):
    """Sessions the runtime has never seen fall through to the polling placeholder."""
    main.state.current_session_id = "in-flight"
    main.state.phase = RuntimePhase.RUNNING

    resp = client.get("/status", params={"session_id": "someone-else"})

    body = resp.json()
    assert body["phase"] == "Running"
    assert body["output"] == ""
    assert body["session_id"] == "someone-else"
