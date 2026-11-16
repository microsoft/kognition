# Kognition

<p align="center">
    <picture>
        <source media="(prefers-color-scheme: dark)" srcset="docs/assets/kognition-logo-dark.svg">
        <img src="docs/assets/kognition-logo.svg" alt="Kognition: Kubernetes for cognition" width="760">
    </picture>
</p>

Kognition is a declarative orchestration platform for AI agents. Define agents,
workflows, models, tools, memory, knowledge and governance policies as resources;
the control plane reconciles their desired state and records execution status.

> [!WARNING]
> Kognition is under active development. APIs and deployment contracts can change
> before a stable release. Tests and scoped acceptance do not establish
> production readiness.

## Who it is for

Kognition targets teams that need declarative agent configuration, observable
execution and explicit access boundaries around AI capabilities.

- **Agent authors:** define workloads and compose models, tools, memory and
  knowledge through a shared resource model.
- **Platform operators:** manage admission, identity, namespaces, quotas and
  approval policies around execution.
- **Integrators:** build against the standalone API or Kubernetes resources and
  inspect state through the CLI and Studio.

Kognition does not supply a model or remove the need to operate providers,
credentials, storage and networking. Read the [overview](docs/overview.md) for
deployment boundaries and current limitations.

## Get started

| Your goal | Start here |
| --- | --- |
| Browse everything online | [Documentation site](https://microsoft.github.io/kognition). |
| Explore a local control plane | [Local development](docs/getting-started/local.md). |
| Prepare a Kubernetes installation | [Kubernetes setup](docs/getting-started/kubernetes.md). |
| Run an agent on a prepared installation | [First agent](docs/getting-started/first-agent.md). |
| Watch a complete Studio walkthrough | [Video tutorial](docs/guides/video-tutorial.md). |
| Find a capability walkthrough | [Guides and samples](docs/guides/README.md). |
| Operate or troubleshoot a deployment | [Operations](docs/operations/README.md). |

The Kubernetes installer checks the target, installs admission and database
components, verifies their readiness, then starts the controller. It refuses to
overwrite an existing installation. Follow [Kubernetes setup](docs/getting-started/kubernetes.md)
or [AKS setup](docs/getting-started/aks.md); configure providers and samples afterward.

## Documentation

Read the published site at **<https://microsoft.github.io/kognition>**, or start
at the **[documentation home](docs/README.md)** in this repository.

- [Resource model](docs/concepts/resource-model.md): configuration, execution,
  authorization and observed state.
- [CLI reference](docs/reference/cli.md): explicit backend selection,
  authentication and common commands.
- [Reference index](docs/reference/README.md): API definitions, configuration,
  architecture references and ADRs.
- [Kognition Studio](docs/studio.md): visual authoring and safe operational views.
- [Documentation map](docs/content-map.md): reader paths and document authority.

The [existing tutorial](TUTORIAL.md) and [quickstart](QUICKSTART.md) remain
available. Use the getting-started guides for installation and
[Operations](docs/operations/README.md) for upgrades or recovery.

## Architecture

- **Control plane:** the standalone API server and controllers, or Kubernetes
  custom resources, admission webhooks and controllers.
- **Execution:** Agent and Workflow resources coordinate runtime instances and
  record execution state.
- **Capability boundaries:** model, tool, memory, knowledge and communication
  components enforce the relevant authorization contracts.
- **State and visibility:** persistent resources, events, metrics and opt-in
  traces expose observed behavior.

Read the [architectural motivation](docs/kognition-manifest.md) and
[reference architecture](docs/architecture/reference/README.md) for detail.
Shared resource concepts do not establish complete parity between deployment modes.

## Build and contribute

Use the Go version declared in [go.mod](go.mod). Building the CLI does not start
the platform or invoke a provider:

**PowerShell (Windows):**

```powershell
go build -o .\bin\kognitionctl.exe .\cmd\kognitionctl
.\bin\kognitionctl.exe --help
```

**Bash (Linux/macOS):**

```bash
go build -o ./bin/kognitionctl ./cmd/kognitionctl
./bin/kognitionctl --help
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the CLA, build/test workflow and commit
policy, [command shells](docs/README.md#command-shells) for shared helper
prerequisites, and [testing](docs/testing.md) for required validation.

The official repository is [microsoft/kognition](https://github.com/microsoft/kognition).
Use [SUPPORT.md](SUPPORT.md) for support boundaries and [SECURITY.md](SECURITY.md)
for private vulnerability reporting. This project follows the
[Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/);
see [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Project sources

| Area | Source |
| --- | --- |
| Resource definitions | [Standalone API](api/v1alpha1/) and [Kubernetes API](api/k8s/v1alpha1/). |
| Binaries and integrations | [Commands](cmd/), [internal implementation](internal/) and [plugin SDK](pkg/pluginsdk/). |
| Runtime components | [Runtime sources](runtime/). |
| Deployment and examples | [Deployment assets](deploy/) and [samples](samples/). |
| Design and work tracking | [Project specification](PROJECT.md), [backlog](BACKLOG.md) and [ADRs](docs/adr/README.md). |

Specifications and plans describe their recorded scope; they are not a checklist
of shipped or production-qualified features.

## Trademarks

This project may contain trademarks or logos for projects, products, or services.
Authorized use of Microsoft trademarks or logos is subject to and must follow
[Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general).
Use of Microsoft trademarks or logos in modified versions of this project must
not cause confusion or imply Microsoft sponsorship. Any use of third-party
trademarks or logos are subject to those third-party's policies.

## Telemetry

OpenTelemetry trace export is disabled by default and has no default Microsoft
destination. Operators can enable export to their own local collector or chosen
backend. Disable it with `KOGNITION_OTEL_TRACES_ENABLED=false` and, for Helm,
`observability.openTelemetry.enabled=false`.

Configured AI and search provider requests and application logs are separate
from trace export. See [observability](docs/observability.md) and
[PRIVACY](PRIVACY) for controls and data-handling boundaries.

## License

Microsoft-owned first-party code is licensed under the [MIT license](LICENSE.TXT).
Dependencies and redistributed third-party components retain their own licenses;
MIT licensing of this repository does not relicense those components.
See [NOTICE](NOTICE) for attribution scope and distribution review boundaries.
