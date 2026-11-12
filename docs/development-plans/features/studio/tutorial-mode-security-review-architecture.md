# Tutorial Mode Architectural Security Review

- Mode: Architectural.
- Date: 2026-10-29.
- Status: Design accepted for isolated implementation; local automated control evidence recorded below.
- Scope: [tutorial plan](tutorial-mode-plan.md) and
  [ADR-0026](../../../adr/0026-studio-tutorial-execution.md).
- This assessment does not certify an installation or authorize live backend access.

## Local Control Evidence, 2026-10-29

The initial implementation uses a separate practice router with no privileged
adapters, a tutorial-only CLI startup guard, restrictive practice CSP/origin
checks, synthetic state and nonce-bound writes. Six desktop/mobile tutorial
browser cases, race-enabled practice tests and affected CLI tests pass locally.
Backend HTTP traps and direct-import checks reject live fallback; lifecycle tests
verify listener ownership and shutdown. Progress tests verify merge/reset fencing,
explicit degraded storage and exclusion of learner content.

No new package dependency or tour library was added. These checks cover the
local tutorial path, not arbitrary local code execution, production
authorization, real providers, a human pilot or full manual accessibility.

## Boundaries

| Component | Exposure/data | Required boundary |
|---|---|---|
| Normal Studio | User-selected live backend and unsaved work | Remains unchanged; tutorial must not cancel work or change context. |
| Practice origin/router | Synthetic resources and temporary learner text | Separate loopback origin; no privileged handlers, live clients, provider factories or credential resolvers. |
| Progress store | Versioned lesson/checkpoint metadata | Fixed per-user location, bounded validated data, atomic writes; no learner content or real target details. |
| Guidance/highlights | Lesson copy and DOM targets | Trusted bundled content; safe text rendering, no remote scripts/media or executable lesson content. |

The practice service may share the CLI process. The proposal isolates routes,
dependencies and browser origins, not arbitrary malicious code execution within
that process. Different ports do not isolate cookies.

## Design Risks and Required Controls

These are prospective design risks, not confirmed vulnerabilities in the current
Studio implementation.

| ID | Threat/severity | Failure mode | Required control |
|---|---|---|---|
| TUT-S01 | Elevation/tampering, High | A missed helper, gallery import, preview or retry reaches the privileged live adapter. | Separate practice router with synthetic dependencies only; no fallback. Audit all fetch/invoke/poll paths. Trap real clients/network/process execution in acceptance tests. |
| TUT-S02 | Disclosure, Medium | Practice stores real prompts, credentials, namespaces or server addresses in progress, logs or analytics. | Metadata-only local progress; ephemeral learner content; no payload logging, credential resolution or external analytics. Validate reset/version and unavailable-storage behavior. |
| TUT-S03 | Spoofing/integrity, Medium | Simulated readiness/approvals are mistaken for live evidence or reused as real capabilities. | Persistent practice labeling; simulated results never enter live stores, audit/usage ledgers or authority systems. No automatic promotion/export-to-live. |
| TUT-S04 | Availability, Medium | Other sites or abandoned sessions create unbounded listeners, state or progress writes. | Loopback binding, fixed local progress path, validated origin/host/session ownership, payload/session limits, bounded service lifecycle and CLI shutdown cleanup. |
| TUT-S05 | Tampering/repudiation, Medium | Completion advances despite failed actions, stale state or incompatible curriculum versions. | Semantic predicates, validated checkpoint IDs, explicit completion versus skipping, concurrency/reset generations and visible errors. Completion is learning metadata, never authorization evidence. |

## Network and Initialization Gate

- Practice must boot without depending on kubeconfig, an API server, real
  credentials, cloud accounts or provider availability.
- Do not register the existing privileged generic resource/invocation handlers
  against a tutorial flag. Their dependency boundary is the live CLI.
- Use only reviewed synthetic routes and pure local helpers. Unsupported schema
  discovery, gallery actions or protected operations must fail explicitly.
- Restrict practice connections/assets to its own origin; block live-origin and
  external fetches, frames and media. Bundle dependencies, with no CDN fallback.
- Do not forward cookies, authorization headers, target URLs or actual namespace
  lists. Practice does not inherit authority from a user-controlled request flag.
- Keep launch/progress/reset handling same-origin and ownership-bound. Derive
  redirect destinations from the owned loopback listener, not request-supplied URLs.

## Required Evidence

Before release, exercise every reachable practice action with real backend,
provider and process adapters trapped. Verify zero such calls on success,
validation failure, unknown routes, reload, resume and reset. Verify original
drafts/context/history remain unchanged, and stored progress excludes content.

Manual accessibility checks must verify keyboard escape/focus behavior and
unobscured controls; a highlighting dependency's advertised features are not
proof. The dependency version/license and actual implementation require a later
code review. No new Azure infrastructure or cloud access is proposed.
