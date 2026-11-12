# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""LLM provider factory — creates the right LangChain model based on config."""

import logging
import os
import re
from contextlib import contextmanager
from contextvars import ContextVar
from typing import Callable
from urllib.parse import urlparse, urlunparse

from langchain_core.language_models import BaseChatModel

from config import AgentConfig

logger = logging.getLogger(__name__)

_model_session_capability: ContextVar[str] = ContextVar("model_session_capability", default="")


@contextmanager
def model_runtime_session_capability(capability: str):
    token = _model_session_capability.set(capability)
    try:
        yield
    finally:
        _model_session_capability.reset(token)


def _azure_resource_root(endpoint: str) -> str:
    """Strip Azure OpenAI path suffix (e.g. /openai/deployments/<id>) and Foundry
    project path (/api/projects/<name>) so the SDK can build the correct URL itself.
    AzureChatOpenAI expects only the resource root."""
    if not endpoint:
        return endpoint
    parsed = urlparse(endpoint)
    if not parsed.scheme or not parsed.netloc:
        return endpoint
    # Drop everything from /openai onward (case-insensitive)
    path = re.sub(r"(?i)/openai(/.*)?$", "", parsed.path or "")
    # Drop Foundry project path (/api/projects/<name>)
    path = re.sub(r"(?i)/api/projects(/[^/]+)?$", "", path)
    return urlunparse((parsed.scheme, parsed.netloc, path, "", "", ""))


def _model_gateway_base_url(endpoint: str) -> str:
    """Return an OpenAI-compatible base URL for the Model Gateway."""
    normalized = endpoint.rstrip("/")
    if normalized.endswith("/v1"):
        return normalized
    return f"{normalized}/v1"


def _azure_ad_token_provider(scope: str = "https://cognitiveservices.azure.com/.default") -> Callable[[], str]:
    """Create a token provider for Azure Cognitive Services.

    Supports client-secret, managed identity, and workload-identity federation
    (see azure_creds.build_azure_credential), falling back to
    DefaultAzureCredential so no explicit secret is required on Azure."""
    from azure_creds import azure_ad_token_provider

    return azure_ad_token_provider(scope)


def _openai_generation_parameters(config: AgentConfig) -> dict[str, int | float | str]:
    if config.model_invocation_profile:
        parameters: dict[str, int | float | str] = {}
        if config.max_tokens is not None:
            parameters["max_tokens"] = config.max_tokens
        if config.temperature is not None:
            parameters["temperature"] = config.temperature
        return parameters
    if config.model_id.lower().startswith("gpt-6"):
        return {"max_completion_tokens": config.max_tokens, "reasoning_effort": "none"}
    if config.model_id.lower().startswith("gpt-5"):
        return {"max_completion_tokens": config.max_tokens}
    return {"max_tokens": config.max_tokens, "temperature": config.temperature}


def create_model(config: AgentConfig) -> BaseChatModel:
    """Create a LangChain chat model based on the provider configuration."""

    provider = config.model_provider.lower()
    if config.model_invocation_profile and not config.model_gateway_url:
        raise ValueError("Profile-backed Python execution requires the Model Gateway")

    if config.model_gateway_url:
        if provider not in ("openai", "openai-compatible", "foundry", "ollama"):
            raise ValueError("Model Gateway currently supports only OpenAI-compatible, Foundry, or Ollama models")
        capability = _model_session_capability.get()
        if not capability:
            raise ValueError("Model Gateway session capability is required for model inference")
        from langchain_openai import ChatOpenAI
        return ChatOpenAI(**{
            "model": "kognition-model" if config.model_invocation_profile else config.model_id,
            "base_url": _model_gateway_base_url(config.model_gateway_url),
            "api_key": capability,
            **({"use_responses_api": False} if "use_responses_api" in getattr(ChatOpenAI, "model_fields", {}) else {}),
            **_openai_generation_parameters(config),
            "timeout": config.timeout_seconds,
        })

    if provider in ("openai", "openai-compatible"):
        from langchain_openai import ChatOpenAI

        api_key = config.api_key
        # When no explicit API key is set but Azure credentials are available,
        # acquire a token for Azure Cognitive Services and use it as the API key.
        if not api_key and os.getenv("AZURE_TENANT_ID"):
            logger.info("openai provider: using Azure AD token as API key")
            api_key = _azure_ad_token_provider("https://cognitiveservices.azure.com/.default")()

        kwargs = {
            "model": config.model_id,
            "api_key": api_key,
            **_openai_generation_parameters(config),
            "timeout": config.timeout_seconds,
        }
        # Support custom endpoints (e.g., Azure OpenAI, local vLLM)
        if config.model_endpoint and config.model_endpoint != "https://api.openai.com/v1":
            kwargs["base_url"] = config.model_endpoint
        # Azure endpoints require api-version as a query parameter.
        if config.api_version and config.model_endpoint and "azure.com" in config.model_endpoint:
            kwargs["default_query"] = {"api-version": config.api_version}

        return ChatOpenAI(**kwargs)

    elif provider == "anthropic":
        from langchain_anthropic import ChatAnthropic

        return ChatAnthropic(**{
            "model": config.model_id,
            "api_key": config.api_key,
            "max_tokens": config.max_tokens,
            "temperature": config.temperature,
            "timeout": float(config.timeout_seconds),
        })

    elif provider == "ollama":
        from langchain_openai import ChatOpenAI

        # Ollama exposes an OpenAI-compatible API
        return ChatOpenAI(**{  # pyright: ignore[reportCallIssue]
            "model": config.model_id,
            "base_url": config.model_endpoint or "http://localhost:11434/v1",
            "api_key": "ollama",  # Ollama doesn't require a real key
            "max_tokens": config.max_tokens,
            "temperature": config.temperature,
            "timeout": config.timeout_seconds,
        })

    elif provider in ("azure", "azure-openai", "foundry"):
        from langchain_openai import AzureChatOpenAI

        azure_endpoint = _azure_resource_root(config.model_endpoint)
        if azure_endpoint != config.model_endpoint:
            logger.info(
                "normalized azure endpoint: %s -> %s",
                config.model_endpoint, azure_endpoint,
            )

        kwargs = {
            "azure_deployment": config.model_id,
            "azure_endpoint": azure_endpoint,
            "api_version": config.api_version,
            **_openai_generation_parameters(config),
            "timeout": config.timeout_seconds,
        }

        if config.api_key:
            kwargs["api_key"] = config.api_key
        elif os.getenv("AZURE_TENANT_ID"):
            # Both Foundry (services.ai.azure.com) and Cognitive Services
            # (cognitiveservices.azure.com) endpoints use the same scope
            # when hitting the /openai path directly (without /api/projects).
            scope = "https://cognitiveservices.azure.com/.default"
            logger.info("azure provider: using AD token with scope %s", scope)
            kwargs["azure_ad_token_provider"] = _azure_ad_token_provider(scope)
        else:
            raise ValueError(
                "Azure provider requires either KOGNITION_API_KEY or "
                "AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET env vars"
            )

        return AzureChatOpenAI(**kwargs)  # pyright: ignore[reportCallIssue]

    else:
        # Fallback: treat as OpenAI-compatible with custom endpoint
        logger.warning(
            "Unknown provider '%s', falling back to OpenAI-compatible", provider
        )
        from langchain_openai import ChatOpenAI

        return ChatOpenAI(**{  # pyright: ignore[reportCallIssue]
            "model": config.model_id,
            "base_url": config.model_endpoint,
            "api_key": config.api_key,
            "max_tokens": config.max_tokens,
            "temperature": config.temperature,
            "timeout": config.timeout_seconds,
        })
