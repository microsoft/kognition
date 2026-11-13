---
title: AKS setup
description: Review AKS prerequisites and authentication constraints before guarded Kubernetes installation.
---

# AKS setup

AKS uses the same [inactive Kubernetes bootstrap](kubernetes.md) and separate activation
requirements as other Kubernetes targets. This guide does not provision cloud
resources, publish images or authorize activation.

## Authentication compatibility

The current installer requires a separately reviewed **static kubeconfig**.
Its exec/auth-provider credential plugins are unsupported, including the usual
kubelogin exec configuration produced for Microsoft Entra authentication.

Do not pass an ordinary `az aks get-credentials` result directly to the installer
or disable TLS verification to work around an authentication failure. Work with
the cluster administrator to obtain an explicitly authorized target and supported
credentials. Protect that kubeconfig as a secret and preserve the reviewed
cluster identity throughout installation and recovery.

Operator inspection through Azure CLI or kubelogin does not establish that the
same authentication configuration is supported by the installer.

## Cluster and registry prerequisites

- An approved AKS cluster, namespaces and administrator-owned authority namespace.
- The required Kubernetes admission APIs and an approved Cilium policy profile.
- cert-manager, CRDs and reachable application images for the reviewed chart.
- Registry connectivity and node image-pull permissions configured through the
  organization's normal platform process.
- The Go version in [go.mod](../../go.mod), kubectl and Helm for the documented
  installation tooling.

Review the [AKS deployment reference](../../deploy/aks/README.md) and its selected
profile. The repository's [Bicep infrastructure](../../infra/main.bicep) and
[parameter tooling](../../deploy/aks/New-FoundationParameters.ps1) are separate
preparation paths, not prerequisites to run against an already prepared cluster.

Do not start or stop a shared cluster, loosen registry network rules or change
the global Kubernetes context as a documentation-validation step.

## Installation and activation

Prepare the exact chart, values, inactive manifest, namespace inventory and
reviewed plan described in [Kubernetes setup](kubernetes.md). The installer does
not obtain Azure credentials, create namespaces or publish images.

The [installation contract](../features/knowledge-provider-execution/installation-recovery-contract.md)
ends at verified inactive admission. Follow the
[activation contract](../features/knowledge-provider-execution/activation-transaction-contract.md)
only after genuine acceptance and administrator approval for the exact target.
Production activation remains blocked by its outstanding gates.

After an installation has satisfied its applicable activation and access-policy
requirements, prepare an approved model and continue to
[First agent](first-agent.md). A healthy control plane does not prove provider
connectivity or authorize a paid model invocation.
