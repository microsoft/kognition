# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""LangGraph agent graph definition.

The graph is a ReAct loop: agent -> (tools -> agent)* -> end. Tools come
from the dispatcher-injected env vars (see ``tools.build_tools``); every
LLM-invokable capability is a Tool CR with an HTTP or MCP endpoint.
"""

import json
import logging
import asyncio
import time
from typing import Annotated, Any, TypedDict

from langchain_core.messages import AIMessage, BaseMessage, SystemMessage, ToolMessage
from langgraph.graph import END, StateGraph
from langgraph.graph.message import add_messages

from config import AgentConfig
from memory import build_memory_tools, memory_runtime_capability
from providers import create_model, model_runtime_session_capability
import runtime_tracing
from tools import (
    _build_kb_search_tool,
    agent_delegation_runtime_session_capability,
    build_agent_delegation_tool,
    build_tools,
    knowledge_runtime_session_capability,
    tool_runtime_session_capability,
)

logger = logging.getLogger(__name__)


# Cap per-field size in the tool_calls_detail records so a single chatty tool
# can't blow up the /status payload. Args/results are clamped to this many
# characters; longer values are suffixed with a truncation marker.
_TOOL_DETAIL_FIELD_MAX = 8192
_RETRIABLE_STATUS_CODES = {408, 409, 429, 500, 502, 503, 504}
_RETRIABLE_ERROR_MARKERS = (
    "connection",
    "rate limit",
    "timeout",
    "temporarily",
    "throttl",
    "too many requests",
    "unavailable",
    "overloaded",
    "reset by peer",
    "service unavailable",
)


def _clamp_for_detail(value: Any) -> Any:
    """Return a JSON-safe, length-bounded representation of ``value``."""
    if isinstance(value, str):
        text = value
    else:
        try:
            text = json.dumps(value, default=str)
            # Roundtrip dicts/lists so the UI gets structured data when possible.
            if isinstance(value, (dict, list)):
                if len(text) <= _TOOL_DETAIL_FIELD_MAX:
                    return value
        except (TypeError, ValueError):
            text = str(value)
    if len(text) > _TOOL_DETAIL_FIELD_MAX:
        text = text[:_TOOL_DETAIL_FIELD_MAX] + f"… [truncated, original {len(text)} chars]"
    return text


def _status_code(exc: Exception) -> int | None:
    """Extract an HTTP status code from common SDK exception shapes."""
    status = getattr(exc, "status_code", None)
    if isinstance(status, int):
        return status
    response = getattr(exc, "response", None)
    status = getattr(response, "status_code", None)
    if isinstance(status, int):
        return status
    return None


def _is_retriable_model_error(exc: Exception) -> bool:
    """Return true when a model failure is likely transient."""
    if isinstance(exc, (TimeoutError, ConnectionError)):
        return True
    status = _status_code(exc)
    if status in _RETRIABLE_STATUS_CODES:
        return True
    text = str(exc).lower()
    return any(marker in text for marker in _RETRIABLE_ERROR_MARKERS)


def _model_retry_delay(config: AgentConfig, attempt: int) -> float:
    initial = config.model_retry_initial_delay_seconds
    if initial <= 0:
        return 0.0
    max_delay = config.model_retry_max_delay_seconds
    delay = initial * (2 ** max(0, attempt - 1))
    if max_delay > 0:
        delay = min(delay, max_delay)
    return delay


def _invoke_model_with_retries(model: Any, messages: list[BaseMessage], config: AgentConfig, span: Any) -> BaseMessage:
    """Invoke the chat model, retrying transient failures before surfacing them."""
    max_attempts = max(1, config.model_retry_max_attempts)
    span.set_attribute("kognition.model.retry.max_attempts", max_attempts)
    for attempt in range(1, max_attempts + 1):
        span.set_attribute("kognition.model.retry.attempt", attempt)
        try:
            response = model.invoke(messages)
            span.set_attribute("kognition.model.retry.attempts", attempt)
            return response
        except Exception as exc:
            if attempt >= max_attempts or not _is_retriable_model_error(exc):
                span.set_attribute("kognition.model.retry.exhausted", attempt >= max_attempts)
                raise
            delay_seconds = _model_retry_delay(config, attempt)
            span.record_exception(
                exc,
                attributes={
                    "kognition.model.retry.attempt": attempt,
                    "kognition.model.retry.delay_ms": int(delay_seconds * 1000),
                },
            )
            logger.warning(
                "model call failed on attempt %d/%d; retrying in %.2fs: %s",
                attempt,
                max_attempts,
                delay_seconds,
                exc,
            )
            if delay_seconds > 0:
                time.sleep(delay_seconds)
    raise RuntimeError("model retry loop exited unexpectedly")


async def _await_sync_call(function: Any, *args: Any) -> Any:
    """Run a sync fallback without acknowledging cancellation before it stops."""
    task = asyncio.create_task(asyncio.to_thread(function, *args))
    try:
        return await asyncio.shield(task)
    except asyncio.CancelledError:
        await task
        raise


async def _ainvoke_model_with_retries(model: Any, messages: list[BaseMessage], config: AgentConfig, span: Any) -> BaseMessage:
    """Invoke the chat model asynchronously with cancellation-aware retries."""
    max_attempts = max(1, config.model_retry_max_attempts)
    span.set_attribute("kognition.model.retry.max_attempts", max_attempts)
    for attempt in range(1, max_attempts + 1):
        span.set_attribute("kognition.model.retry.attempt", attempt)
        try:
            async_invoke = getattr(model, "ainvoke", None)
            if callable(async_invoke):
                response = await async_invoke(messages)
            else:
                response = await _await_sync_call(model.invoke, messages)
            span.set_attribute("kognition.model.retry.attempts", attempt)
            return response
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            if attempt >= max_attempts or not _is_retriable_model_error(exc):
                span.set_attribute("kognition.model.retry.exhausted", attempt >= max_attempts)
                raise
            delay_seconds = _model_retry_delay(config, attempt)
            span.record_exception(
                exc,
                attributes={
                    "kognition.model.retry.attempt": attempt,
                    "kognition.model.retry.delay_ms": int(delay_seconds * 1000),
                },
            )
            logger.warning(
                "model call failed on attempt %d/%d; retrying in %.2fs: %s",
                attempt,
                max_attempts,
                delay_seconds,
                exc,
            )
            if delay_seconds > 0:
                await asyncio.sleep(delay_seconds)
    raise RuntimeError("model retry loop exited unexpectedly")


class AgentState(TypedDict):
    """State tracked through the agent graph."""

    messages: Annotated[list[BaseMessage], add_messages]
    tool_calls_count: int
    tool_calls_detail: Annotated[list[dict], lambda a, b: (a or []) + (b or [])]
    token_usage: dict
    memory_capability: str
    tool_session_capability: str
    model_session_capability: str
    agent_delegation_session_capability: str
    knowledge_session_capability: str


def build_agent_graph(config: AgentConfig):
    """Build a LangGraph agent graph from configuration."""
    model = None
    if not config.model_gateway_url:
        model = create_model(config)
    tools = build_tools(
        resolved_json=config.tools_resolved_json,
        fallback_json=config.tools_json,
        tool_gateway_url=config.tool_gateway_url,
    )
    delegation_tool = build_agent_delegation_tool(config.agent_communication_gateway_url) if config.agent_delegation_enabled else None
    if delegation_tool is not None:
        tools.append(delegation_tool)

    # Attach the knowledge-base search tool when the dispatcher injected
    # any resolved KBs. It multiplexes across per-KB pgvector stores and an
    # in-memory keyword fallback for inline sources.
    try:
        kb_records = json.loads(config.kb_resolved_json)
    except json.JSONDecodeError:
        kb_records = []
    kb_tool = _build_kb_search_tool(kb_records, config.knowledge_gateway_url, config.knowledge_gateway_ca_file)
    if kb_tool is not None:
        tools.append(kb_tool)
        logger.info("attached knowledge_search tool over %d KB record(s)", len(kb_records))

    # Attach memory recall/store tools when a memory volume is bound. Recall uses
    # the memory service's semantic search and returns an explicit "no relevant
    # memories" signal rather than confabulating from unrelated records.
    memory_tools = build_memory_tools(config)
    if memory_tools:
        tools.extend(memory_tools)
        logger.info("attached %d memory tool(s)", len(memory_tools))

    tools_by_name = {t.name: t for t in tools}

    async def agent_node(state: AgentState) -> dict:
        """Invoke the LLM with current message history."""
        messages = state["messages"]
        full_messages = [SystemMessage(content=config.system_prompt)] + messages

        # Model Gateway capabilities are session-scoped, while the graph is
        # shared by a runtime Pod. Construct its provider client per turn so it
        # reads the active ContextVar without mutating the Pod template.
        with model_runtime_session_capability(state.get("model_session_capability", "")):
            active_model = create_model(config) if config.model_gateway_url else model
        assert active_model is not None
        if tools:
            active_model = active_model.bind_tools(tools)

        with runtime_tracing.start_span(
            "kognition.runtime.agent.model",
            attributes={
                "gen_ai.system": config.model_provider,
                "gen_ai.request.model": config.model_id,
                "kognition.agent.name": config.agent_name,
                "kognition.message.count": len(full_messages),
            },
        ) as span:
            response = await _ainvoke_model_with_retries(active_model, full_messages, config, span)

            token_usage = state.get("token_usage", {})
            if hasattr(response, "response_metadata"):
                usage = response.response_metadata.get("token_usage", {})
                if usage:
                    span.set_attribute("gen_ai.usage.input_tokens", usage.get("prompt_tokens", 0))
                    span.set_attribute("gen_ai.usage.output_tokens", usage.get("completion_tokens", 0))
                    token_usage = {
                        "prompt_tokens": token_usage.get("prompt_tokens", 0)
                        + usage.get("prompt_tokens", 0),
                        "completion_tokens": token_usage.get("completion_tokens", 0)
                        + usage.get("completion_tokens", 0),
                        "total_tokens": token_usage.get("total_tokens", 0)
                        + usage.get("total_tokens", 0),
                    }

        return {
            "messages": [response],
            "token_usage": token_usage,
        }

    def should_continue(state: AgentState) -> str:
        messages = state["messages"]
        last_message = messages[-1]

        if isinstance(last_message, AIMessage) and last_message.tool_calls:
            return "tools"
        return "end"

    async def tool_node(state: AgentState) -> dict:
        """Execute tool calls by dispatching to the bound StructuredTools."""
        messages = state["messages"]
        last_message = messages[-1]
        assert isinstance(last_message, AIMessage)
        tool_calls_count = state.get("tool_calls_count", 0)

        tool_messages: list[BaseMessage] = []
        new_details: list[dict] = []
        for tool_call in last_message.tool_calls:
            tool_calls_count += 1
            name = tool_call["name"]
            args = tool_call.get("args") or {}
            tool = tools_by_name.get(name)
            started = time.time()
            error: str = ""
            with runtime_tracing.start_span(
                "kognition.runtime.agent.tool",
                attributes={
                    "tool.name": name,
                    "kognition.tool.call.id": tool_call.get("id", ""),
                },
            ) as span:
                if tool is None:
                    logger.warning("model called unknown tool '%s'", name)
                    content = f"Tool '{name}' is not available."
                    error = "tool not available"
                    span.set_attribute("error.type", error)
                else:
                    try:
                        with (
                            memory_runtime_capability(state.get("memory_capability", "")),
                            tool_runtime_session_capability(state.get("tool_session_capability", "")),
                            agent_delegation_runtime_session_capability(
                                state.get("agent_delegation_session_capability", "")
                            ),
                            knowledge_runtime_session_capability(
                                state.get("knowledge_session_capability", "")
                            ),
                        ):
                            async_invoke = getattr(tool, "ainvoke", None)
                            if callable(async_invoke):
                                result = await async_invoke(args)
                            else:
                                result = await _await_sync_call(tool.invoke, args)
                        content = str(result)
                    except Exception as exc:  # noqa: BLE001 - surface to model
                        logger.warning("tool '%s' raised: %s", name, exc)
                        content = f"Tool '{name}' failed: {exc}"
                        error = str(exc)
                        runtime_tracing.record_exception(span, exc)
                duration_ms = int((time.time() - started) * 1000)
                span.set_attribute("kognition.tool.duration_ms", duration_ms)
                span.set_attribute("kognition.tool.result.length", len(content))
            logger.info("tool call: %s(%s) -> %d chars", name, args, len(content))
            tool_messages.append(ToolMessage(content=content, tool_call_id=tool_call["id"]))
            detail = {
                "name": name,
                "args": _clamp_for_detail(args),
                "result": _clamp_for_detail(content),
                "duration_ms": duration_ms,
            }
            if error:
                detail["error"] = error
            new_details.append(detail)

        return {
            "messages": tool_messages,
            "tool_calls_count": tool_calls_count,
            "tool_calls_detail": new_details,
        }

    graph = StateGraph(AgentState)
    graph.add_node("agent", agent_node)
    graph.add_node("tools", tool_node)

    graph.set_entry_point("agent")
    graph.add_conditional_edges("agent", should_continue, {"tools": "tools", "end": END})
    graph.add_edge("tools", "agent")

    return graph.compile()
