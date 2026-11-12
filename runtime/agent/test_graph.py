# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import asyncio
import sys
import threading
from pathlib import Path

import pytest

pytest.importorskip("langchain_core")
pytest.importorskip("langgraph")

from langchain_core.messages import AIMessage, HumanMessage  # noqa: E402

sys.path.insert(0, str(Path(__file__).parent))

import graph  # noqa: E402
import memory  # noqa: E402
import providers  # noqa: E402
import tools  # noqa: E402
from config import AgentConfig  # noqa: E402


class FakeModel:
    def __init__(self, outcomes):
        self.outcomes = list(outcomes)
        self.calls = 0

    def invoke(self, _messages):
        self.calls += 1
        outcome = self.outcomes.pop(0)
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    def bind_tools(self, _tools):
        return self


class StatusError(Exception):
    def __init__(self, status_code: int):
        super().__init__(f"model returned HTTP {status_code}")
        self.status_code = status_code


def _config() -> AgentConfig:
    return AgentConfig(
        system_prompt="Be brief.",
        model_retry_max_attempts=3,
        model_retry_initial_delay_seconds=0,
        model_retry_max_delay_seconds=0,
    )


def _input_state():
    return {
        "messages": [HumanMessage(content="hello")],
        "tool_calls_count": 0,
        "tool_calls_detail": [],
        "token_usage": {},
        "memory_capability": "",
        "tool_session_capability": "",
        "model_session_capability": "",
        "agent_delegation_session_capability": "",
    }


@pytest.mark.asyncio
@pytest.mark.parametrize("enabled", [False, True])
async def test_agent_graph_honors_explicit_delegation_setting(monkeypatch: pytest.MonkeyPatch, enabled):
    observed = []
    model = FakeModel([AIMessage(content="declared contract")])

    def capture_tools(registered):
        observed.extend(tool.name for tool in registered)
        return model

    model.bind_tools = capture_tools
    monkeypatch.setattr(graph, "create_model", lambda _config: model)
    config = _config()
    config.memory_enabled = False
    config.agent_communication_gateway_url = "http://127.0.0.1:8094"
    config.agent_delegation_enabled = enabled
    result = await graph.build_agent_graph(config).ainvoke(_input_state())
    assert observed == (["delegate_to_agent"] if enabled else [])
    assert result["messages"][-1].content == "declared contract"


@pytest.mark.asyncio
async def test_agent_graph_retries_transient_model_failures(monkeypatch: pytest.MonkeyPatch):
    model = FakeModel([TimeoutError("temporary timeout"), AIMessage(content="ok")])
    monkeypatch.setattr(graph, "create_model", lambda _config: model)

    agent = graph.build_agent_graph(_config())

    result = await agent.ainvoke(_input_state())

    assert model.calls == 2
    assert result["messages"][-1].content == "ok"


@pytest.mark.asyncio
async def test_agent_graph_retries_retriable_model_status_codes(monkeypatch: pytest.MonkeyPatch):
    model = FakeModel([StatusError(429), AIMessage(content="ok")])
    monkeypatch.setattr(graph, "create_model", lambda _config: model)

    agent = graph.build_agent_graph(_config())

    result = await agent.ainvoke(_input_state())

    assert model.calls == 2
    assert result["messages"][-1].content == "ok"


@pytest.mark.asyncio
async def test_agent_graph_does_not_retry_non_transient_model_failures(monkeypatch: pytest.MonkeyPatch):
    model = FakeModel([ValueError("invalid request"), AIMessage(content="unused")])
    monkeypatch.setattr(graph, "create_model", lambda _config: model)

    agent = graph.build_agent_graph(_config())

    with pytest.raises(ValueError, match="invalid request"):
        await agent.ainvoke(_input_state())
    assert model.calls == 1


@pytest.mark.asyncio
async def test_agent_graph_surfaces_error_after_retry_exhaustion(monkeypatch: pytest.MonkeyPatch):
    model = FakeModel([
        TimeoutError("timeout one"),
        TimeoutError("timeout two"),
        TimeoutError("timeout three"),
    ])
    monkeypatch.setattr(graph, "create_model", lambda _config: model)

    agent = graph.build_agent_graph(_config())

    with pytest.raises(TimeoutError, match="timeout three"):
        await agent.ainvoke(_input_state())
    assert model.calls == 3


@pytest.mark.asyncio
async def test_agent_graph_recreates_gateway_model_for_each_invocation(monkeypatch: pytest.MonkeyPatch):
    config = _config()
    config.model_gateway_url = "http://model-gateway:8093/v1"
    models = [FakeModel([AIMessage(content="first")]), FakeModel([AIMessage(content="second")])]
    calls = []
    capabilities = []

    def create_gateway_model(_config):
        capabilities.append(providers._model_session_capability.get())
        model = models[len(calls)]
        calls.append(model)
        return model

    monkeypatch.setattr(graph, "create_model", create_gateway_model)
    agent = graph.build_agent_graph(config)

    first_state = {**_input_state(), "model_session_capability": "first-capability"}
    second_state = {**_input_state(), "model_session_capability": "second-capability"}
    assert (await agent.ainvoke(first_state))["messages"][-1].content == "first"
    assert (await agent.ainvoke(second_state))["messages"][-1].content == "second"
    assert calls == models
    assert capabilities == ["first-capability", "second-capability"]
    assert providers._model_session_capability.get() == ""


@pytest.mark.asyncio
async def test_agent_graph_cancellation_drains_sync_model_call(monkeypatch: pytest.MonkeyPatch):
    started = threading.Event()
    release = threading.Event()
    finished = threading.Event()

    class BlockingModel:
        def invoke(self, _messages):
            started.set()
            release.wait()
            finished.set()
            return AIMessage(content="too late")

    monkeypatch.setattr(graph, "create_model", lambda _config: BlockingModel())
    agent = graph.build_agent_graph(_config())
    invocation = asyncio.create_task(agent.ainvoke(_input_state()))
    assert await asyncio.to_thread(started.wait, 1)

    invocation.cancel()
    await asyncio.sleep(0.02)
    assert invocation.done() is False
    assert finished.is_set() is False

    release.set()
    with pytest.raises(asyncio.CancelledError):
        await invocation
    assert finished.is_set() is True


@pytest.mark.asyncio
async def test_agent_graph_cancellation_drains_sync_tool_call(monkeypatch: pytest.MonkeyPatch):
    started = threading.Event()
    release = threading.Event()
    finished = threading.Event()

    class BlockingTool:
        name = "blocking_tool"

        def invoke(self, _args):
            started.set()
            release.wait()
            finished.set()
            return "too late"

    model = FakeModel([
        AIMessage(
            content="",
            tool_calls=[{"name": "blocking_tool", "args": {}, "id": "call-1", "type": "tool_call"}],
        ),
        AIMessage(content="unused"),
    ])
    monkeypatch.setattr(graph, "create_model", lambda _config: model)
    monkeypatch.setattr(graph, "build_tools", lambda **_kwargs: [BlockingTool()])
    agent = graph.build_agent_graph(_config())
    invocation = asyncio.create_task(agent.ainvoke(_input_state()))
    assert await asyncio.to_thread(started.wait, 1)

    invocation.cancel()
    await asyncio.sleep(0.02)
    assert invocation.done() is False
    assert finished.is_set() is False

    release.set()
    with pytest.raises(asyncio.CancelledError):
        await invocation
    assert finished.is_set() is True


@pytest.mark.asyncio
async def test_agent_graph_restores_tool_capabilities_at_execution_boundary(monkeypatch: pytest.MonkeyPatch):
    observed: dict[str, str] = {}

    class CapabilityTool:
        name = "capability_tool"

        def invoke(self, _args):
            observed["memory"] = memory._memory_capability.get()
            observed["tool"] = tools._tool_session_capability.get()
            observed["delegation"] = tools._agent_delegation_session_capability.get()
            return "391"

    model = FakeModel([
        AIMessage(
            content="",
            tool_calls=[{"name": "capability_tool", "args": {}, "id": "call-1", "type": "tool_call"}],
        ),
        AIMessage(content="391"),
    ])
    monkeypatch.setattr(graph, "create_model", lambda _config: model)
    monkeypatch.setattr(graph, "build_tools", lambda **_kwargs: [CapabilityTool()])
    agent = graph.build_agent_graph(_config())
    invocation_state = _input_state()
    invocation_state.update({
        "memory_capability": "memory-capability",
        "tool_session_capability": "tool-capability",
        "agent_delegation_session_capability": "delegation-capability",
    })

    assert memory._memory_capability.get() == ""
    assert tools._tool_session_capability.get() == ""
    assert tools._agent_delegation_session_capability.get() == ""
    result = await agent.ainvoke(invocation_state)

    assert observed == {
        "memory": "memory-capability",
        "tool": "tool-capability",
        "delegation": "delegation-capability",
    }
    assert result["messages"][-1].content == "391"
    assert memory._memory_capability.get() == ""
    assert tools._tool_session_capability.get() == ""
    assert tools._agent_delegation_session_capability.get() == ""