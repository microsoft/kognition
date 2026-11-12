# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import sys
from pathlib import Path

import pytest
import httpx

sys.path.insert(0, str(Path(__file__).parent))

from config import AgentConfig, _parse_timeout_seconds  # noqa: E402
from providers import _model_gateway_base_url, create_model, model_runtime_session_capability  # noqa: E402


def test_model_gateway_base_url_adds_openai_api_prefix():
    assert _model_gateway_base_url("http://model-gateway:8093") == "http://model-gateway:8093/v1"


def test_model_gateway_base_url_preserves_existing_openai_api_prefix():
    assert _model_gateway_base_url("http://model-gateway:8093/v1/") == "http://model-gateway:8093/v1"


@pytest.mark.parametrize("profiled", [False, True])
def test_ollama_tool_requests_use_governed_gateway_and_session_capability(monkeypatch, profiled):
    import json
    import langchain_openai

    constructor = langchain_openai.ChatOpenAI
    received = {}

    def upstream(request):
        assert str(request.url) == "https://gateway.example/v1/chat/completions"
        assert request.headers["Authorization"] == "Bearer test-session-capability"
        received.update(json.loads(request.content))
        return httpx.Response(200, json={
            "id": "local-tool-call", "object": "chat.completion", "created": 0, "model": "qwen2.5:1.5b",
            "choices": [{
                "index": 0, "message": {
                    "role": "assistant", "content": None,
                    "tool_calls": [{
                        "id": "greeting-call", "type": "function",
                        "function": {"name": "sandbox-greet", "arguments": '{"who":"Kognition"}'},
                    }],
                }, "finish_reason": "tool_calls",
            }],
            "usage": {"prompt_tokens": 11, "completion_tokens": 3, "total_tokens": 14},
        })

    with httpx.Client(transport=httpx.MockTransport(upstream)) as client:
        def with_transport(**kwargs):
            return constructor(http_client=client, **kwargs)

        monkeypatch.setattr(with_transport, "model_fields", constructor.model_fields, raising=False)
        monkeypatch.setattr(langchain_openai, "ChatOpenAI", with_transport)
        config = AgentConfig(
            agent_name="local-agent", model_provider="ollama", model_id="qwen2.5:1.5b",
            model_endpoint="http://private-ollama:11434", api_key="test-provider-key",
            model_gateway_url="https://gateway.example", model_invocation_profile=profiled,
            max_tokens=None if profiled else 128,
        )
        with model_runtime_session_capability("test-session-capability"):
            model = create_model(config)
        response = model.bind_tools([{
            "type": "function", "function": {
                "name": "sandbox-greet",
                "parameters": {"type": "object", "properties": {"who": {"type": "string"}}, "required": ["who"]},
            },
        }]).invoke("Greet Kognition.")

    assert received["model"] == ("kognition-model" if profiled else "qwen2.5:1.5b")
    assert response.tool_calls[0]["name"] == "sandbox-greet"
    assert response.tool_calls[0]["args"] == {"who": "Kognition"}
    assert response.usage_metadata["total_tokens"] == 14
    assert "private-ollama" not in json.dumps(received)
    assert "test-provider-key" not in json.dumps(received)


def test_governed_ollama_requires_session_capability():
    config = AgentConfig(model_provider="ollama", model_gateway_url="https://gateway.example")
    with pytest.raises(ValueError, match="session capability is required"):
        create_model(config)


@pytest.mark.parametrize("provider", ["anthropic", "bedrock", "unsupported"])
def test_unsupported_gateway_provider_remains_rejected(provider):
    config = AgentConfig(model_provider=provider, model_gateway_url="https://gateway.example")
    with model_runtime_session_capability("test-session-capability"):
        with pytest.raises(ValueError, match="supports only"):
            create_model(config)


def test_runtime_timeout_accepts_bare_seconds_and_go_style_durations():
    assert _parse_timeout_seconds("45") == 45
    assert _parse_timeout_seconds("2m") == 120
    assert _parse_timeout_seconds("1m30s") == 90
    assert _parse_timeout_seconds("1500ms") == 1.5


@pytest.mark.parametrize("provider", ["foundry", "openai"])
@pytest.mark.parametrize("model", ["gpt-5.6-luna", "gpt-6-luna", "gpt-4.1"])
def test_provider_uses_generation_parameters_supported_by_deployment(monkeypatch, provider, model):
    import langchain_openai

    received = {}
    result = object()

    def constructor(**kwargs):
        received.update(kwargs)
        return result

    monkeypatch.setattr(langchain_openai, "AzureChatOpenAI" if provider == "foundry" else "ChatOpenAI", constructor)
    config = AgentConfig(
        agent_name="test-agent",
        model_provider=provider,
        model_id=model,
        model_endpoint="https://example.openai.azure.com",
        api_key="test-key",
        max_tokens=256,
        temperature=0.25,
    )
    assert create_model(config) is result
    if model.startswith(("gpt-5", "gpt-6")):
        assert received["max_completion_tokens"] == 256
        assert "max_tokens" not in received
        assert "temperature" not in received
        if model.startswith("gpt-6"):
            assert received["reasoning_effort"] == "none"
    else:
        assert received["max_tokens"] == 256
        assert received["temperature"] == 0.25
        assert "max_completion_tokens" not in received


def test_gateway_provider_uses_reasoning_deployment_parameters(monkeypatch):
    import langchain_openai

    received = {}

    def constructor(**kwargs):
        received.update(kwargs)
        return object()

    monkeypatch.setattr(langchain_openai, "ChatOpenAI", constructor)
    config = AgentConfig(
        agent_name="test-agent",
        model_provider="foundry",
        model_id="gpt-6-luna",
        model_gateway_url="https://model-gateway.example",
        max_tokens=128,
    )
    with model_runtime_session_capability("test-capability"):
        create_model(config)
    assert received["max_completion_tokens"] == 128
    assert received["reasoning_effort"] == "none"
    assert "temperature" not in received
    assert received["api_key"] == "test-capability"


@pytest.mark.parametrize("gateway,profiled", [(False, False), (True, False), (True, True)])
def test_gpt6_tool_request_serializes_through_real_sdk(monkeypatch, gateway, profiled):
    import json
    import langchain_openai

    constructor_name = "ChatOpenAI" if gateway else "AzureChatOpenAI"
    constructor = getattr(langchain_openai, constructor_name)
    received = {}

    def upstream(request):
        received.update(json.loads(request.content))
        assert request.url.path.endswith("/chat/completions")
        return httpx.Response(200, json={
            "id": "sample-completion",
            "object": "chat.completion",
            "created": 0,
            "model": "gpt-6-luna",
            "choices": [{
                "index": 0,
                "message": {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [{
                        "id": "sample-tool-call",
                        "type": "function",
                        "function": {"name": "sample_lookup", "arguments": '{"topic":"kognition"}'},
                    }],
                },
                "finish_reason": "tool_calls",
            }],
            "usage": {"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3},
        })

    with httpx.Client(transport=httpx.MockTransport(upstream)) as client:
        def with_transport(**kwargs):
            return constructor(http_client=client, **kwargs)

        monkeypatch.setattr(with_transport, "model_fields", constructor.model_fields, raising=False)
        monkeypatch.setattr(langchain_openai, constructor_name, with_transport)
        config = AgentConfig(
            agent_name="test-agent",
            model_provider="foundry",
            model_id="kognition-model" if profiled else "gpt-6-luna",
            model_endpoint="https://example.openai.azure.com",
            model_gateway_url="https://gateway.example" if gateway else "",
            api_key="test-key",
            model_invocation_profile=profiled,
            max_tokens=None if profiled else 256,
        )
        with model_runtime_session_capability("test-capability"):
            model = create_model(config)
        tool = {
            "type": "function",
            "function": {
                "name": "sample_lookup",
                "parameters": {"type": "object", "properties": {"topic": {"type": "string"}}, "required": ["topic"]},
            },
        }
        response = model.bind_tools([tool]).invoke("Use sample_lookup for kognition.")
    assert response.tool_calls[0]["name"] == "sample_lookup"
    if profiled:
        assert "max_completion_tokens" not in received
        assert "reasoning_effort" not in received
    else:
        assert received["max_completion_tokens"] == 256
        assert received["reasoning_effort"] == "none"
    assert "temperature" not in received
    assert "max_tokens" not in received


def test_profile_runtime_preserves_omitted_generation_settings(monkeypatch):
    monkeypatch.setenv("KOGNITION_MODEL_INVOCATION_PROFILE", "true")
    monkeypatch.delenv("KOGNITION_MAX_TOKENS", raising=False)
    monkeypatch.delenv("KOGNITION_TEMPERATURE", raising=False)
    config = AgentConfig.from_env()
    assert config.model_invocation_profile
    assert config.max_tokens is None
    assert config.temperature is None
    constructed = AgentConfig(model_invocation_profile=True)
    assert constructed.max_tokens is None
    assert constructed.temperature is None


@pytest.mark.parametrize("model", ["production-chat", "gpt-6-luna", "claude-alias"])
def test_profile_runtime_leaves_provider_decisions_to_gateway(monkeypatch, model):
    import langchain_openai

    received = {}

    def constructor(**kwargs):
        received.update(kwargs)
        return object()

    monkeypatch.setattr(constructor, "model_fields", {"use_responses_api": object()}, raising=False)
    monkeypatch.setattr(langchain_openai, "ChatOpenAI", constructor)
    config = AgentConfig(
        agent_name="test-agent", model_id=model, model_provider="openai-compatible",
        model_gateway_url="https://gateway.example", model_invocation_profile=True,
        max_tokens=None, temperature=None,
    )
    with model_runtime_session_capability("capability"):
        create_model(config)
    assert not ({"max_tokens", "max_completion_tokens", "temperature", "reasoning_effort"} & received.keys())
    assert received["use_responses_api"] is False
    assert received["model"] == "kognition-model"
    config.max_tokens = 128
    config.temperature = 0.1
    with model_runtime_session_capability("capability"):
        create_model(config)
    assert received["max_tokens"] == 128
    assert received["temperature"] == 0.1
    config.model_gateway_url = ""
    with pytest.raises(ValueError, match="requires the Model Gateway"):
        create_model(config)