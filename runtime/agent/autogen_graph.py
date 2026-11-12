# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import asyncio
import json
import logging

from autogen_agentchat.agents import AssistantAgent
from autogen_agentchat.messages import TextMessage, ToolCallExecutionEvent, ToolCallRequestEvent
from autogen_core import CancellationToken, FunctionCall
from autogen_core.model_context import BufferedChatCompletionContext
from autogen_core.models import AssistantMessage, FunctionExecutionResult, FunctionExecutionResultMessage, UserMessage
from autogen_core.tools import BaseTool
from autogen_ext.models.openai import OpenAIChatCompletionClient
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage

from config import AgentConfig
from memory import build_memory_tools, memory_runtime_capability
from providers import _model_gateway_base_url, _openai_generation_parameters, model_runtime_session_capability
import runtime_tracing
from tools import (
    _build_kb_search_tool,
    agent_delegation_runtime_session_capability,
    build_agent_delegation_tool,
    build_tools,
    knowledge_runtime_session_capability,
    tool_runtime_session_capability,
)

logging.getLogger("autogen_agentchat.events").disabled = True
logging.getLogger("autogen_core.events").disabled = True


class GovernedTool(BaseTool):
    def __init__(self, tool):
        super().__init__(tool.args_schema, str, tool.name, tool.description)
        self.tool = tool
        self.active_tasks = set()

    async def run(self, args, cancellation_token):
        task = asyncio.create_task(asyncio.to_thread(self.tool.invoke, args.model_dump()))
        self.active_tasks.add(task)
        task.add_done_callback(self.active_tasks.discard)
        try:
            return str(await asyncio.shield(task))
        except asyncio.CancelledError:
            await asyncio.shield(task)
            raise


def gateway_model(config, capability):
    if config.model_provider.lower() not in ("openai", "openai-compatible", "foundry"):
        raise ValueError("AutoGen requires a supported Model Gateway provider")
    tool_parameters = {} if config.model_invocation_profile else {"parallel_tool_calls": False}
    return OpenAIChatCompletionClient(
        model="kognition-model" if config.model_invocation_profile else config.model_id,
        base_url=_model_gateway_base_url(config.model_gateway_url),
        api_key=capability,
        default_headers=runtime_tracing.inject_trace_headers(),
        **_openai_generation_parameters(config),
        **tool_parameters,
        timeout=config.timeout_seconds,
        max_retries=0,
        model_info={"vision": False, "function_calling": True, "json_output": False, "family": "unknown", "structured_output": False},
    )


def sdk_history(messages):
    result = []
    for message in messages:
        if isinstance(message, HumanMessage):
            result.append(UserMessage(content=str(message.content), source="user"))
        elif isinstance(message, AIMessage):
            content = str(message.content)
            if message.tool_calls:
                content = [FunctionCall(id=call["id"], name=call["name"], arguments=json.dumps(call["args"])) for call in message.tool_calls]
            result.append(AssistantMessage(content=content, source="kognition_autogen"))
        elif isinstance(message, ToolMessage):
            execution = FunctionExecutionResult(content=str(message.content), call_id=message.tool_call_id, name=message.name or "tool", is_error=False)
            if result and isinstance(result[-1], FunctionExecutionResultMessage):
                result[-1].content.append(execution)
            else:
                result.append(FunctionExecutionResultMessage(content=[execution]))
        else:
            raise ValueError("unsupported AutoGen conversation message")
    return result


class AutoGenGraph:
    def __init__(self, config, model_factory):
        self.config = config
        self.model_factory = model_factory
        tools = build_tools(resolved_json=config.tools_resolved_json, fallback_json=config.tools_json, tool_gateway_url=config.tool_gateway_url)
        tools.extend(build_memory_tools(config))
        delegation = build_agent_delegation_tool(config.agent_communication_gateway_url) if config.agent_delegation_enabled else None
        if delegation is not None:
            tools.append(delegation)
        knowledge = json.loads(config.kb_resolved_json)
        if not isinstance(knowledge, list):
            raise ValueError("AutoGen knowledge configuration must be a list")
        search = _build_kb_search_tool(knowledge, config.knowledge_gateway_url, config.knowledge_gateway_ca_file)
        if search is not None:
            tools.append(search)
        if len(tools) > 50 or len({tool.name for tool in tools}) != len(tools):
            raise ValueError("AutoGen tools must be unique and bounded")
        self.tools = [GovernedTool(tool) for tool in tools]

    async def astream(self, state, stream_mode="values"):
        if stream_mode != "values":
            raise ValueError("AutoGen supports values state streaming")
        messages = list(state.get("messages", []))
        capability = state.get("model_session_capability", "")
        if not capability or not messages or not isinstance(messages[-1], HumanMessage):
            raise ValueError("AutoGen requires an authenticated model session and a user turn")
        if len(messages) > 80 or sum(len(str(message.content).encode("utf-8")) for message in messages) > 4 << 20:
            raise ValueError("AutoGen conversation exceeds bounds")
        saved = state.get("autogen_state")
        if saved is not None and (not isinstance(saved, dict) or len(json.dumps(saved).encode("utf-8")) > 4 << 20):
            raise ValueError("AutoGen saved state exceeds bounds")
        client = self.model_factory(self.config, capability)
        cancellation = CancellationToken()
        result = {**state, "messages": messages, "tool_calls_count": 0, "tool_calls_detail": [], "token_usage": {}}
        pending = {}
        events = None
        try:
            assistant = AssistantAgent(
                "kognition_autogen",
                model_client=client,
                tools=self.tools or None,
                model_context=BufferedChatCompletionContext(buffer_size=80, initial_messages=[] if saved is not None else sdk_history(messages[:-1])),
                system_message=self.config.system_prompt,
                reflect_on_tool_use=True,
                max_tool_iterations=20,
            )
            if saved is not None:
                await assistant.load_state(saved)
            with (
                memory_runtime_capability(state.get("memory_capability", "")),
                tool_runtime_session_capability(state.get("tool_session_capability", "")),
                model_runtime_session_capability(capability),
                agent_delegation_runtime_session_capability(state.get("agent_delegation_session_capability", "")),
                knowledge_runtime_session_capability(state.get("knowledge_session_capability", "")),
            ):
                events = assistant.run_stream(task=str(messages[-1].content), cancellation_token=cancellation, output_task_messages=False)
                async for event in events:
                    if isinstance(event, ToolCallRequestEvent):
                        if len(event.content) != 1:
                            raise ValueError("AutoGen requires sequential governed tool calls")
                        calls = []
                        for call in event.content:
                            arguments = json.loads(call.arguments)
                            if not isinstance(arguments, dict):
                                raise ValueError("AutoGen tool input must be an object")
                            calls.append({"id": call.id, "name": call.name, "args": arguments, "type": "tool_call"})
                            pending[call.id] = (call.name, arguments)
                        result["messages"].append(AIMessage(content="", tool_calls=calls))
                    elif isinstance(event, ToolCallExecutionEvent):
                        for execution in event.content:
                            name, arguments = pending.pop(execution.call_id, (execution.name, {}))
                            result["messages"].append(ToolMessage(content=execution.content, tool_call_id=execution.call_id, name=name))
                            result["tool_calls_count"] += 1
                            result["tool_calls_detail"].append({"name": name, "call_id": execution.call_id, "args": arguments, "result": execution.content, "duration_ms": 0, "error": "tool execution failed" if execution.is_error else None})
                            try:
                                response = json.loads(execution.content)
                            except (ValueError, TypeError):
                                response = None
                            if isinstance(response, dict) and "runtime_approval_state" in response:
                                approval = response["runtime_approval_state"]
                                if not isinstance(approval, str) or approval.lower() not in ("pending", "terminal", "unavailable"):
                                    raise ValueError("invalid AutoGen approval response")
                                result["runtime_approval_state"] = approval
                    elif isinstance(event, TextMessage) and event.source == "kognition_autogen":
                        if len(event.content.encode("utf-8")) > 65536:
                            raise ValueError("AutoGen output exceeds bounds")
                        result["messages"].append(AIMessage(content=event.content))
                    usage = client.total_usage()
                    result["token_usage"] = {"prompt_tokens": usage.prompt_tokens, "completion_tokens": usage.completion_tokens, "total_tokens": usage.prompt_tokens + usage.completion_tokens}
                    if isinstance(event, (TextMessage, ToolCallExecutionEvent)) and "runtime_approval_state" not in result:
                        exported = await assistant.save_state()
                        if len(json.dumps(exported).encode("utf-8")) > 4 << 20:
                            raise ValueError("AutoGen saved state exceeds bounds")
                        result["autogen_state"] = exported
                    yield {**result, "messages": list(result["messages"]), "tool_calls_detail": list(result["tool_calls_detail"])}
                    if "runtime_approval_state" in result:
                        return
        except asyncio.CancelledError:
            cancellation.cancel()
            raise
        except Exception:
            raise RuntimeError("AutoGen execution failed") from None
        finally:
            cancellation.cancel()
            active = [task for tool in self.tools for task in tuple(tool.active_tasks)]
            if active:
                await asyncio.shield(asyncio.gather(*active, return_exceptions=True))
            if events is not None:
                await events.aclose()
            await client.close()

    async def ainvoke(self, state):
        final = None
        async for value in self.astream(state):
            final = value
        if final is None:
            raise RuntimeError("AutoGen returned no execution state")
        return final


def build_autogen_graph(config: AgentConfig, model_factory=None):
    if not all((config.runtime_auth_token, config.model_gateway_url, config.tool_gateway_url, config.memory_service_url, config.agent_communication_gateway_url)):
        raise ValueError("AutoGen requires authenticated runtime and four protected gateways")
    return AutoGenGraph(config, model_factory or gateway_model)