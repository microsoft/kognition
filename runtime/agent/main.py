# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Kognition Agent Runtime — FastAPI server with LangGraph agent execution."""

import asyncio
import hmac
import json
import logging
import os
import time
from collections import OrderedDict
from contextlib import asynccontextmanager, contextmanager
from enum import Enum
from typing import Optional

from fastapi import FastAPI, Header, Response
from fastapi.responses import JSONResponse
from langchain_core.messages import AIMessage, BaseMessage, HumanMessage
from opentelemetry.trace import SpanContext
from pydantic import BaseModel

# Maximum number of conversations to keep in memory and turns per conversation.
# We cap both to bound memory; an LRU eviction keeps the most-recent chats.
_MAX_CONVERSATIONS = 64
_MAX_TURNS_PER_CONVERSATION = 40

# Number of completed session results to retain so late /status polls from the
# controller can still fetch the result after a newer session has taken over
# the singleton runtime. Sized to comfortably cover parallel-workflow fan-outs.
_MAX_COMPLETED_SESSIONS = 64

# Conversation trace roots are real spans around the first turn. Later turns
# attach to their saved span context so Jaeger sees one trace with a real parent.
_MAX_CONVERSATION_TRACES = 64

from config import AgentConfig
from runtime_binding import matches_configuration, startup_configuration
from graph import build_agent_graph
from memory import memory_runtime_capability
from tools import agent_delegation_runtime_session_capability, knowledge_runtime_session_capability, tool_runtime_session_capability
from providers import model_runtime_session_capability
import runtime_tracing


def build_runtime_graph(config: AgentConfig):
    if config.runtime_class in ("", "default", "native"):
        return build_agent_graph(config)
    if config.runtime_class == "autogen":
        from autogen_graph import build_autogen_graph

        return build_autogen_graph(config)
    raise ValueError("unsupported runtime class")

# Load configuration from environment
config = AgentConfig.from_env()
external_runtime = os.getenv("KOGNITION_EXTERNAL_RUNTIME", "false").lower() == "true"
loaded_configuration = startup_configuration(os.environ)
if external_runtime and (not config.runtime_auth_token or not config.model_gateway_url):
    raise ValueError("external runtime requires runtime authentication and Model Gateway")

# Configure logging
logging.basicConfig(
    level=getattr(logging, config.log_level.upper(), logging.INFO),
    format="%(asctime)s %(levelname)s [%(name)s] %(message)s",
)
logger = logging.getLogger("kognition.runtime")


class RuntimePhase(str, Enum):
    STARTING = "Starting"
    READY = "Ready"
    RUNNING = "Running"
    COMPLETED = "Completed"
    FAILED = "Failed"


class RuntimeState:
    """Mutable runtime state for the agent pod."""

    def __init__(self):
        self.phase: RuntimePhase = RuntimePhase.STARTING
        self.start_time: float = time.time()
        self.token_usage: dict = {
            "prompt_tokens": 0,
            "completion_tokens": 0,
            "total_tokens": 0,
        }
        self.tool_calls: int = 0
        # Per-turn tool call detail records — replaced on each /invoke so the
        # studio UI shows only the latest turn's calls. Each record is a dict
        # with keys: name, args, result, duration_ms, optional error.
        self.tool_calls_detail: list[dict] = []
        self.output: str = ""
        self.error: str = ""
        self.invocation_count: int = 0
        self.current_session_id: str = ""
        self.current_conversation_id: str = ""
        self.current_spiffe_id: str = ""
        self.current_memory_capability: str = ""
        self.current_tool_session_capability: str = ""
        self.current_model_session_capability: str = ""
        self.current_agent_delegation_session_capability: str = ""
        self.current_knowledge_session_capability: str = ""
        self.current_task: Optional[asyncio.Task] = None
        self.current_task_cancellable: bool = False
        # LRU of conversation_id -> list[BaseMessage]. Bounded by
        # _MAX_CONVERSATIONS; each list is trimmed to _MAX_TURNS_PER_CONVERSATION.
        self.conversations: "OrderedDict[str, list[BaseMessage]]" = OrderedDict()
        # LRU of session_id -> completed-session snapshot dict. Written when a
        # session ends (COMPLETED or FAILED) so /status can still return the
        # result after another session took over the singleton runtime. Snapshot
        # keys: phase, output, error, token_usage, tool_calls, tool_calls_detail,
        # invocation_count.
        self.completed_sessions: "OrderedDict[str, dict]" = OrderedDict()
        self.conversation_trace_contexts: "OrderedDict[str, SpanContext]" = OrderedDict()
        self.bridge_sessions: dict[str, dict] = {}


state = RuntimeState()
agent_graph = None


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Application lifespan — build graph on startup."""
    global agent_graph
    runtime_tracing.configure_tracing(config)
    logger.info(
        "Starting Kognition Agent Runtime: agent=%s instance=%s provider=%s model=%s",
        config.agent_name,
        config.instance_name,
        config.model_provider,
        config.model_id,
    )
    try:
        if config.workload_identity_mode == "spire":
            import httpx

            async with httpx.AsyncClient(follow_redirects=False, timeout=5) as transport_client:
                ready = await transport_client.get(config.workload_proxy_url.rstrip("/") + "/readyz")
                if ready.status_code != 200:
                    raise ValueError("SPIRE workload transport is not ready")
        agent_graph = build_runtime_graph(config)
        state.phase = RuntimePhase.READY
        logger.info("Agent graph compiled successfully, runtime is READY")
    except Exception as e:
        state.phase = RuntimePhase.FAILED
        state.error = str(e)
        logger.error("Failed to build agent graph: %s", e)

    yield

    runtime_tracing.shutdown_tracing()
    logger.info("Agent runtime shutting down")


app = FastAPI(title="Kognition Agent Runtime", lifespan=lifespan)


# -- Health & Status Endpoints --


class HealthResponse(BaseModel):
    status: str
    phase: str
    uptime_seconds: float


class StatusResponse(BaseModel):
    phase: str
    agent_name: str
    instance_name: str
    session_id: str
    spiffe_id: str = ""
    invocation_count: int
    token_usage: dict
    tool_calls: int
    tool_calls_detail: list[dict] = []
    output: str
    error: str
    uptime_seconds: float


@app.get("/healthz", response_model=HealthResponse)
def healthz():
    """Kubernetes liveness probe."""
    return HealthResponse(
        status="ok" if state.phase != RuntimePhase.FAILED else "error",
        phase=state.phase.value,
        uptime_seconds=time.time() - state.start_time,
    )


@app.get("/readyz", response_model=HealthResponse)
def readyz():
    """Kubernetes readiness probe."""
    ready = state.phase in (RuntimePhase.READY, RuntimePhase.RUNNING, RuntimePhase.COMPLETED)
    return HealthResponse(
        status="ok" if ready else "not_ready",
        phase=state.phase.value,
        uptime_seconds=time.time() - state.start_time,
    )


def _require_runtime_auth(authorization: Optional[str]) -> None:
    if not config.runtime_auth_token:
        return
    expected = f"Bearer {config.runtime_auth_token}"
    if not authorization or not hmac.compare_digest(authorization, expected):
        from fastapi import HTTPException

        raise HTTPException(status_code=401, detail="runtime authorization is required")


@app.get("/status", response_model=StatusResponse)
def status(session_id: Optional[str] = None, authorization: Optional[str] = Header(default=None)):
    """Full agent status (polled by controller).

    The agent Deployment is shared across many AgentInstances and the pod holds
    a single global ``state.output``. When the caller passes ``?session_id=X``
    that does not match the session currently owning the runtime, first check
    the completed-sessions LRU so a queued /invoke that has since finished can
    still hand back its result. Otherwise return a Running-with-no-output
    placeholder so the caller keeps polling instead of grabbing another
    session's leftover completion. FAILED is scoped to the session that failed —
    a foreign session still sees the placeholder so the next /invoke can
    recover the pod (see /invoke).
    """
    _require_runtime_auth(authorization)
    if external_runtime and session_id and session_id != state.current_session_id and session_id not in state.completed_sessions:
        return JSONResponse(status_code=404, content={"detail": "Session outcome is unavailable"})
    if (
        session_id
        and state.current_session_id
        and session_id != state.current_session_id
    ):
        snapshot = state.completed_sessions.get(session_id)
        if snapshot is not None:
            state.completed_sessions.move_to_end(session_id)
            return StatusResponse(
                phase=snapshot["phase"],
                agent_name=config.agent_name,
                instance_name=config.instance_name,
                session_id=session_id,
                spiffe_id=snapshot.get("spiffe_id", ""),
                invocation_count=snapshot["invocation_count"],
                token_usage=snapshot["token_usage"],
                tool_calls=snapshot["tool_calls"],
                tool_calls_detail=snapshot["tool_calls_detail"],
                output=snapshot["output"],
                error=snapshot["error"],
                uptime_seconds=time.time() - state.start_time,
            )
        return StatusResponse(
            phase=RuntimePhase.RUNNING.value,
            agent_name=config.agent_name,
            instance_name=config.instance_name,
            session_id=session_id,
            spiffe_id="",
            invocation_count=state.invocation_count,
            token_usage={"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
            tool_calls=0,
            tool_calls_detail=[],
            output="",
            error="",
            uptime_seconds=time.time() - state.start_time,
        )
    return StatusResponse(
        phase=state.phase.value,
        agent_name=config.agent_name,
        instance_name=config.instance_name,
        session_id=state.current_session_id or config.session_id,
        spiffe_id=state.current_spiffe_id,
        invocation_count=state.invocation_count,
        token_usage=state.token_usage,
        tool_calls=state.tool_calls,
        tool_calls_detail=state.tool_calls_detail,
        output=state.output,
        error=state.error,
        uptime_seconds=time.time() - state.start_time,
    )


# -- Invocation Endpoint --


class InvokeRequest(BaseModel):
    message: str
    runtime_configuration: str = ""
    session_id: str = ""
    # Informational workload URI propagated by the controller. This is not an
    # authenticated SVID and must not be used as proof of caller identity.
    spiffe_id: str = ""
    memory_capability: str = ""
    tool_session_capability: str = ""
    model_session_capability: str = ""
    agent_delegation_session_capability: str = ""
    knowledge_session_capability: str = ""
    # Optional client-supplied identifier that groups turns into a single
    # multi-turn conversation. When set, the runtime preserves message history
    # across invocations sharing the same conversation_id, so the LLM sees prior
    # turns. When empty, each invocation is treated as a standalone turn (no
    # history) — matching the prior behaviour.
    conversation_id: str = ""


class InvokeAck(BaseModel):
    accepted: bool
    phase: str
    session_id: str
    invocation_count: int
    conversation_id: str = ""


@app.post("/configuration-check", status_code=204)
def configuration_check(expected: dict[str, str], authorization: Optional[str] = Header(default=None)):
    from fastapi import HTTPException

    _require_runtime_auth(authorization)
    if not external_runtime or not matches_configuration(json.dumps(expected), loaded_configuration):
        raise HTTPException(status_code=412, detail="external runtime configuration mismatch")
    return Response(status_code=204)


def _conversation_history(conversation_id: str) -> list[BaseMessage]:
    """Return the message history for a conversation, or [] if none/disabled.

    Touching a conversation marks it as most-recently-used in the LRU.
    """
    if not conversation_id:
        return []
    history = state.conversations.get(conversation_id)
    if history is None:
        return []
    # Mark as most-recently-used.
    state.conversations.move_to_end(conversation_id)
    return list(history)


def _archive_completed_session(session_id: str):
    """Snapshot the current session's outcome into the completed-sessions LRU.

    Called once a session reaches COMPLETED or FAILED so that a later /status
    poll targeting this session_id — even after a newer session has taken over
    the runtime and clobbered state.output — can still recover the result.
    """
    if not session_id:
        return
    while len(state.completed_sessions) >= _MAX_COMPLETED_SESSIONS:
        state.completed_sessions.popitem(last=False)
    state.completed_sessions[session_id] = {
        "phase": state.phase.value,
        "output": state.output,
        "error": state.error,
        "token_usage": dict(state.token_usage),
        "tool_calls": state.tool_calls,
        "tool_calls_detail": list(state.tool_calls_detail),
        "invocation_count": state.invocation_count,
        "spiffe_id": state.current_spiffe_id,
    }
    state.completed_sessions.move_to_end(session_id)


def _store_conversation_turn(
    conversation_id: str,
    user_message: BaseMessage,
    assistant_message: Optional[BaseMessage],
):
    """Append a turn (user + assistant) to a conversation's history with LRU+cap."""
    if not conversation_id:
        return
    history = state.conversations.get(conversation_id)
    if history is None:
        # Evict oldest to stay under cap.
        while len(state.conversations) >= _MAX_CONVERSATIONS:
            state.conversations.popitem(last=False)
        history = []
        state.conversations[conversation_id] = history
    history.append(user_message)
    if assistant_message is not None:
        history.append(assistant_message)
    # Trim oldest turns if the conversation grows past the cap (drop in pairs).
    excess = len(history) - _MAX_TURNS_PER_CONVERSATION * 2
    if excess > 0:
        del history[:excess]
    state.conversations.move_to_end(conversation_id)


@contextmanager
def _conversation_parent_context(conversation_id: str):
    """Attach or create the parent span context for a conversation."""
    if not conversation_id:
        yield
        return
    span_context = state.conversation_trace_contexts.get(conversation_id)
    if span_context is not None:
        state.conversation_trace_contexts.move_to_end(conversation_id)
        with runtime_tracing.attach_span_context(span_context):
            yield
        return

    attributes = {
        "kognition.agent.name": config.agent_name,
        "kognition.instance.name": config.instance_name,
        "kognition.namespace": config.namespace,
        "kognition.conversation.id": conversation_id,
    }
    with runtime_tracing.start_span("kognition.runtime.conversation", attributes=attributes) as span:
        span_context = span.get_span_context()
        while len(state.conversation_trace_contexts) >= _MAX_CONVERSATION_TRACES:
            state.conversation_trace_contexts.popitem(last=False)
        state.conversation_trace_contexts[conversation_id] = span_context
        yield


async def _run_agent(message: str, conversation_id: str, traceparent: str = "", tracestate: str = "", memory_capability: str = "", tool_session_capability: str = "", model_session_capability: str = "", agent_delegation_session_capability: str = "", knowledge_session_capability: str = ""):
    """Background task: execute the agent graph and update global state.

    If conversation_id is set, prior turns from that conversation are prepended
    to the message list so the LLM sees the full chat history.
    """
    parent_context = runtime_tracing.attach_traceparent(traceparent, tracestate)
    if not traceparent:
        parent_context = _conversation_parent_context(conversation_id)
    session_id = state.current_session_id

    with parent_context:
        attributes = {
            "kognition.agent.name": config.agent_name,
            "kognition.instance.name": config.instance_name,
            "kognition.namespace": config.namespace,
            "kognition.session.id": session_id,
            "kognition.conversation.id": conversation_id,
            "gen_ai.system": config.model_provider,
            "gen_ai.request.model": config.model_id,
        }
        with runtime_tracing.start_span("kognition.runtime.invoke", attributes=attributes) as span:
            user_message = HumanMessage(content=message)
            prior = _conversation_history(conversation_id)
            invoke_messages = prior + [user_message]
            try:
                with (
                    memory_runtime_capability(memory_capability),
                    tool_runtime_session_capability(tool_session_capability),
                    model_runtime_session_capability(model_session_capability),
                    agent_delegation_runtime_session_capability(agent_delegation_session_capability),
                    knowledge_runtime_session_capability(knowledge_session_capability),
                ):
                    invoke_state = {
                        "messages": invoke_messages,
                        "tool_calls_count": 0,
                        "tool_calls_detail": [],
                        "token_usage": {},
                        "memory_capability": memory_capability,
                        "tool_session_capability": tool_session_capability,
                        "model_session_capability": model_session_capability,
                        "agent_delegation_session_capability": agent_delegation_session_capability,
                        "knowledge_session_capability": knowledge_session_capability,
                    }
                    bridge_session = state.bridge_sessions.get(conversation_id)
                    if bridge_session is not None and bridge_session.get("sdk_state") is not None:
                        invoke_state["autogen_state"] = bridge_session["sdk_state"]
                    async_stream = getattr(agent_graph, "astream", None)
                    if callable(async_stream):
                        result = invoke_state
                        async for graph_state in async_stream(invoke_state, stream_mode="values"):
                            result = graph_state
                            if state.current_session_id == session_id:
                                state.token_usage = dict(graph_state.get("token_usage", {}) or {})
                                state.tool_calls = graph_state.get("tool_calls_count", 0)
                                state.tool_calls_detail = list(graph_state.get("tool_calls_detail", []) or [])
                    else:
                        result = await asyncio.to_thread(agent_graph.invoke, invoke_state)

                messages = result.get("messages", [])
                last_message = messages[-1] if messages else None
                raw = last_message.content if last_message is not None else ""
                if isinstance(raw, list):
                    parts = []
                    for block in raw:
                        if isinstance(block, str):
                            parts.append(block)
                        elif isinstance(block, dict):
                            text = block.get("text")
                            if isinstance(text, str):
                                parts.append(text)
                    output = "".join(parts)
                else:
                    output = raw if isinstance(raw, str) else str(raw)
                token_usage = result.get("token_usage", {}) or {}
                tool_calls = result.get("tool_calls_count", 0)
                tool_calls_detail = result.get("tool_calls_detail", []) or []

                span.set_attribute("gen_ai.usage.input_tokens", token_usage.get("prompt_tokens", 0))
                span.set_attribute("gen_ai.usage.output_tokens", token_usage.get("completion_tokens", 0))
                span.set_attribute("kognition.tool.calls", tool_calls)
                span.set_attribute("kognition.output.length", len(output))

                state.output = output
                state.tool_calls = tool_calls
                state.tool_calls_detail = tool_calls_detail
                state.token_usage = {
                    "prompt_tokens": token_usage.get("prompt_tokens", 0),
                    "completion_tokens": token_usage.get("completion_tokens", 0),
                    "total_tokens": token_usage.get("total_tokens", 0),
                }
                state.phase = RuntimePhase.COMPLETED
                if bridge_session is not None:
                    bridge_session["approval_state"] = result.get("runtime_approval_state", "")
                    bridge_session["sdk_state"] = result.get("autogen_state") if not bridge_session["approval_state"] else None
                _archive_completed_session(session_id)

                # Persist this turn into the conversation history (if any).
                assistant_message: Optional[BaseMessage] = None
                if isinstance(last_message, AIMessage):
                    assistant_message = last_message
                elif output:
                    assistant_message = AIMessage(content=output)
                _store_conversation_turn(conversation_id, user_message, assistant_message)

                logger.info(
                    "Invocation completed: tokens=%d tools=%d output_len=%d conversation=%s history_turns=%d",
                    token_usage.get("total_tokens", 0),
                    tool_calls,
                    len(output),
                    conversation_id or "-",
                    len(state.conversations.get(conversation_id, [])) // 2 if conversation_id else 0,
                )
            except asyncio.CancelledError:
                state.phase = RuntimePhase.FAILED
                state.error = "session cancelled by controller"
                _archive_completed_session(session_id)
                logger.info("Invocation cancelled: session=%s", session_id)
                raise
            except Exception as e:
                runtime_tracing.record_exception(span, e)
                state.phase = RuntimePhase.FAILED
                state.error = str(e)
                _archive_completed_session(state.current_session_id)
                logger.exception("Agent invocation failed: %s", e)
            finally:
                if state.current_task is asyncio.current_task():
                    state.current_task = None
                    state.current_task_cancellable = False


@app.post("/invoke", response_model=InvokeAck, status_code=202)
async def invoke(
    req: InvokeRequest,
    authorization: Optional[str] = Header(default=None),
    traceparent: Optional[str] = Header(default=""),
    tracestate: Optional[str] = Header(default=""),
):
    """Asynchronously kick off the agent invocation.

    Session semantics: the agent runtime is a singleton process inside the pod,
    but the agent Deployment is shared across multiple AgentInstances. Each
    /invoke call carries the controller's session_id. When a new session_id
    arrives, the prior result is cleared and a fresh graph execution starts.
    Repeated calls with the same session_id are idempotent (no-op) so reconciler
    retries don't double-invoke. FAILED is scoped to the session that failed:
    a new session_id resets the pod; retrying the same failed session is
    rejected.
    """
    from fastapi import HTTPException

    _require_runtime_auth(authorization)
    if agent_graph is None:
        detail = "Agent graph not initialized"
        if state.error:
            detail = f"Agent graph not initialized: {state.error}"
        raise HTTPException(status_code=503, detail=detail)

    if external_runtime and (
        not req.session_id or not req.model_session_capability
        or not matches_configuration(req.runtime_configuration, loaded_configuration)
    ):
        raise HTTPException(status_code=412, detail="external runtime invocation binding is required")

    incoming = req.session_id or ""
    snapshot = state.completed_sessions.get(incoming)
    if snapshot is not None:
        state.completed_sessions.move_to_end(incoming)
        if snapshot["phase"] == RuntimePhase.FAILED.value:
            raise HTTPException(status_code=503, detail=f"Agent failed: {snapshot['error']}")
        return InvokeAck(
            accepted=False,
            phase=snapshot["phase"],
            session_id=incoming,
            invocation_count=snapshot["invocation_count"],
        )
    same_session = incoming != "" and incoming == state.current_session_id

    if state.phase == RuntimePhase.FAILED and (same_session or incoming == ""):
        # Retrying the dead session — or a probe with no session id we could
        # disambiguate. Either way, surface the failure.
        raise HTTPException(status_code=503, detail=f"Agent failed: {state.error}")

    if same_session and state.phase in (RuntimePhase.RUNNING, RuntimePhase.COMPLETED):
        return InvokeAck(
            accepted=False,
            phase=state.phase.value,
            session_id=state.current_session_id,
            invocation_count=state.invocation_count,
        )

    if state.phase == RuntimePhase.RUNNING and not same_session:
        # A different session arrived while the previous one is still in flight.
        # Reject so the controller can retry once the prior run drains.
        raise HTTPException(
            status_code=409,
            detail=f"runtime busy with session {state.current_session_id}",
        )

    state.current_session_id = incoming
    state.current_conversation_id = req.conversation_id or ""
    state.current_spiffe_id = req.spiffe_id or ""
    state.current_memory_capability = req.memory_capability or ""
    state.current_tool_session_capability = req.tool_session_capability or ""
    state.current_model_session_capability = req.model_session_capability or ""
    state.current_agent_delegation_session_capability = req.agent_delegation_session_capability or ""
    state.current_knowledge_session_capability = req.knowledge_session_capability or ""
    logger.info(
        "Accepted runtime invocation: session=%s memory_capability=%s tool_session_capability=%s model_session_capability=%s agent_delegation_session_capability=%s",
        incoming,
        bool(state.current_memory_capability),
        bool(state.current_tool_session_capability),
        bool(state.current_model_session_capability),
        bool(state.current_agent_delegation_session_capability),
    )
    state.phase = RuntimePhase.RUNNING
    state.invocation_count += 1
    state.output = ""
    state.error = ""
    state.token_usage = {
        "prompt_tokens": 0,
        "completion_tokens": 0,
        "total_tokens": 0,
    }
    state.tool_calls = 0
    state.tool_calls_detail = []

    state.current_task_cancellable = callable(getattr(agent_graph, "astream", None))
    state.current_task = asyncio.create_task(_run_agent(req.message, state.current_conversation_id, traceparent or "", tracestate or "", memory_capability=state.current_memory_capability, tool_session_capability=state.current_tool_session_capability, model_session_capability=state.current_model_session_capability, agent_delegation_session_capability=state.current_agent_delegation_session_capability, knowledge_session_capability=state.current_knowledge_session_capability))

    return InvokeAck(
        accepted=True,
        phase=state.phase.value,
        session_id=state.current_session_id,
        invocation_count=state.invocation_count,
        conversation_id=state.current_conversation_id,
    )


class CancelRequest(BaseModel):
    session_id: str
    reason: str = ""


class CancelAck(BaseModel):
    accepted: bool
    session_id: str


@app.post("/cancel", response_model=CancelAck)
async def cancel(req: CancelRequest, authorization: Optional[str] = Header(default=None)):
    """Cooperatively cancel the matching in-flight runtime session."""
    from fastapi import HTTPException

    _require_runtime_auth(authorization)
    if not req.session_id or req.session_id != state.current_session_id:
        raise HTTPException(status_code=404, detail="runtime session not found")
    task = state.current_task
    if state.phase != RuntimePhase.RUNNING or task is None or task.done():
        return CancelAck(accepted=False, session_id=req.session_id)
    if not state.current_task_cancellable:
        raise HTTPException(status_code=409, detail="runtime execution does not support cooperative cancellation")
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass
    snapshot = state.completed_sessions.get(req.session_id)
    if snapshot is None or snapshot["phase"] != RuntimePhase.FAILED.value:
        raise HTTPException(status_code=409, detail="runtime session did not acknowledge cancellation")
    logger.info("Cancellation requested: session=%s reason=%s", req.session_id, req.reason)
    return CancelAck(accepted=True, session_id=req.session_id)


class BridgeSessionRequest(BaseModel):
    model_config = {"extra": "forbid"}
    agent_name: str
    namespace: str
    runtime_configuration: str
    session_id: str = ""
    instance_uid: str = ""


class BridgeStepRequest(BaseModel):
    model_config = {"extra": "forbid"}
    message: str
    turn_id: str
    model_session_capability: str
    memory_capability: str = ""
    tool_session_capability: str = ""
    agent_delegation_session_capability: str = ""
    knowledge_session_capability: str = ""


class BridgeCheckpoint(BridgeSessionRequest):
    version: int
    sdk_state: dict
    restore_bound: bool = False


def _require_bridge(authorization):
    from fastapi import HTTPException

    _require_runtime_auth(authorization)
    if not external_runtime or config.runtime_class != "autogen" or agent_graph is None:
        raise HTTPException(status_code=503, detail="AutoGen session bridge is unavailable")


def _bridge_binding(req):
    from fastapi import HTTPException

    if (
        req.agent_name != loaded_configuration.get("KOGNITION_AGENT_NAME")
        or req.namespace != loaded_configuration.get("KOGNITION_NAMESPACE")
        or len(req.runtime_configuration.encode("utf-8")) > 65536
        or not matches_configuration(req.runtime_configuration, loaded_configuration)
    ):
        raise HTTPException(status_code=412, detail="session configuration mismatch")


def _bridge_session(session_id):
    from fastapi import HTTPException

    session = state.bridge_sessions.get(session_id)
    if session is None:
        raise HTTPException(status_code=404, detail="runtime session not found")
    return session


@app.post("/sessions", status_code=201)
async def create_bridge_session(req: BridgeSessionRequest, authorization: Optional[str] = Header(default=None)):
    from fastapi import HTTPException
    from uuid import UUID, uuid4

    _require_bridge(authorization)
    _bridge_binding(req)
    if req.session_id:
        try:
            parsed = UUID(req.session_id)
            if not parsed.int or str(parsed) != req.session_id or not req.instance_uid or len(req.instance_uid) > 128:
                raise ValueError("invalid bound session")
        except ValueError:
            raise HTTPException(status_code=400, detail="invalid controller session binding") from None
        existing = state.bridge_sessions.get(req.session_id)
        if existing is not None:
            if existing["binding"] != req.model_dump():
                raise HTTPException(status_code=409, detail="controller session binding conflict")
            return {"session_id": req.session_id}
    elif req.instance_uid:
        raise HTTPException(status_code=400, detail="instance binding requires a controller session")
    if len(state.bridge_sessions) >= 64:
        raise HTTPException(status_code=429, detail="runtime session capacity reached")
    session_id = req.session_id or str(uuid4())
    from autogen_agentchat.state import AssistantAgentState

    state.bridge_sessions[session_id] = {"binding": req.model_dump(), "sdk_state": AssistantAgentState().model_dump(), "turn_id": "", "cancelled": False, "approval_state": ""}
    return {"session_id": session_id}


@app.post("/sessions/restore", status_code=201)
async def restore_bridge_session(req: BridgeCheckpoint, authorization: Optional[str] = Header(default=None)):
    from fastapi import HTTPException

    _require_bridge(authorization)
    _bridge_binding(req)
    if req.version != 1 or not req.sdk_state or len(json.dumps(req.sdk_state).encode("utf-8")) > 4 << 20:
        raise HTTPException(status_code=400, detail="invalid runtime checkpoint")
    try:
        from autogen_agentchat.state import AssistantAgentState

        AssistantAgentState.model_validate(req.sdk_state)
    except Exception:
        raise HTTPException(status_code=400, detail="invalid SDK checkpoint") from None
    binding = req.model_dump(exclude={"version", "sdk_state", "restore_bound"})
    if req.restore_bound:
        if not req.session_id or not req.instance_uid:
            raise HTTPException(status_code=400, detail="bound restore requires immutable controller ownership")
        existing = state.bridge_sessions.get(req.session_id)
        if existing is not None and (existing["turn_id"] or existing["cancelled"] or existing["approval_state"] or state.phase == RuntimePhase.RUNNING):
            raise HTTPException(status_code=409, detail="bound restore requires an unused quiescent session")
    else:
        binding["session_id"], binding["instance_uid"] = "", ""
    response = await create_bridge_session(BridgeSessionRequest(**binding), authorization)
    state.bridge_sessions[response["session_id"]]["sdk_state"] = req.sdk_state
    return response


@app.post("/sessions/{session_id}/step", status_code=202)
async def step_bridge_session(session_id: str, req: BridgeStepRequest, authorization: Optional[str] = Header(default=None), traceparent: Optional[str] = Header(default=""), tracestate: Optional[str] = Header(default="")):
    from fastapi import HTTPException

    _require_bridge(authorization)
    session = _bridge_session(session_id)
    if session["cancelled"] or session["approval_state"]:
        raise HTTPException(status_code=409, detail="session requires controller recovery")
    if session["binding"]["session_id"]:
        if req.turn_id != session["binding"]["session_id"]:
            raise HTTPException(status_code=409, detail="controller turn binding conflict")
        if session["turn_id"]:
            return {"accepted": False, "session_id": session_id}
    if not req.turn_id or len(req.turn_id) > 128 or not req.model_session_capability or len(req.message.encode("utf-8")) > 65536:
        raise HTTPException(status_code=400, detail="invalid runtime turn")
    if state.phase == RuntimePhase.RUNNING:
        raise HTTPException(status_code=409, detail="runtime is busy")
    if req.turn_id in state.completed_sessions or req.turn_id == state.current_session_id:
        raise HTTPException(status_code=409, detail="runtime turn ID already used")
    result = await invoke(InvokeRequest(
        **req.model_dump(exclude={"turn_id"}), session_id=req.turn_id,
        conversation_id=session_id, runtime_configuration=session["binding"]["runtime_configuration"],
    ), authorization, traceparent or "", tracestate or "")
    session["turn_id"] = req.turn_id
    return result


@app.get("/sessions/{session_id}/status")
async def bridge_session_status(session_id: str, authorization: Optional[str] = Header(default=None)):
    _require_bridge(authorization)
    session = _bridge_session(session_id)
    if not session["turn_id"]:
        return {"phase": "Failed" if session["cancelled"] else "Ready", "session_id": session_id, "agent_name": session["binding"]["agent_name"], "instance_uid": session["binding"]["instance_uid"], "error": "Session cancelled" if session["cancelled"] else "", "output": "", "tool_calls_detail": [], "token_usage": {}}
    result = status(session_id=session["turn_id"], authorization=authorization)
    return {**result.model_dump(), "instance_uid": session["binding"]["instance_uid"], "runtime_approval_state": session["approval_state"]}


@app.get("/sessions/{session_id}/checkpoint")
async def checkpoint_bridge_session(session_id: str, authorization: Optional[str] = Header(default=None)):
    from fastapi import HTTPException

    _require_bridge(authorization)
    session = _bridge_session(session_id)
    snapshot = state.completed_sessions.get(session["turn_id"])
    completed_turn = snapshot is not None and snapshot["phase"] == "Completed"
    if state.phase == RuntimePhase.RUNNING or session["approval_state"] or session["cancelled"] or (session["turn_id"] and not completed_turn) or not session["sdk_state"]:
        raise HTTPException(status_code=409, detail="runtime session is not checkpointable")
    return BridgeCheckpoint(**session["binding"], version=1, sdk_state=session["sdk_state"])


@app.post("/sessions/{session_id}/cancel")
async def cancel_bridge_session(session_id: str, authorization: Optional[str] = Header(default=None)):
    _require_bridge(authorization)
    session = _bridge_session(session_id)
    if session["turn_id"] == state.current_session_id and state.phase == RuntimePhase.RUNNING:
        await cancel(CancelRequest(session_id=session["turn_id"]), authorization)
    session["cancelled"] = True
    return {"accepted": True, "session_id": session_id}


@app.post("/sessions/{session_id}/release")
async def release_bridge_session(session_id: str, authorization: Optional[str] = Header(default=None)):
    from fastapi import HTTPException

    _require_bridge(authorization)
    session = state.bridge_sessions.get(session_id)
    if session is None:
        return {"accepted": True, "session_id": session_id}
    snapshot = state.completed_sessions.get(session["turn_id"])
    if state.phase == RuntimePhase.RUNNING or not snapshot or snapshot["phase"] != "Completed" or session["approval_state"] or session["cancelled"]:
        raise HTTPException(status_code=409, detail="runtime session is not safely reclaimable")
    state.bridge_sessions.pop(session_id)
    state.conversations.pop(session_id, None)
    state.conversation_trace_contexts.pop(session_id, None)
    return {"accepted": True, "session_id": session_id}


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=config.port, ssl_certfile=config.control_tls_certificate or None, ssl_keyfile=config.control_tls_key or None)
