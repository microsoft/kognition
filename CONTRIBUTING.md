# Contributing to Kognition

This repository is in development. See [BACKLOG.md](BACKLOG.md) for current scope.

[Thiago Salles](mailto:thiagosalles@microsoft.com) is the project maintainer,
responsible for community feedback, pull request review and merging, and release
coordination. See [SUPPORT.md](SUPPORT.md) for support channels and security
reporting boundaries.

## Contributor License Agreement

This project welcomes contributions and suggestions. Most contributions require
you to agree to a Contributor License Agreement (CLA) declaring that you have
the right to, and actually do, grant us the rights to use your contribution.
For details, visit <https://cla.opensource.microsoft.com>.

When you submit a pull request, a CLA bot will automatically determine whether
you need to provide a CLA and decorate the pull request appropriately. Simply
follow the instructions provided by the bot. You will only need to do this once
across all repositories using our CLA.

The repository owner must enable the Microsoft CLA integration before accepting
public contributions. This document does not attest that it is already enabled.

## Development

- Read [README.md](README.md), [docs/testing.md](docs/testing.md) and
  [docs/security.md](docs/security.md) before changing runtime behavior.
- Use Go 1.26 or newer, Docker and Buf as described in the README. Build with
  `make build`; run focused Go tests for the packages you change.
- Keep changes scoped, preserve native runtime compatibility, and add regression
  coverage for changed behavior through public interfaces.
- Run `go test` and the repository-configured Go linters for affected packages.
  Follow the existing formatting and error-handling conventions.
- Describe the behavior, tests and remaining limitations in your pull request.
  Use a task prefix when a backlog task exists. Do not include credentials,
  personal data, internal-only documents or deployment-specific access details.
- Changes to shared contracts, security controls or deployments require explicit
  review. Do not activate clusters, incur provider charges or modify cloud
  resources as part of an ordinary local test run.
- Retain third-party copyright and license notices. Describe any new dependency
  and its license in the pull request and update distribution attribution.

## Code of Conduct

This project has adopted the [Microsoft Open Source Code of
Conduct](CODE_OF_CONDUCT.md). For more information see the
[Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or
contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with questions
or comments.
