# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Startup configuration binding for operator-managed HTTP runtimes."""

import json


CONFIGURATION_KEYS = frozenset({
    "KOGNITION_WORKLOAD_IDENTITY_MODE", "KOGNITION_WORKLOAD_PROXY_URL",
    "KOGNITION_RUNTIME_CLASS",
    "KOGNITION_NAMESPACE", "KOGNITION_AGENT_NAME", "KOGNITION_MODEL_PROVIDER",
    "KOGNITION_MODEL_ID", "KOGNITION_MODEL_GATEWAY_URL", "KOGNITION_SYSTEM_PROMPT",
    "KOGNITION_MODEL_INVOCATION_PROFILE",
    "KOGNITION_MAX_TOKENS", "KOGNITION_TIMEOUT_SECONDS", "KOGNITION_TOOLS_JSON",
    "KOGNITION_TOOLS_RESOLVED", "KOGNITION_MEMORY_ENABLED", "KOGNITION_MEMORY_VOLUME",
    "KOGNITION_MEMORY_SERVICE_URL", "KOGNITION_TOOL_GATEWAY_URL",
    "KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL", "KOGNITION_AGENT_DELEGATION_ENABLED", "KOGNITION_KB_RESOLVED",
    "KOGNITION_KNOWLEDGE_GATEWAY_URL", "KOGNITION_KNOWLEDGE_GATEWAY_CA_FILE",
    "KOGNITION_TEMPERATURE", "KOGNITION_MODEL_RETRY_MAX_ATTEMPTS",
    "KOGNITION_MODEL_RETRY_INITIAL_DELAY_SECONDS", "KOGNITION_MODEL_RETRY_MAX_DELAY_SECONDS",
})


def startup_configuration(environment):
    return {name: environment[name] for name in CONFIGURATION_KEYS if name in environment}


def matches_configuration(serialized, loaded):
    try:
        expected = json.loads(serialized)
    except (ValueError, TypeError):
        return False
    return (
        isinstance(expected, dict)
        and bool(expected.get("KOGNITION_NAMESPACE"))
        and bool(expected.get("KOGNITION_AGENT_NAME"))
        and bool(expected.get("KOGNITION_MODEL_GATEWAY_URL"))
        and expected == loaded
    )