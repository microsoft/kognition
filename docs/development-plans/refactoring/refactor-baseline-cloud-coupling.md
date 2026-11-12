# Refactor Baseline: Cloud SDK Coupling

- Plan task: RP01 (baseline inventory), first artifact. The test, skip and operation inventories are still pending.
- Date: 2026-11-10.
- Source: `fix/refactor-baseline` at `8fd235c` plus uncommitted work unrelated to this report.
- Scope: Go dependency coupling only. Python, images, charts and tests are not covered here.

## Method

```powershell
# Go files that import a cloud SDK directly
Get-ChildItem -Recurse -Include *.go -File | Select-String -Pattern '"(github.com/Azure/|github.com/AzureAD/|github.com/aws/|aws-sdk-go|[^"]*anthropic)[^"]*"' -List

# Azure SDK packages linked into each binary
go list -deps ./cmd/<binary> | Select-String 'github.com/Azure|AzureAD'
```

## Direct SDK imports

Seven non-test files import an Azure SDK. No Go file imports an AWS or Anthropic SDK; those drivers do not add a module dependency.

| File | Area |
| --- | --- |
| `internal/cpi/drivers/foundry/token_provider.go` | Foundry model driver credentials |
| `providers/azuresearch/backend_operation.go`, `client.go` | Azure AI Search knowledge provider |
| `cmd/kognition-knowledge-azuresearch/{cleaner,indexer,main,querier}.go` | Azure AI Search provider binary |

## Core binaries that link Azure SDK packages

Twelve core binaries link 50 Azure SDK packages each. Eight binaries link none: `kognitionctl`, `kubectl-kognition`, `kognition-migrate`, `kognition-knowledge-pgvector`, `kognition-workload-identity-proxy`, `kognition-intent-gateway-client`, `kognition-e2e-mint-agent-delegation` and `kognition-trustworthiness-fixture`.

Two routes carry the SDK into core, so removing the dependency needs two separate seams:

| Route | Import chain | Binaries affected |
| --- | --- | --- |
| Foundry driver | `cmd/kognition-apiserver` imports the driver directly. `internal/modelgateway` and `internal/memory/embed` (through `internal/memory/serviceapp`) import it. | apiserver, controller-manager, k8s-controller, model-gateway, memory-service, knowledge-indexer and the gateways that reach them |
| Azure Search provider | `internal/gatewaybackend` imports `internal/k8s/controller`, which imports `providers/azuresearch`. `cmd/kognition-knowledge-gateway` imports it directly. | k8s-controller, model-gateway, tool-gateway, memory-policy-gateway, intent-gateway, agent-communication-gateway, knowledge-gateway |

The Azure Search route is the more disruptive one: gateways that have nothing to do with knowledge reach the provider through the shared Kubernetes controller package.

## Consequences for the plan

- RP04 must give the Foundry driver and the Azure Search provider a registration seam in core, so that `internal/modelgateway`, `internal/memory/embed`, `cmd/kognition-apiserver` and `internal/k8s/controller` stop importing them.
- RP05 then composes each binary from registered providers. A binary that needs Azure gets it from the `integrations/azure` module at build time, not from core.
- The completion check is `go list -deps` on every core binary returning zero `github.com/Azure` packages, plus `go.mod` without any Azure `require`.
- Foundry token handling is the only direct credential-library use. A core replacement for token acquisition must not weaken credential isolation.
