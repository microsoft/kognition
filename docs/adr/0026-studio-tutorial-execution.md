# ADR-0026: Studio Tutorial Execution

- Status: Accepted.
- Date: 2026-10-29.
- Approval: Workspace user approved proceeding with the isolation-first plan on 2026-10-29.
- Related: [tutorial plan](../development-plans/features/studio/tutorial-mode-plan.md),
  [security controls](../development-plans/features/studio/tutorial-mode-security-review-architecture.md),
  [ADR-0014](0014-runtime-autonomy-gate-completion-and-recovery.md),
  [ADR-0015](0015-provider-integration-boundaries.md).
- Open items: Practice topology and local progress persistence. No existing
  PROJECT open item is closed by this record.

## Context

Studio's shared resource and invocation handlers use the CLI's actual backend.
Its mutable client state includes live drafts, namespaces, previews and graph
history. A visual tutorial flag alone cannot prevent accidental real mutations,
provider calls or loss of unsaved work.

The user selected isolated practice and a first-Agent task journey. Dedicated
practice areas have precedents in VS Code and Grafana; a separate browser tab is
the proposed Studio-specific implementation, not a universal tutorial convention.

## Priorities and Requirements

| Rank | Priority | Concrete requirement |
|---|---|---|
| First | No live effects | Tutorial routes must not resolve credentials or call live APIs, providers, controllers or tool processes, including on errors or unsupported actions. |
| Second | Preserve real work | Starting, resetting and exiting practice must not modify the live draft, selected context, graph history or ongoing executions. |
| Third | Familiar, accessible practice | Reuse the actual Studio interface with non-modal guidance; avoid a separate mock UI that drifts from its builders. |
| Fourth | Small local footprint | Use existing Go/vanilla-JavaScript infrastructure, local assets and metadata-only progress, without cloud services. |

## Options Against Priorities

| Option | No live effects | Preserve work | Familiar/access | Footprint and trade-off |
|---|---|---|---|---|
| Guide the connected environment | Does not meet the selected requirement; confirmations still permit real effects | Context/drafts require protection | Exact UI, but unsafe for low-stakes practice | Smallest code change; user declined this scope |
| In-page simulation sharing the live listener/session | Partial; every existing/future request path needs correct routing | Requires complete state, poller and async-response isolation | Seamless same-tab experience | Lower listener cost, higher coupling and regression risk |
| Separate practice origin/tab using shared UI assets | Meets the design requirement when the router has only synthetic dependencies and fail-closed networking | Original tab stays intact; no draft cloning across origins | Actual UI with explicit practice labels and accessible guidance | One additional loopback service plus lifecycle/progress handling |

## Decision

Use the third option. A practice-only router receives synthetic state and reviewed
pure helpers, never privileged resource/invocation adapters or live backend
configuration. Relative requests stay on the practice origin; unsupported routes
return explicit errors. Network policy and tests guard against live-origin or
external requests. A cosmetic query parameter cannot activate practice on a live
router.

Open practice only through an explicit, announced launch action. Add a
practice-only CLI entry path that does not depend on live backend initialization.
Reuse Studio assets/catalog, not a duplicated mock interface. Keep one bounded
practice service owned by the CLI lifecycle and clean up session data on reset/
exit/shutdown. Simulation is not an OS-level security boundary.

Store only versioned lesson and lesson-checkpoint IDs, completion/skipping, dismissal and
revision metadata in a per-user local progress store. This supports resume across
ephemeral origins without persisting user content or real target details.
Reconstruct synthetic lesson checkpoints on resume; custom practice edits are
temporary. Runtime Contract checkpoints are not tutorial progress.

This prioritizes absence of live effects and preservation of real work over
keeping the tutorial in the original tab. Dedicated practice UI should not
introduce a new authority or credential system.

## Consequences

- Requires a separate practice lifecycle/router and auditing all client request
  paths; a tour library cannot provide the isolation boundary.
- Original tabs and real executions remain independent. Cookies are not isolated
  by port alone; ignore/omit live credentials and restrict networking explicitly.
- Progress needs safe local persistence rather than assuming a stable browser
  origin. Errors, version changes and resets require visible handling.
- Shared UI must label simulated saves, status and output consistently.
- Driver.js may provide optional highlights only after an accessibility spike;
  the application owns lessons and progress regardless of that choice.
- Existing backend gates and accepted ADRs remain unchanged. Approval covers
  isolated tutorial development, not cloud resources, paid calls or live backend changes.

## Implementation Qualification

The foundation must prove practice-only boot, zero privileged dependency calls,
preservation of the original workspace and accessible non-modal interaction.
Local implementation evidence and remaining pilot/manual checks are tracked in
the linked plan; accepting this decision is not a release certification.
