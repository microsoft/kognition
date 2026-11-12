# Workflow Feature Demos

Six runnable demos, one per major workflow feature documented in
`TUTORIAL.md`. Each manifest is self-contained except for two shared
agents (`wfdemo-llm`, `wfdemo-classifier`) defined in `_agents.yaml`.

## Optional Sample Server

[tool-sandbox.yaml](tool-sandbox.yaml) contains an OpenAPI/MCP demonstration
server. It belongs to the samples, not the Kognition platform. The base Helm
chart and platform installer do not install it. Platform MCP clients remain
available to connect to an operator-selected MCP server.

Install the sample server only on a test cluster with Kognition's ToolClass CRD
already installed. Installation needs permission to create the listed resources,
including the namespace and cluster-scoped ToolClasses. Cleanup requires listing
Tools across namespaces to check references. Use an explicit context:

```powershell
.\samples\workflows\tool-sandbox.ps1 -Action Install -Context <test-context> -WhatIf
.\samples\workflows\tool-sandbox.ps1 -Action Install -Context <test-context>
```

Add `-Kubeconfig <path>` when needed. The script applies only the sample server
manifest and waits for its Deployment. It creates a namespace, ConfigMap,
Deployment, Service and two shared sample ToolClasses; it does not install
agents, models, credentials or other workflow samples. The endpoint is
`http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/mcp`.
The server has no application authentication and is intended for synthetic
demonstrations on a trusted isolated test network, not production or public
exposure. Do not send sensitive inputs to it.

### Test MCP Without an Agent or Model

Forward the sample Service to loopback in a separate terminal:

```powershell
kubectl --context <test-context> -n samples-tool-sandbox port-forward --address 127.0.0.1 service/tool-sandbox 18080:8080
```

In another terminal, initialize the sample session, list tools and invoke the
synthetic greeting:

```powershell
$uri = 'http://127.0.0.1:18080/mcp'
Invoke-RestMethod -Uri $uri -Method Post -ContentType application/json -Body '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"sample-client","version":"1"}}}'
Invoke-RestMethod -Uri $uri -Method Post -ContentType application/json -Body '{"jsonrpc":"2.0","method":"notifications/initialized"}'
Invoke-RestMethod -Uri $uri -Method Post -ContentType application/json -Body '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
Invoke-RestMethod -Uri $uri -Method Post -ContentType application/json -Body '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sandbox-greet","arguments":{"who":"sample-user"}}}'
```

The list contains `sandbox-greet`; the invocation returns a greeting and synthetic
demo timestamp. No external model or provider credential is needed for this
protocol test. Stop the port-forward with Ctrl+C afterward.

## Layout

```
samples/workflows/
├── _agents.yaml                # shared agents (apply first)
├── conditional-branching.yaml  # CEL edge conditions
├── parallel-dispatch.yaml      # fan-out / fan-in + maxParallelSteps
├── tool-sandbox.yaml           # in-cluster OpenAPI+MCP tool server (apply before tool-steps-http)
├── tool-sandbox.ps1            # explicit-context sample server install and cleanup
├── tool-steps-http.yaml        # type=tool steps (OpenAPI + MCP) + AgentPolicy grants
├── quorum-approval.yaml        # multi-approver gate
├── retries-and-errors.yaml     # step retry + continueOnError
└── shared-memory.yaml          # per-run shared MemoryClaim
```

## Prerequisites in `default` namespace

- `SecretRef/foundry-creds`
- `Model/gpt-6-luna`
- `MemoryClass/redis-fast` (for `shared-memory.yaml`)

Prepare these prerequisites explicitly; installing the platform does not create
them or authorize sample execution.

## Apply everything

```powershell
kubectl apply -f samples/workflows/_agents.yaml
Get-ChildItem samples/workflows/*.yaml -Exclude _agents.yaml |
  ForEach-Object { kubectl apply -f $_.FullName }
```

## Inspect

```powershell
kubectl -n default get workflows.kognition.dev -l app.kubernetes.io/part-of=workflow-demos
kubectl -n default get agents.kognition.dev   -l app.kubernetes.io/part-of=workflow-demos
```

## Trigger any demo

Replace `<wf>` with the workflow name and `<input>` with any string:

```yaml
apiVersion: kognition.dev/v1alpha1
kind: WorkflowRun
metadata:
  name: <wf>-test-1
  namespace: default
spec:
  workflowRef:
    name: <wf>
  input: "<your input>"
```

See the comment header of each demo file for a ready-to-paste `WorkflowRun`
and any post-trigger commands (approvals, memory verification, etc.).

## Tear down

```powershell
kubectl --context <test-context> -n default delete workflows,workflowruns,agents,tools,toolgrants,agentpolicies,agentroles,agentrolebindings -l app.kubernetes.io/part-of=workflow-demos
```

After deleting demo Tools that reference `sandbox-openapi` or `sandbox-mcp`,
remove the optional server:

```powershell
.\samples\workflows\tool-sandbox.ps1 -Action Uninstall -Context <test-context>
```

The script refuses cleanup while Tools still reference either shared ToolClass.
It deletes only the named server resources and ToolClasses, retaining the
namespace and any unrelated resources. This check is a cleanup guard, not a
transactional lock; do not concurrently create new references during cleanup.
