# tools-in-action

End-to-end sample that exercises **governed HTTP tool execution** in the
Kognition runtime. The agent is given two tools (`sandbox-calculator` and
`sandbox-datetime`) served by the shared `tool-sandbox` HTTP service. The Tool
Gateway owns target endpoint access. This proves the full pipeline:

1. `Tool` CRs referencing an OpenAPI endpoint are accepted by the apiserver.
2. The controller injects `KOGNITION_TOOLS_RESOLVED` with Tool schemas and
  immutable resource versions, plus the Tool Gateway URL. Target endpoints are
  omitted from the runtime Pod.
3. The runtime exchanges its session capability for one exact Tool capability.
4. The Tool Gateway re-evaluates ToolGrant, AgentRole, policy, session, and
  Tool revision, invokes the endpoint, records durable events, and returns the
  result to the model.

## What's in here

| Resource | Kind | Purpose |
|---|---|---|
| `sandbox-calculator` | `Tool` | Safe arithmetic evaluator, served at `POST /calculator` on the tool-sandbox service. |
| `sandbox-datetime` | `Tool` | Returns the current UTC time as ISO-8601, served at `POST /datetime`. |
| `tool-user` | `Agent` | Uses `gpt-6-luna` and is forced (by its instructions) to call a tool for math and time questions. |
| `tool-user-tool-grants` | `AgentPolicy` | Synthesizes the ToolGrants required by the Tool Gateway. |
| `tool-user-tool-invoker` | `AgentRole` and `AgentRoleBinding` | Allows the Agent to invoke `Tool` resources. |

The `sandbox-openapi` `ToolClass` and the underlying `tool-sandbox`
Deployment/Service are declared in [`samples/workflows/tool-sandbox.yaml`](../workflows/tool-sandbox.yaml).

## Prerequisites

- Kognition installed through the [guarded installer](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md)
  and separately approved for activation. Use an explicit reviewed kubeconfig and
  dedicated enrolled workload namespace; adapt the example's `default` references
  consistently. Do not run samples during a bootstrap/recovery writer freeze.
- The tool sandbox applied: `kubectl apply -f samples/workflows/tool-sandbox.yaml`.
- A working `Model` named `gpt-6-luna` in the `default` namespace
  (`samples/foundry-agent/` shows how to set one up against Azure AI Foundry).
- `kognitionctl` built from this repo.

## Apply

These commands work in Bash and PowerShell:

```sh
kubectl apply -f samples/workflows/tool-sandbox.yaml
kubectl apply -f samples/tools-in-action/manifests.yaml
kubectl -n default wait --for=jsonpath='{.status.phase}'=Ready agent/tool-user --timeout=120s
```

When `networkPolicy.enabled=true`, permit the Gateway to reach the sandbox
before running the sample. The runtime Pod remains unable to reach this Service
directly.

Review `toolGateway.privateEgress` with a namespace selector for the enrolled
workload namespace, Pod selector `app: tool-sandbox` and port `8080` in the complete
platform values. Apply platform changes through the guarded installer before
activation; direct Helm upgrade and `--reuse-values` are unsupported.

## Run

```powershell
.\bin\kognitionctl.exe chat tool-user "what is 17 * 23?" --mode kubernetes
# => 17 multiplied by 23 is 391.

.\bin\kognitionctl.exe chat tool-user "what is the current UTC time?" --mode kubernetes
# => The current UTC time is "2026-06-26T21:28:24Z".

.\bin\kognitionctl.exe chat tool-user "Compute (123 + 456) * 2, then get the current UTC time." --mode kubernetes
# => The result of (123 + 456) * 2 is 1158. The current UTC time is "...".
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl chat tool-user "what is 17 * 23?" --mode kubernetes
./bin/kognitionctl chat tool-user "what is the current UTC time?" --mode kubernetes
./bin/kognitionctl chat tool-user "Compute (123 + 456) * 2, then get the current UTC time." --mode kubernetes
```

## Verify tool execution

The runtime logs every tool call. Tail the agent pod:

```powershell
kubectl -n default logs deployment/agent-tool-user --tail=20 | Select-String "tool call"
# tool call: sandbox_calculator({'expression': '17 * 23'}) -> 45 chars
# tool call: sandbox_datetime({}) -> 62 chars
```

**Bash (Linux/macOS):**

```bash
kubectl -n default logs deployment/agent-tool-user --tail=20 | grep -F 'tool call'
```

Each invocation also emits a single `Invocation completed: tokens=... tools=N`
line with the count of tool calls in that turn.

The completed `AgentInstance` retains bounded details for each Tool call. Use
the instance name printed by `kognitionctl chat` to inspect the exact arguments,
Tool result, and duration without retrieving the runtime's target endpoint:

Use the following command in either shell:

```sh
kubectl -n default get agentinstance '<instance-name>' -o jsonpath='{.status.toolCallsDetail}'
# [{"args":"{\"expression\":\"17 * 23\"}","durationMs":243,"name":"sandbox_calculator","result":"{\"expression\": \"17 * 23\", \"result\": 391}"}]
```

The Gateway also persists `ToolInvocationAuthorized` and
`ToolInvocationCompleted` Kubernetes Events for the same instance:

```sh
kubectl -n default get events --field-selector 'involvedObject.kind=AgentInstance,involvedObject.name=<instance-name>'
```

## How the wiring works

- `ToolClass.spec.provisioner: openapi` (declared in `tool-sandbox.yaml` as
  `sandbox-openapi`) tells the platform that Tools in this class are HTTP
  services described by OpenAPI. Tool CRs reference this class via `className`.
- Each `Tool` CR carries an `endpoint` (the sandbox service URL) and an
  `inputSchema` (the JSON Schema the LLM binds against). The dispatcher exposes
  only the schema and Tool resource version to Gateway-enabled runtimes. The
  Tool Gateway resolves and invokes `endpoint` after authorization.
- `spec.metadata.allowPrivateNetwork: "true"` permits a reviewed private
  endpoint at the application layer. In a NetworkPolicy-enforcing cluster, the
  endpoint's namespace labels, Pod labels, and port must also appear in
  `toolGateway.privateEgress`; this is the network-layer approval.
- Tool CR names are kebab-case (Kubernetes rules); the runtime canonicalizes
  `sandbox-calculator` -> `sandbox_calculator` before exposing it to the LLM.
- `KOGNITION_TOOLS_RESOLVED` is the dispatcher-injected env var containing
  schemas and immutable Tool versions. Gateway mode intentionally omits target
  endpoints. Inspect it on the pod with:

  ```powershell
  kubectl -n default exec deployment/agent-tool-user -- sh -c 'echo $KOGNITION_TOOLS_RESOLVED' | ConvertFrom-Json
  ```

  **Bash (Linux/macOS):**

  ```bash
  kubectl -n default exec deployment/agent-tool-user -- sh -c 'echo "$KOGNITION_TOOLS_RESOLVED"' |
    python3 -m json.tool
  ```

## Adding your own tool

1. Serve a POST endpoint that accepts JSON arguments and returns a JSON body.
   For quick demos, extend the `tool-sandbox` ConfigMap in
   [`samples/workflows/tool-sandbox.yaml`](../workflows/tool-sandbox.yaml) with a
   new route (see how `/calculator` and `/datetime` are wired).
2. Declare a `Tool` CR with `protocol: OpenAPI`, `className: sandbox-openapi`
   (or your own OpenAPI ToolClass), the `endpoint` URL, and an `inputSchema`.
3. Reference the Tool from your Agent's `spec.tools[].ref.name`.

For MCP-backed tools set `protocol: MCP` and `className: sandbox-mcp` (or
another MCP ToolClass). See `sandbox-greet` in
[`samples/workflows/tool-steps-http.yaml`](../workflows/tool-steps-http.yaml)
for a JSON-RPC example.
