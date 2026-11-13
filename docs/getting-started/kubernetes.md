---
title: Kubernetes setup
description: Prepare an inactive Kubernetes installation and understand the separate activation requirements.
---

# Kubernetes setup

The committed installer ends at verified inactive admission. It does not activate the controller,
provision providers or install samples. Running workloads requires separately approved activation
with genuine acceptance evidence for the exact target. Production activation remains
blocked by the outstanding gates in the
[activation contract](../features/knowledge-provider-execution/activation-transaction-contract.md).

Use this page to orient installation preparation. The
[installation and recovery contract](../features/knowledge-provider-execution/installation-recovery-contract.md)
defines the complete plan, commands and recovery requirements.

## 1. Prepare an explicitly authorized target

The operator must review and prepare:

- A separate static kubeconfig for the authorized cluster. The installer rejects
  exec/auth-provider credential plugins, insecure TLS and impersonation.
- Installation and dedicated workload namespaces, plus a separate
  administrator-owned authority namespace, with their recorded UIDs.
- The required Kubernetes and Cilium APIs, CRDs, image availability and
  cert-manager infrastructure. These are separate reviewed changes.
- Database migration, backup and credential requirements for the selected profile.
- A writer freeze covering operators, GitOps, autoscalers and workload producers.
  Existing work must drain before admission changes.

Do not enroll namespaces containing unrelated credentials. A controller compromise
can expose credentials throughout its enrolled trust domain.

Use the Go version in [go.mod](../../go.mod), Helm and kubectl. See
[AKS setup](aks.md) for AKS-specific authentication and networking constraints.

## 2. Prepare immutable installation artifacts

Follow the installation contract's
[reviewed plan](../features/knowledge-provider-execution/installation-recovery-contract.md#reviewed-plan)
and the [trusted-render contract](../features/knowledge-provider-execution/bootstrap-verification-contract.md).
Review the chart archive, values, inactive manifest, digest-pinned images,
namespace inventory and retained objects. The controller must render with zero
replicas and without workload permissions.

Protect local plans, values, kubeconfigs and snapshots as credentials. The example
uses an ignored `.local/reviewed/` directory; creating that directory does not
produce a valid plan or approve a target. Preserve exact artifact bytes and hashes.

## 3. Run inactive bootstrap

Only after completing the contract's preparation, drain and plan review:

```powershell
go build -o .\bin\kognition-install.exe .\tools\kognition-install
.\bin\kognition-install.exe bootstrap --plan .\.local\reviewed\install.json --writers-frozen
```

`--writers-frozen` confirms an externally maintained freeze; it does not acquire
a cluster-wide lock. The installer does not create namespaces, install CRDs or
cert-manager, load images or obtain cloud credentials.

A successful result reports **verified inactive bootstrap**. The admission
services are available, but the controller remains stopped and producers must
remain frozen. Helm success and ready gateway Pods do not authorize workloads
or prove database schema readiness.

Automatic `bootstrap.ps1`, `quickstart` and `fresh-kubernetes-demo` paths are
blocked by the current installation workflow. Do not bypass them with direct
Helm activation, replica changes or synthetic acceptance reports.

## 4. Review the separate activation requirements

Retire the completed bootstrap claim only as described in the installation
contract, then follow the
[activation transaction](../features/knowledge-provider-execution/activation-transaction-contract.md).
Activation requires genuine acceptance and administrator approval for the exact
target. Dated local evidence and Deployment readiness do not replace those gates.

This guide does not authorize activation. Continue to
[First agent](first-agent.md) only on an installation whose applicable activation,
provider and access-policy requirements have already been satisfied.

## Failure, upgrades and recovery

Retain the original error, exact executable and reviewed artifacts. Follow the
[explicit recovery procedure](../features/knowledge-provider-execution/installation-recovery-contract.md#explicit-recovery)
with the transaction's exact claim UID and the required settlement confirmations.
Do not reset the installation, strip finalizers or use an automatic rollback to
make a failed operation appear successful.

Existing releases and namespace-enrollment changes use the same authoritative
installation, drain and activation contracts. See
[Operations](../operations/README.md) for the corresponding references.
