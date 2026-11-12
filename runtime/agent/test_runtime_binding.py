# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import pytest

from runtime_binding import matches_configuration, startup_configuration


@pytest.mark.parametrize("certificate,key", [("/operator/tls.crt", ""), ("", "/operator/tls.key")])
def test_worker_control_tls_rejects_incomplete_operator_material(monkeypatch, certificate, key):
    from config import AgentConfig

    monkeypatch.setenv("KOGNITION_CONTROL_TLS_CERT_FILE", certificate)
    monkeypatch.setenv("KOGNITION_CONTROL_TLS_KEY_FILE", key)
    with pytest.raises(ValueError, match="both certificate and key"):
        AgentConfig.from_env()


def test_worker_control_tls_uses_explicit_operator_paths(monkeypatch):
    from config import AgentConfig

    monkeypatch.setenv("KOGNITION_CONTROL_TLS_CERT_FILE", "/operator/tls.crt")
    monkeypatch.setenv("KOGNITION_CONTROL_TLS_KEY_FILE", "/operator/tls.key")
    configuration = AgentConfig.from_env()
    assert configuration.control_tls_certificate == "/operator/tls.crt"
    assert configuration.control_tls_key == "/operator/tls.key"

def test_spire_profile_uses_only_bound_loopback_transport(monkeypatch):
    from config import AgentConfig

    monkeypatch.setenv("KOGNITION_WORKLOAD_IDENTITY_MODE", "spire")
    monkeypatch.setenv("KOGNITION_WORKLOAD_PROXY_URL", "http://127.0.0.1:8096")
    loaded = AgentConfig.from_env()
    assert loaded.model_gateway_url == "http://127.0.0.1:8096/model"
    assert loaded.tool_gateway_url == "http://127.0.0.1:8096/tool"
    assert loaded.memory_service_url == "http://127.0.0.1:8096/memory"
    assert loaded.agent_communication_gateway_url == "http://127.0.0.1:8096/communication"
    environment = {"KOGNITION_NAMESPACE": "tenant", "KOGNITION_AGENT_NAME": "worker", "KOGNITION_MODEL_GATEWAY_URL": "https://gateway", "KOGNITION_WORKLOAD_IDENTITY_MODE": "spire", "KOGNITION_WORKLOAD_PROXY_URL": "http://127.0.0.1:8096"}
    configuration = startup_configuration(environment)
    assert matches_configuration(json.dumps(configuration), configuration)
    assert not matches_configuration(json.dumps({**configuration, "KOGNITION_WORKLOAD_PROXY_URL": "http://127.0.0.1:9000"}), configuration)


@pytest.mark.parametrize("origin", ["", "http://localhost:8096", "http://remote:8096", "https://127.0.0.1:8096", "http://127.0.0.1:8096/path", "http://user:secret@127.0.0.1:8096", "http://127.0.0.1:8096?target=remote"])
def test_spire_profile_rejects_unsafe_transport_origin(monkeypatch, origin):
    from config import AgentConfig

    monkeypatch.setenv("KOGNITION_WORKLOAD_IDENTITY_MODE", "spire")
    monkeypatch.setenv("KOGNITION_WORKLOAD_PROXY_URL", origin)
    with pytest.raises(ValueError):
        AgentConfig.from_env()


def test_binding_excludes_authentication_and_telemetry_secrets():
    environment = {
        "KOGNITION_NAMESPACE": "tenant", "KOGNITION_AGENT_NAME": "agent",
        "KOGNITION_MODEL_GATEWAY_URL": "http://model:8093",
        "KOGNITION_RUNTIME_AUTH_TOKEN": "secret", "KOGNITION_API_KEY": "secret",
        "KOGNITION_OTEL_TRACES_HEADERS": "secret",
    }
    loaded = startup_configuration(environment)
    assert len(loaded) == 3
    assert matches_configuration(json.dumps(loaded), loaded)
    for key in loaded:
        different = dict(loaded, **{key: "different"})
        assert not matches_configuration(json.dumps(different), loaded)
    assert not matches_configuration("[]", loaded)
    assert not matches_configuration("invalid", loaded)
    assert not matches_configuration("{}", {})


@pytest.mark.parametrize("override", ["KOGNITION_TOOLS_JSON", "KOGNITION_AGENT_DELEGATION_ENABLED", "KOGNITION_TEMPERATURE", "KOGNITION_MODEL_RETRY_MAX_ATTEMPTS", "KOGNITION_MODEL_RETRY_INITIAL_DELAY_SECONDS", "KOGNITION_MODEL_RETRY_MAX_DELAY_SECONDS"])
def test_binding_rejects_undeclared_runtime_tools(override):
    expected = {"KOGNITION_NAMESPACE": "tenant", "KOGNITION_AGENT_NAME": "agent",
                "KOGNITION_MODEL_GATEWAY_URL": "http://model:8093"}
    loaded = startup_configuration({**expected, override: "[]"})
    assert not matches_configuration(json.dumps(expected), loaded)


def test_binding_rejects_runtime_engine_mismatch():
    native = {"KOGNITION_NAMESPACE": "tenant", "KOGNITION_AGENT_NAME": "agent", "KOGNITION_MODEL_GATEWAY_URL": "http://model:8093"}
    autogen = {**native, "KOGNITION_RUNTIME_CLASS": "autogen"}
    assert matches_configuration(json.dumps(autogen), startup_configuration(autogen))
    assert not matches_configuration(json.dumps(autogen), startup_configuration(native))
    assert not matches_configuration(json.dumps(native), startup_configuration(autogen))


def test_binding_pins_profile_omission_semantics():
    legacy = {"KOGNITION_NAMESPACE": "tenant", "KOGNITION_AGENT_NAME": "agent", "KOGNITION_MODEL_GATEWAY_URL": "https://gateway"}
    profiled = {**legacy, "KOGNITION_MODEL_INVOCATION_PROFILE": "true"}
    assert matches_configuration(json.dumps(profiled), startup_configuration(profiled))
    assert not matches_configuration(json.dumps(profiled), startup_configuration(legacy))
    assert not matches_configuration(json.dumps(legacy), startup_configuration(profiled))