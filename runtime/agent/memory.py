# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Agent memory tools: semantic recall and store over the memory service.

These tools let an agent persist notes to and recall notes from its bound
MemoryVolume via ``kognition-memory-service``. Recall uses the service's
server-side semantic search: the service embeds the query and returns the most
relevant records. When nothing is relevant the tool returns the exact text
``"No relevant memories found."`` rather than inventing an answer from unrelated
recent records — the confabulation failure this design exists to prevent.

Both tools accept an optional ``tag`` so notes can be grouped (stored under
``metadata.tag``) and recall restricted to a single tag.
"""

from __future__ import annotations

import json
import logging
import uuid
from contextlib import contextmanager
from contextvars import ContextVar

import httpx
from langchain_core.tools import StructuredTool
from pydantic import BaseModel, Field

from config import AgentConfig
import runtime_tracing

logger = logging.getLogger(__name__)

_HTTP_TIMEOUT_SECONDS = 30.0
_NO_MEMORIES = "No relevant memories found."
_DEFAULT_TOP_K = 5
_DEFAULT_MIN_SCORE = 0.5
_memory_capability: ContextVar[str] = ContextVar("memory_capability", default="")


@contextmanager
def memory_runtime_capability(capability: str):
    token = _memory_capability.set(capability)
    try:
        yield
    finally:
        _memory_capability.reset(token)


def _resolve_volume(config: AgentConfig) -> str:
    """Resolve the volume handle: the bound MemoryVolume, else the agent name."""
    return config.memory_volume or config.agent_name


def build_memory_tools(config: AgentConfig) -> list[StructuredTool]:
    """Return the ``memory_recall`` and ``memory_store`` tools for this agent.

    Returns an empty list when memory is disabled, no memory service URL is
    configured, or no volume handle can be resolved.
    """
    if not config.memory_enabled:
        return []

    base_url = (config.memory_service_url or "").rstrip("/")
    if not base_url:
        logger.info("memory tools disabled: no memory service URL configured")
        return []

    volume = _resolve_volume(config)
    if not volume:
        logger.info("memory tools disabled: no volume handle resolved")
        return []

    logger.info("attaching memory tools for volume %s", volume)
    return [
        _build_recall_tool(base_url, volume),
        _build_store_tool(base_url, volume),
    ]


class _RecallArgs(BaseModel):
    query: str = Field(
        description="What to recall — a question or description of the information you need."
    )
    k: int = Field(default=_DEFAULT_TOP_K, description="Maximum number of memories to return.")
    tag: str = Field(
        default="",
        description="Optional tag to restrict recall to memories stored with this tag.",
    )


def _memory_headers() -> dict[str, str]:
    capability = _memory_capability.get()
    headers = {"Authorization": f"Bearer {capability}"} if capability else {}
    return {**headers, **runtime_tracing.inject_trace_headers()}


def _build_recall_tool(base_url: str, volume: str) -> StructuredTool:
    def memory_recall(query: str, k: int = _DEFAULT_TOP_K, tag: str = "") -> str:
        top_k = max(1, int(k) if k else _DEFAULT_TOP_K)
        payload: dict = {
            "volume_handle": volume,
            "query": query,
            "top_k": top_k,
            "min_score": _DEFAULT_MIN_SCORE,
        }
        if tag:
            payload["metadata"] = {"tag": tag}

        try:
            resp = httpx.post(
                f"{base_url}/v1/memory/search", json=payload, timeout=_HTTP_TIMEOUT_SECONDS, headers=_memory_headers()
            )
        except httpx.HTTPError as exc:
            logger.warning("memory recall failed: %s", exc)
            return f"Memory recall failed: {exc}"

        # Preserve compatibility with services that reject text-only search.
        if resp.status_code == 400:
            logger.warning("memory recall unavailable: %s", resp.text)
            return (
                "Memory recall is unavailable: semantic search is not configured "
                "for this memory volume."
            )
        if resp.status_code >= 400:
            return f"Memory recall failed: HTTP {resp.status_code}"

        try:
            results = resp.json()
        except ValueError:
            return _NO_MEMORIES
        if not isinstance(results, list) or not results:
            return _NO_MEMORIES

        memories: list[dict] = []
        for item in results:
            if not isinstance(item, dict):
                continue
            rec = item.get("record")
            if not isinstance(rec, dict):
                continue
            memories.append(
                {
                    "content": rec.get("content", ""),
                    "tag": (rec.get("metadata") or {}).get("tag", ""),
                    "score": item.get("score"),
                }
            )
        if not memories:
            return _NO_MEMORIES
        return json.dumps(memories)

    return StructuredTool.from_function(
        func=memory_recall,
        name="memory_recall",
        description=(
            "Recall relevant notes previously stored in the agent's memory. "
            "Returns a JSON list of the most relevant memories, or the exact "
            "text 'No relevant memories found.' when nothing matches. When it "
            "returns that, do not guess — say you have no memory of it."
        ),
        args_schema=_RecallArgs,
    )


class _StoreArgs(BaseModel):
    content: str = Field(description="The note or fact to remember for later recall.")
    tag: str = Field(
        default="",
        description="Optional tag to categorize this memory for filtered recall.",
    )


def _build_store_tool(base_url: str, volume: str) -> StructuredTool:
    pending_invocations: dict[tuple[str, str], str] = {}

    def memory_store(content: str, tag: str = "") -> str:
        text = (content or "").strip()
        if not text:
            return "Nothing to store: content was empty."

        payload: dict = {"volume_handle": volume, "content": text}
        if tag:
            payload["metadata"] = {"tag": tag}

        pending_key = (_memory_capability.get(), json.dumps(payload, sort_keys=True, separators=(",", ":")))
        invocation_id = pending_invocations.setdefault(pending_key, str(uuid.uuid4()))
        headers = {**_memory_headers(), "X-Kognition-Memory-Invocation-Id": invocation_id}

        try:
            resp = httpx.post(
                f"{base_url}/v1/memory/store", json=payload, timeout=_HTTP_TIMEOUT_SECONDS, headers=headers
            )
        except httpx.HTTPError as exc:
            logger.warning("memory store failed: %s", exc)
            if _memory_capability.get():
                return json.dumps({"runtime_approval_state": "Unavailable", "invocation_id": invocation_id, "error": "Memory write completion is unconfirmed."})
            return f"Memory store failed: {exc}"

        try:
            response = resp.json()
        except ValueError:
            response = None
        if isinstance(response, dict) and "runtime_approval_state" in response:
            if response.get("invocation_id") not in (None, invocation_id):
                return json.dumps({"runtime_approval_state": "Unavailable", "error": "Memory write invocation ID mismatch."})
            if str(response["runtime_approval_state"]).lower() == "terminal":
                pending_invocations.pop(pending_key, None)
            return json.dumps(response)
        if resp.status_code == 202:
            return json.dumps({"runtime_approval_state": "Unavailable", "error": "Memory write completion is unconfirmed."})
        if resp.status_code < 500:
            pending_invocations.pop(pending_key, None)
        if resp.status_code >= 400:
            if resp.status_code >= 500 and _memory_capability.get():
                return json.dumps({"runtime_approval_state": "Unavailable", "invocation_id": invocation_id, "error": "Memory write completion is unconfirmed."})
            return f"Memory store failed: HTTP {resp.status_code}"
        return "Stored."

    return StructuredTool.from_function(
        func=memory_store,
        name="memory_store",
        description=(
            "Persist a note or fact to the agent's memory so it can be recalled "
            "later with memory_recall. Optionally tag it for filtered recall."
        ),
        args_schema=_StoreArgs,
    )
