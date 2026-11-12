# Orbital Mark 7 — Overview

**Orbital Mark 7** is a hosted large-reasoning-model service for building
autonomous agents. It exposes a chat-completions API, a function-calling
interface, a vision endpoint, and an embeddings endpoint behind a single
regional deployment resource.

## Who it's for

- **Application developers** who want a low-latency reasoning model without
  managing GPUs.
- **Platform teams** who need per-tenant isolation, regional data residency,
  and predictable token pricing.
- **Agent builders** who rely on structured function calling and long-context
  retrieval.

## Key capabilities

- **Reasoning-optimized model** tuned for multi-step tool use and planning.
- **32,000-token context window** at launch.
- **Function calling** with parallel tool invocation.
- **Vision** input for images and rendered documents.
- **Embeddings** for retrieval-augmented generation.

## Positioning versus Mark 6

Mark 7 roughly doubles the effective reasoning depth of Mark 6 while holding
per-token latency flat. The biggest operational change is that deployments are
now **regional by default** — a Mark 6 "global" deployment does not exist in
Mark 7, so every deployment is pinned to a specific region for data residency.

## Support lifecycle

Mark 7 is supported for **24 months** from general availability. Security fixes
are backported to the two most recent dot-releases only. Customers are expected
to be on Mark 7.1 or newer within six months of that release shipping.
