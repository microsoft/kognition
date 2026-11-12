# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Agent runtime configuration loaded from environment variables."""

import os
import re
import ipaddress
from dataclasses import dataclass, field


@dataclass
class AgentConfig:
    """Configuration for a single agent runtime pod."""

    # Identity
    agent_name: str = ""
    instance_name: str = ""
    namespace: str = "default"
    session_id: str = ""
    runtime_auth_token: str = ""
    runtime_class: str = "native"
    workload_identity_mode: str = "capability"
    workload_proxy_url: str = ""

    # Model
    model_provider: str = "openai"  # openai, anthropic, ollama, azure
    model_id: str = "gpt-4o"
    model_endpoint: str = "https://api.openai.com/v1"
    api_key: str = ""
    api_version: str = "2024-12-01-preview"
    model_gateway_url: str = ""
    model_invocation_profile: bool = False

    # Agent behavior
    system_prompt: str = "You are a helpful assistant."
    max_tokens: int | None = None
    temperature: float | None = None
    timeout_seconds: float = 30
    model_retry_max_attempts: int = 3
    model_retry_initial_delay_seconds: float = 0.5
    model_retry_max_delay_seconds: float = 4.0

    # Tools (JSON-encoded list of tool definitions)
    tools_json: str = "[]"
    # Tools with fully-resolved Tool CR specs (name, description, protocol,
    # endpoint, inputSchema). Populated by the dispatcher. Preferred over
    # tools_json by the runtime when present.
    tools_resolved_json: str = "[]"
    tool_gateway_url: str = ""
    agent_communication_gateway_url: str = ""
    agent_delegation_enabled: bool = True

    # Memory
    memory_service_url: str = ""  # empty = use default in-cluster URL
    memory_enabled: bool = True
    # When set, the runtime uses this as its volume_handle instead of
    # falling back to agent_name. Injected by the dispatcher from the
    # bound MemoryVolume so two agents that reference the same
    # MemoryClaim actually share the same memory bucket.
    memory_volume: str = ""

    # Knowledge bases. JSON-encoded list of resolved KBs produced by the
    # dispatcher (KOGNITION_KB_RESOLVED). Each KB carries its name and a
    # list of sources; only Type="inline" sources currently carry content
    # the runtime can search.
    kb_resolved_json: str = "[]"
    knowledge_gateway_url: str = ""
    knowledge_gateway_ca_file: str = ""

    # Runtime
    port: int = 8080
    log_level: str = "INFO"
    control_tls_certificate: str = ""
    control_tls_key: str = ""

    def __post_init__(self) -> None:
        if not self.model_invocation_profile:
            if self.max_tokens is None:
                self.max_tokens = 4096
            if self.temperature is None:
                self.temperature = 0.7

    @classmethod
    def from_env(cls) -> "AgentConfig":
        """Load configuration from environment variables."""
        configuration = cls(
            agent_name=os.getenv("KOGNITION_AGENT_NAME", ""),
            instance_name=os.getenv("KOGNITION_INSTANCE_NAME", ""),
            namespace=os.getenv("KOGNITION_NAMESPACE", "default"),
            session_id=os.getenv("KOGNITION_SESSION_ID", ""),
            runtime_auth_token=os.getenv("KOGNITION_RUNTIME_AUTH_TOKEN", ""),
            runtime_class=os.getenv("KOGNITION_RUNTIME_CLASS", "native"),
            workload_identity_mode=os.getenv("KOGNITION_WORKLOAD_IDENTITY_MODE", "capability"),
            workload_proxy_url=os.getenv("KOGNITION_WORKLOAD_PROXY_URL", ""),
            model_provider=os.getenv("KOGNITION_MODEL_PROVIDER", "openai"),
            model_id=os.getenv("KOGNITION_MODEL_ID", "gpt-4o"),
            model_endpoint=os.getenv("KOGNITION_MODEL_ENDPOINT", "https://api.openai.com/v1"),
            api_key=os.getenv("KOGNITION_API_KEY", ""),
            api_version=os.getenv("KOGNITION_API_VERSION", "2024-12-01-preview"),
            model_gateway_url=os.getenv("KOGNITION_MODEL_GATEWAY_URL", ""),
            model_invocation_profile=os.getenv("KOGNITION_MODEL_INVOCATION_PROFILE", "false").lower() == "true",
            system_prompt=os.getenv("KOGNITION_SYSTEM_PROMPT", "You are a helpful assistant."),
            max_tokens=(
                int(os.environ["KOGNITION_MAX_TOKENS"]) if "KOGNITION_MAX_TOKENS" in os.environ
                else None if os.getenv("KOGNITION_MODEL_INVOCATION_PROFILE", "false").lower() == "true" else 4096
            ),
            temperature=(
                float(os.environ["KOGNITION_TEMPERATURE"]) if "KOGNITION_TEMPERATURE" in os.environ
                else None if os.getenv("KOGNITION_MODEL_INVOCATION_PROFILE", "false").lower() == "true" else 0.7
            ),
            timeout_seconds=_parse_timeout_seconds(os.getenv("KOGNITION_TIMEOUT_SECONDS", "30")),
            model_retry_max_attempts=max(1, int(os.getenv("KOGNITION_MODEL_RETRY_MAX_ATTEMPTS", "3"))),
            model_retry_initial_delay_seconds=max(0.0, float(os.getenv("KOGNITION_MODEL_RETRY_INITIAL_DELAY_SECONDS", "0.5"))),
            model_retry_max_delay_seconds=max(0.0, float(os.getenv("KOGNITION_MODEL_RETRY_MAX_DELAY_SECONDS", "4.0"))),
            tools_json=os.getenv("KOGNITION_TOOLS_JSON", "[]"),
            tools_resolved_json=os.getenv("KOGNITION_TOOLS_RESOLVED", "[]"),
            tool_gateway_url=os.getenv("KOGNITION_TOOL_GATEWAY_URL", ""),
            agent_communication_gateway_url=os.getenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL", ""),
            agent_delegation_enabled=os.getenv("KOGNITION_AGENT_DELEGATION_ENABLED", "true").lower() in ("true", "1", "yes"),
            memory_service_url=os.getenv("KOGNITION_MEMORY_SERVICE_URL", ""),
            memory_enabled=os.getenv("KOGNITION_MEMORY_ENABLED", "true").lower() in ("true", "1", "yes"),
            memory_volume=os.getenv("KOGNITION_MEMORY_VOLUME", ""),
            kb_resolved_json=os.getenv("KOGNITION_KB_RESOLVED", "[]"),
            knowledge_gateway_url=os.getenv("KOGNITION_KNOWLEDGE_GATEWAY_URL", ""),
            knowledge_gateway_ca_file=os.getenv("KOGNITION_KNOWLEDGE_GATEWAY_CA_FILE", ""),
            port=int(os.getenv("KOGNITION_PORT", "8080")),
            log_level=os.getenv("KOGNITION_LOG_LEVEL", "INFO"),
            control_tls_certificate=os.getenv("KOGNITION_CONTROL_TLS_CERT_FILE", ""),
            control_tls_key=os.getenv("KOGNITION_CONTROL_TLS_KEY_FILE", ""),
        )
        if bool(configuration.control_tls_certificate) != bool(configuration.control_tls_key):
            raise ValueError("worker control TLS requires both certificate and key")
        if configuration.workload_identity_mode not in ("capability", "spire"):
            raise ValueError("unsupported workload identity profile")
        if configuration.workload_identity_mode == "spire":
            from urllib.parse import urlparse

            endpoint = urlparse(configuration.workload_proxy_url)
            try:
                local = ipaddress.ip_address(endpoint.hostname or "").is_loopback
            except ValueError:
                local = False
            if endpoint.scheme != "http" or not local or endpoint.username or endpoint.password or endpoint.query or endpoint.fragment or endpoint.path not in ("", "/"):
                raise ValueError("SPIRE transport requires a literal loopback origin")
            origin = configuration.workload_proxy_url.rstrip("/")
            configuration.model_gateway_url = origin + "/model"
            configuration.tool_gateway_url = origin + "/tool"
            configuration.memory_service_url = origin + "/memory"
            configuration.agent_communication_gateway_url = origin + "/communication"
        return configuration


def _parse_timeout_seconds(raw: str) -> float:
    """Parse a positive bare-second or Go-style duration for provider clients."""
    value = raw.strip()
    if not value:
        raise ValueError("KOGNITION_TIMEOUT_SECONDS must not be empty")
    try:
        seconds = float(value)
    except ValueError:
        seconds = 0.0
        position = 0
        for match in re.finditer(r"(\d+(?:\.\d+)?)(ms|s|m|h)", value):
            if match.start() != position:
                raise ValueError(f"invalid timeout duration {raw!r}")
            magnitude = float(match.group(1))
            seconds += magnitude * {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[match.group(2)]
            position = match.end()
        if position != len(value):
            raise ValueError(f"invalid timeout duration {raw!r}")
    if seconds <= 0:
        raise ValueError("KOGNITION_TIMEOUT_SECONDS must be positive")
    return seconds
