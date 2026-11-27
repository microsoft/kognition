# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import asyncio
import json
import threading

import pytest

pytest.importorskip("autogen_agentchat")

from autogen_core import FunctionCall
from autogen_core.models import CreateResult, RequestUsage
from autogen_ext.models.replay import ReplayChatCompletionClient
from langchain_core.messages import AIMessage, HumanMessage
from langchain_core.tools import StructuredTool
import httpx

from autogen_graph import build_autogen_graph
from config import AgentConfig

MODEL_INFO = {"vision": False, "function_calling": True, "json_output": False, "family": "unknown", "structured_output": False}


def protected_config(**overrides):
    values = {
        "agent_name": "worker",
        "namespace": "tenant",
        "runtime_auth_token": "test-controller-token",
        "model_gateway_url": "http://127.0.0.1:8093",
        "tool_gateway_url": "http://127.0.0.1:8092",
        "memory_service_url": "http://127.0.0.1:8091",
        "agent_communication_gateway_url": "http://127.0.0.1:8094",
        "memory_enabled": False,
        "system_prompt": "Keep the original instructions.",
    }
    values.update(overrides)
    return AgentConfig(**values)


def completion(content, input_tokens=2, output_tokens=3, reason="stop"):
    return CreateResult(content=content, finish_reason=reason, usage=RequestUsage(prompt_tokens=input_tokens, completion_tokens=output_tokens), cached=False)


@pytest.mark.asyncio
@pytest.mark.parametrize("names", [[], ["get_resources", "get_available_api_resources", "QueryTool", "GeneratePromQLTool", "DashboardManagementTool"]])
async def test_real_autogen_explicit_tool_contract_disables_implicit_delegation(names):
    observed = []
    client = ReplayChatCompletionClient([completion("exact reviewed contract")], model_info=MODEL_INFO)
    original = client.create

    async def capture(messages, *, tools=(), **kwargs):
        observed.extend(tool["name"] if isinstance(tool, dict) else tool.schema["name"] for tool in tools)
        return await original(messages, tools=tools, **kwargs)

    client.create = capture
    resolved = json.dumps([{"name": f"reviewed-resource-{index}", "runtimeName": name, "inputSchema": {"type": "object", "properties": {}}} for index, name in enumerate(names)])
    graph = build_autogen_graph(protected_config(tools_resolved_json=resolved, agent_delegation_enabled=False), model_factory=lambda config, capability: client)
    final = [value async for value in graph.astream({"messages": [HumanMessage(content="run the reviewed contract")], "model_session_capability": "model-proof"})][-1]
    assert observed == names
    assert final["messages"][-1].content == "exact reviewed contract"


@pytest.mark.asyncio
async def test_real_autogen_assistant_preserves_history_and_usage():
    observed = []

    def model_factory(config, capability):
        client = ReplayChatCompletionClient([completion("real SDK answer")], model_info=MODEL_INFO)
        observed.append((config, capability, client))
        return client

    graph = build_autogen_graph(protected_config(), model_factory=model_factory)
    state = {
        "messages": [HumanMessage(content="old question"), AIMessage(content="old answer"), HumanMessage(content="new question")],
        "model_session_capability": "scoped-model-proof",
    }
    snapshots = [value async for value in graph.astream(state, stream_mode="values")]
    final = snapshots[-1]
    assert [message.content for message in final["messages"]] == ["old question", "old answer", "new question", "real SDK answer"]
    usage = observed[0][2].total_usage()
    assert final["token_usage"] == {"prompt_tokens": usage.prompt_tokens, "completion_tokens": usage.completion_tokens, "total_tokens": usage.prompt_tokens + usage.completion_tokens}
    assert usage.prompt_tokens > 0 and usage.completion_tokens == 3
    assert observed[0][1] == "scoped-model-proof"
    assert final["tool_calls_count"] == 0


@pytest.mark.parametrize("missing", ["runtime_auth_token", "model_gateway_url", "tool_gateway_url", "memory_service_url", "agent_communication_gateway_url"])
def test_autogen_requires_protected_runtime_configuration(missing):
    with pytest.raises(ValueError):
        build_autogen_graph(protected_config(**{missing: ""}))


@pytest.mark.asyncio
async def test_autogen_rejects_missing_model_capability_before_factory():
    def forbidden_factory(*args):
        pytest.fail("model factory reached without a session capability")

    graph = build_autogen_graph(protected_config(), model_factory=forbidden_factory)
    with pytest.raises(ValueError):
        _ = [value async for value in graph.astream({"messages": [HumanMessage(content="question")]})]


@pytest.mark.asyncio
async def test_real_autogen_tools_use_registered_governed_helper(monkeypatch):
    from langchain_core.tools import StructuredTool

    calls = []

    def guarded_echo(text: str) -> str:
        calls.append(text)
        return "governed result"

    tool = StructuredTool.from_function(guarded_echo, name="guarded_echo", description="A registered governed tool")
    monkeypatch.setattr("autogen_graph.build_tools", lambda **kwargs: [tool])
    client = ReplayChatCompletionClient([
        completion([FunctionCall(id="call-1", name="guarded_echo", arguments='{"text":"approved input"}')], reason="function_calls"),
        completion("completed"),
    ], model_info=MODEL_INFO)
    graph = build_autogen_graph(
        protected_config(),
        model_factory=lambda config, capability: client,
    )
    final = [value async for value in graph.astream({"messages": [HumanMessage(content="question")], "model_session_capability": "model-proof"})][-1]
    assert calls == ["approved input"]
    assert final["messages"][-1].content == "completed"
    assert final["tool_calls_count"] == 1
    usage = client.total_usage()
    assert final["token_usage"]["total_tokens"] == usage.prompt_tokens + usage.completion_tokens
    assert usage.completion_tokens == 6


def test_runtime_factory_selects_autogen_without_native_fallback(monkeypatch):
    import main

    monkeypatch.setattr(main, "build_agent_graph", lambda config: pytest.fail("AutoGen silently selected native"))
    config = protected_config()
    config.runtime_class = "autogen"
    graph = main.build_runtime_graph(config)
    assert graph.__class__.__name__ == "AutoGenGraph"


def test_runtime_factory_preserves_native_and_rejects_unknown(monkeypatch):
    import main

    sentinel = object()
    monkeypatch.setattr(main, "build_agent_graph", lambda config: sentinel)
    config = protected_config()
    config.runtime_class = "native"
    assert main.build_runtime_graph(config) is sentinel
    config.runtime_class = "unsupported"
    with pytest.raises(ValueError):
        main.build_runtime_graph(config)


@pytest.mark.asyncio
@pytest.mark.parametrize("approval_state", ["Pending", "Terminal", "Unavailable"])
async def test_autogen_approval_barrier_stops_before_reflection(monkeypatch, approval_state):
    calls = []

    def approval_tool(text: str) -> str:
        calls.append(text)
        return json.dumps({"runtime_approval_state": approval_state, "invocation_id": "stable-invocation"})

    tool = StructuredTool.from_function(approval_tool, name="approval_tool", description="Governed approval")
    monkeypatch.setattr("autogen_graph.build_tools", lambda **kwargs: [tool])
    client = ReplayChatCompletionClient([
        completion([FunctionCall(id="approval-call", name="approval_tool", arguments='{"text":"requested"}')], reason="function_calls"),
        completion("must not reflect before authorization"),
    ], model_info=MODEL_INFO)
    graph = build_autogen_graph(protected_config(), model_factory=lambda config, capability: client)
    result = await graph.ainvoke({"messages": [HumanMessage(content="invoke")], "model_session_capability": "model-capability"})
    assert calls == ["requested"]
    assert result["runtime_approval_state"] == approval_state
    assert json.loads(result["messages"][-1].content)["invocation_id"] == "stable-invocation"
    assert client.total_usage().completion_tokens == 3


@pytest.mark.asyncio
@pytest.mark.parametrize("operation", ["memory", "delegation", "delegation-invoke"])
@pytest.mark.parametrize("approval_state,status_code", [("Pending", 202), ("Terminal", 409), ("Unavailable", 503)])
async def test_autogen_gateway_approval_barrier_preserves_response(monkeypatch, operation, approval_state, status_code):
    calls = []

    def gateway(endpoint, *, json, timeout, headers):
        calls.append((endpoint, json, headers))
        if operation == "delegation-invoke" and len(calls) == 1:
            return httpx.Response(200, request=httpx.Request("POST", endpoint), json={"capability": "invocation-proof"})
        invocation_id = headers["X-Kognition-Memory-Invocation-Id"] if operation == "memory" else json["invocation_id"]
        return httpx.Response(status_code, request=httpx.Request("POST", endpoint), json={
            "runtime_approval_state": approval_state, "invocation_id": invocation_id, "runtime_approval": {"name": "approval-1"},
        })

    monkeypatch.setattr("memory.httpx.post" if operation == "memory" else "tools.httpx.post", gateway)
    tool_name = "memory_store" if operation == "memory" else "delegate_to_agent"
    arguments = {"content": "requested note"} if operation == "memory" else {"target_namespace": "tenant", "target_agent_service": "reviewer", "message": "requested review"}
    client = ReplayChatCompletionClient([
        completion([FunctionCall(id="governed-call", name=tool_name, arguments=json.dumps(arguments))], reason="function_calls"),
        completion("must not reflect before authorization"),
    ], model_info=MODEL_INFO)
    graph = build_autogen_graph(protected_config(memory_enabled=operation == "memory"), model_factory=lambda config, capability: client)
    result = await graph.ainvoke({
        "messages": [HumanMessage(content="perform governed action")], "model_session_capability": "model-proof", "memory_capability": "memory-proof", "agent_delegation_session_capability": "delegation-proof",
    })
    assert len(calls) == (2 if operation == "delegation-invoke" else 1)
    assert calls[0][2]["Authorization"] == ("Bearer memory-proof" if operation == "memory" else "Bearer delegation-proof")
    if operation == "delegation-invoke":
        assert calls[1][2]["Authorization"] == "Bearer invocation-proof"
        assert calls[0][1]["invocation_id"] == calls[1][1]["invocation_id"]
    assert result["runtime_approval_state"] == approval_state
    expected_invocation = calls[0][2]["X-Kognition-Memory-Invocation-Id"] if operation == "memory" else calls[0][1]["invocation_id"]
    assert expected_invocation
    response_body = json.loads(result["messages"][-1].content)
    assert response_body["invocation_id"] == expected_invocation
    assert response_body["runtime_approval_state"] == approval_state
    assert response_body["runtime_approval"] == {"name": "approval-1"}
    assert client.total_usage().completion_tokens == 3


@pytest.mark.asyncio
async def test_autogen_rejects_parallel_tool_batch_before_any_effect(monkeypatch):
    calls = []

    def effect_tool(text: str) -> str:
        calls.append(text)
        return "effect"

    tool = StructuredTool.from_function(effect_tool, name="effect_tool", description="Governed effect")
    monkeypatch.setattr("autogen_graph.build_tools", lambda **kwargs: [tool])
    client = ReplayChatCompletionClient([
        completion([
            FunctionCall(id="first", name="effect_tool", arguments='{"text":"first"}'),
            FunctionCall(id="second", name="effect_tool", arguments='{"text":"second"}'),
        ], reason="function_calls"),
        completion("must not execute"),
    ], model_info=MODEL_INFO)
    graph = build_autogen_graph(protected_config(), model_factory=lambda config, capability: client)
    with pytest.raises(RuntimeError):
        await graph.ainvoke({"messages": [HumanMessage(content="invoke")], "model_session_capability": "model-capability"})
    assert calls == []


@pytest.mark.asyncio
async def test_autogen_cancellation_drains_governed_tool_before_client_close(monkeypatch):
    entered = asyncio.Event()
    release = threading.Event()
    loop = asyncio.get_running_loop()
    timeline = []

    def blocking_tool(text: str) -> str:
        loop.call_soon_threadsafe(entered.set)
        assert release.wait(5), "test did not release governed tool"
        timeline.append("effect finished")
        return text

    tool = StructuredTool.from_function(blocking_tool, name="blocking_tool", description="Governed effect")
    monkeypatch.setattr("autogen_graph.build_tools", lambda **kwargs: [tool])
    client = ReplayChatCompletionClient([
        completion([FunctionCall(id="blocking", name="blocking_tool", arguments='{"text":"requested"}')], reason="function_calls"),
        completion("must not continue after cancellation"),
    ], model_info=MODEL_INFO)

    async def close():
        timeline.append("client closed")

    monkeypatch.setattr(client, "close", close)
    graph = build_autogen_graph(protected_config(), model_factory=lambda config, capability: client)
    invocation = asyncio.create_task(graph.ainvoke({"messages": [HumanMessage(content="invoke")], "model_session_capability": "model-capability"}))
    await asyncio.wait_for(entered.wait(), 5)
    invocation.cancel()
    try:
        done, _ = await asyncio.wait({invocation}, timeout=0.05)
        assert not done, "cancellation completed before the tool effect stopped"
        assert "client closed" not in timeline
    finally:
        release.set()
        with pytest.raises(asyncio.CancelledError):
            await invocation
    assert timeline == ["effect finished", "client closed"]
    assert client.total_usage().completion_tokens == 3


@pytest.mark.asyncio
async def test_autogen_sdk_state_restores_history_and_tool_results_without_replay(monkeypatch):
    effects = []

    def persisted_tool(text: str) -> str:
        effects.append(text)
        return "durable tool result"

    tool = StructuredTool.from_function(persisted_tool, name="persisted_tool", description="Governed effect")
    monkeypatch.setattr("autogen_graph.build_tools", lambda **kwargs: [tool])
    first_client = ReplayChatCompletionClient([
        completion([FunctionCall(id="persisted-call", name="persisted_tool", arguments='{"text":"only once"}')], reason="function_calls"),
        completion("first answer"),
    ], model_info=MODEL_INFO)
    first = await build_autogen_graph(protected_config(), model_factory=lambda config, capability: first_client).ainvoke({
        "messages": [HumanMessage(content="original user turn")], "model_session_capability": "ephemeral-model-token",
    })
    saved = json.loads(json.dumps(first["autogen_state"]))
    assert "ephemeral-model-token" not in json.dumps(saved)
    second_client = ReplayChatCompletionClient([completion("restored answer")], model_info=MODEL_INFO)
    observed = []
    original_create = second_client.create

    async def capture(messages, **kwargs):
        observed.extend(messages)
        return await original_create(messages, **kwargs)

    monkeypatch.setattr(second_client, "create", capture)
    restored = await build_autogen_graph(protected_config(), model_factory=lambda config, capability: second_client).ainvoke({
        "messages": [HumanMessage(content="next user turn")], "model_session_capability": "fresh-model-token", "autogen_state": saved,
    })
    assert effects == ["only once"]
    assert restored["messages"][-1].content == "restored answer"
    assert any(getattr(message, "content", "") == "original user turn" for message in observed)
    assert any(getattr(message, "content", "") == "first answer" for message in observed)
    assert any(isinstance(message.content, list) and any(getattr(item, "call_id", None) == "persisted-call" and item.content == "durable tool result" for item in message.content) for message in observed)


@pytest.mark.asyncio
async def test_autogen_does_not_retry_unconfirmed_gateway_failure(monkeypatch):
    import autogen_graph

    requests = []
    sdk_client = autogen_graph.OpenAIChatCompletionClient

    async def unavailable(request):
        requests.append(request)
        return httpx.Response(502, json={"error": {"message": "provider outcome unconfirmed", "type": "upstream_error"}})

    def factory(**kwargs):
        return sdk_client(**kwargs, http_client=httpx.AsyncClient(transport=httpx.MockTransport(unavailable)))

    monkeypatch.setattr(autogen_graph, "OpenAIChatCompletionClient", factory)
    agent = build_autogen_graph(protected_config(agent_delegation_enabled=False))
    with pytest.raises(RuntimeError, match="AutoGen execution failed"):
        await agent.ainvoke({"messages": [HumanMessage(content="one intentional inference")], "model_session_capability": "scoped-model-proof"})
    assert len(requests) == 1, "SDK repeated an unconfirmed provider operation"
    assert requests[0].headers["Authorization"] == "Bearer scoped-model-proof"


def test_real_autogen_http_runtime_forwards_only_model_session_capability(monkeypatch):
    from fastapi.testclient import TestClient
    import autogen_graph
    import main

    requests = []
    observed = threading.Event()
    completed = threading.Event()
    sdk_client = autogen_graph.OpenAIChatCompletionClient

    async def gateway(request):
        requests.append(request)
        observed.set()
        return httpx.Response(200, json={
            "id": "fixture-completion", "object": "chat.completion", "created": 1, "model": "gpt-4o-2024-08-06",
            "choices": [{"index": 0, "message": {"role": "assistant", "content": "actual SDK HTTP response"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
        })

    def factory(**kwargs):
        return sdk_client(**kwargs, http_client=httpx.AsyncClient(transport=httpx.MockTransport(gateway)))

    configuration = {"KOGNITION_NAMESPACE": "tenant", "KOGNITION_AGENT_NAME": "worker", "KOGNITION_MODEL_GATEWAY_URL": "http://127.0.0.1:8093", "KOGNITION_RUNTIME_CLASS": "autogen"}
    config = protected_config(runtime_class="autogen")
    monkeypatch.setattr(autogen_graph, "OpenAIChatCompletionClient", factory)
    monkeypatch.setattr(main, "config", config)
    monkeypatch.setattr(main, "external_runtime", True)
    monkeypatch.setattr(main, "loaded_configuration", configuration)
    monkeypatch.setattr(main, "state", main.RuntimeState())
    monkeypatch.setattr(main, "agent_graph", build_autogen_graph(config))
    original_archive = main._archive_completed_session

    def archive(session_id):
        original_archive(session_id)
        completed.set()

    monkeypatch.setattr(main, "_archive_completed_session", archive)
    main.state.phase = main.RuntimePhase.READY
    payload = {"message": "authenticated user turn", "session_id": "http-sdk-session", "model_session_capability": "scoped-model-capability", "runtime_configuration": json.dumps(configuration)}
    headers = {"Authorization": "Bearer test-controller-token", "traceparent": "00-11111111111111111111111111111111-2222222222222222-01"}
    with TestClient(main.app) as client:
        assert client.post("/invoke", json=payload).status_code == 401
        mismatched = {**payload, "runtime_configuration": json.dumps({**configuration, "KOGNITION_NAMESPACE": "other-tenant"})}
        assert client.post("/invoke", json=mismatched, headers=headers).status_code == 412
        assert not requests
        assert client.post("/invoke", json=payload, headers=headers).status_code == 202
        assert observed.wait(5), "real SDK did not reach the governed HTTP gateway"
        assert completed.wait(5), "real SDK HTTP invocation did not reach a terminal result"
        status = client.get("/status?session_id=http-sdk-session", headers=headers).json()
        assert status["phase"] == "Completed"
        assert status["output"] == "actual SDK HTTP response"
        assert status["token_usage"] == {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
        completed.clear()
        session = client.post("/sessions", json={"agent_name": "worker", "namespace": "tenant", "runtime_configuration": json.dumps(configuration)}, headers=headers)
        assert session.status_code == 201
        session_id = session.json()["session_id"]
        empty_checkpoint = client.get(f"/sessions/{session_id}/checkpoint", headers=headers)
        assert empty_checkpoint.status_code == 200
        assert empty_checkpoint.json()["sdk_state"]["llm_context"] == {"messages": []}
        assert client.post(f"/sessions/{session_id}/step", json={"message": "bridge first turn", "turn_id": "bridge-first-turn", "model_session_capability": "fresh-bridge-capability"}, headers=headers).status_code == 202
        assert completed.wait(5)
        bridge_status = client.get(f"/sessions/{session_id}/status", headers=headers)
        assert bridge_status.status_code == 200
        assert bridge_status.json()["phase"] == "Completed"
        checkpoint = client.get(f"/sessions/{session_id}/checkpoint", headers=headers)
        assert checkpoint.status_code == 200
        assert "fresh-bridge-capability" not in checkpoint.text
        assert "test-controller-token" not in checkpoint.text
        assert checkpoint.json()["sdk_state"]
        restored = client.post("/sessions/restore", json=checkpoint.json(), headers=headers)
        assert restored.status_code == 201
        assert restored.json()["session_id"] != session_id
        assert client.post("/sessions/restore", json={**checkpoint.json(), "namespace": "other-tenant"}, headers=headers).status_code == 412
        bound_id = "70000000-0000-4000-8000-000000000001"
        binding = {"agent_name": "worker", "namespace": "tenant", "runtime_configuration": json.dumps(configuration), "session_id": bound_id, "instance_uid": "instance-uid"}
        assert client.post("/sessions", json=binding, headers=headers).json()["session_id"] == bound_id
        assert client.post("/sessions", json=binding, headers=headers).json()["session_id"] == bound_id
        assert client.post("/sessions", json={**binding, "instance_uid": "other-instance"}, headers=headers).status_code == 409
        assert client.get(f"/sessions/{bound_id}/status", headers=headers).json()["instance_uid"] == "instance-uid"
        assert client.post(f"/sessions/{bound_id}/release", headers=headers).status_code == 409
        completed.clear()
        turn = {"message": "bound controller turn", "turn_id": bound_id, "model_session_capability": "bound-model-capability"}
        assert client.post(f"/sessions/{bound_id}/step", json={**turn, "turn_id": "foreign-turn"}, headers=headers).status_code == 409
        assert client.post(f"/sessions/{bound_id}/step", json=turn, headers=headers).status_code == 202
        assert completed.wait(5)
        assert client.post(f"/sessions/{bound_id}/step", json=turn, headers=headers).json()["accepted"] is False
        bound_status = client.get(f"/sessions/{bound_id}/status", headers=headers).json()
        assert bound_status["session_id"] == bound_id
        assert bound_status["instance_uid"] == "instance-uid"
        assert bound_status["token_usage"]["total_tokens"] == 18
        checkpoint = client.get(f"/sessions/{bound_id}/checkpoint", headers=headers)
        assert checkpoint.status_code == 200
        assert client.post("/sessions/restore", json={**checkpoint.json(), "restore_bound": True}, headers=headers).status_code == 409
        assert client.post(f"/sessions/{bound_id}/release", headers=headers).status_code == 200
        assert client.get(f"/sessions/{bound_id}/status", headers=headers).status_code == 404
    assert len(requests) == 3
    assert requests[0].headers["authorization"] == "Bearer scoped-model-capability"
    assert requests[0].headers.get("traceparent", "").startswith("00-11111111111111111111111111111111-")
    assert requests[0].url.path == "/v1/chat/completions"
    assert "test-controller-token" not in requests[0].content.decode()
