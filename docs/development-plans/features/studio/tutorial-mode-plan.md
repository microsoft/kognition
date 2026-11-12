# Studio Tutorial Mode Plan

- Status: Initial core implementation locally verified; pilot and focused extensions remain open.
- Date: 2026-10-29.
- Product direction confirmed: isolated practice, a first-Agent journey, then optional focused lessons.
- Architecture: [ADR-0026](../../../adr/0026-studio-tutorial-execution.md), Accepted for isolated implementation.
- Safety gate: [architectural review](tutorial-mode-security-review-architecture.md).
- Related: [Studio guide](../../../studio.md), [Studio milestone](../../../../BACKLOG.md#epic-0023---kognition-studio-authoring-completion---in-progress).

## Recommendation

### Local Implementation Evidence, 2026-10-29

The workspace user approved proceeding. The practice-only CLI path and separate
loopback service now reuse Studio assets without registering live API adapters.
The native lesson panel implements the first-Agent journey, outcome checks,
explicit skipping/pause/reset, temporary synthetic state and versioned,
metadata-only progress with OS file locking and reset fencing.

Six real-server desktop/mobile tutorial browser cases pass. The existing Studio
suite reports 61 passed and one intentional desktop skip. Practice Go tests pass
with the race detector; affected CLI, Dashboard/API and catalog tests pass.
Tests cover offline boot despite a forced Kubernetes mode/missing kubeconfig,
owned listener reuse/shutdown, zero live HTTP calls, strict origins/CSP,
unsupported routes, correct/incorrect outcomes and content-free progress.

Version one uses the common/standalone resource view; Kubernetes-specific schema
lessons are not claimed. A native panel/focus helper was sufficient, so no tour
library or dependency was installed. Human pilot, manual screen-reader/zoom
qualification and the optional focused chapters remain open. No live cluster or
paid provider was used.

Provide an optional, task-based tutorial in the actual Studio interface, backed
by synthetic practice resources. The learner creates and previews a simple Agent,
with contextual instructions and meaningful progress checks. Do not start with
a tooltip tour of all 51 kinds or require credentials, Kubernetes, cloud access
or paid inference.

Use a persistent, non-modal lesson panel as the primary guide. Add a lightweight
highlight only when it helps locate a control. The real builder, explorer, YAML
view and preview remain operable. Offer **Start tutorial** in a nonblocking
first-use card and keep **Learn** permanently available.

Dedicated practice areas are established patterns; a separate browser tab is
not a universal onboarding convention. For Studio, recommend the same UI on a
separate loopback origin/tab because the normal server can mutate real resources.
The topology is accepted for isolated implementation; production certification
is outside this tutorial's scope.

## Evidence Applied

| Source | Relevant finding | Studio application |
|---|---|---|
| [NN/g: onboarding tutorials](https://www.nngroup.com/articles/onboarding-tutorials/) | Intrusive, out-of-context tours are often skipped; practice and contextual, revisitable help are stronger approaches. | Opt-in tasks, action-based progress, short instructions and reusable help. |
| [VS Code walkthrough guidelines](https://code.visualstudio.com/api/ux-guidelines/walkthroughs) and [Editor Playground](https://code.visualstudio.com/docs/editing/getting-started/tips-and-tricks#getting-started) | Actionable checklists and hands-on practice; avoid excessive steps and competing walkthroughs. | One primary journey; optional follow-on chapters only after the first outcome. |
| [Grafana getting started](https://grafana.com/docs/grafana/latest/fundamentals/getting-started/) | Grafana Play provides a separate place to explore examples before configuring an installation. | Practice data and a clear separation from the learner's real environment. |
| [WAI-ARIA dialog guidance](https://www.w3.org/WAI/ARIA/apg/patterns/dialog-modal/) | A modal makes outside content inert. | Do not put an interactive, app-wide lesson behind a modal focus trap. |

These sources support the learning pattern, not a claim that every product opens
tutorials in another browser tab or that a library guarantees accessibility.

## Learning Journey

Target approximately five to seven minutes as a design hypothesis, with no timer
or completion deadline. Use a preseeded synthetic Model so first use does not
become a provider-configuration exercise.

| Step | Learner action | Completion condition |
|---|---|---|
| Understand practice | Read the Practice banner and identify the explorer, builder and lesson panel. | Explicitly acknowledge that no real deployment or provider call will occur. |
| Create an Agent | Open Create, name an Agent and select the practice Model. | Valid draft with a reference to the seeded Model; do not require a fixed name. |
| Set its instructions | Write or revise a short inline instruction. | Nonempty, valid instructions changed by the learner; do not grade prose with keyword guesses. |
| Inspect the manifest | Open YAML, locate the name/model/instructions, then return to Builder. | View transitions preserve the same draft and values. |
| Save in practice | Use **Save in practice** and locate the saved Agent in the explorer. | Validated write/readback from the synthetic store, not merely a button click. |
| Preview a conversation | Submit a sample message. | A clearly marked deterministic **Simulated response** appears; zero real inference requests. |
| Try independently | Make a small change without highlighting and identify the next action. | Updated draft/manifest agree and the learner correctly distinguishes practice from live Studio. |

Each step explains one concept and one action, with optional **Why?** and
**Show me** help. Hints may reveal menus, expand sections or focus controls.
They must not fill answers, save resources, approve actions or submit runs.

After completion, offer short, optional chapters:

| Chapter | Outcome | Release scope |
|---|---|---|
| Reusable instructions | Create a Prompt through a reference and return to the Agent draft. | First extension |
| Memory | Create a practice MemoryClaim and attach it to the Agent. | Follow-on |
| Autonomy | Select supported Tool/Memory/Communication approval policies and an AutonomyClass. | Follow-on |
| Secret references | Create a dummy SecretRef without entering or resolving a credential. | Follow-on |
| Workflows | Assemble a small DAG and inspect a simulated run using keyboard-capable controls. | Later extension |

Do not teach deferred Model approval/cost-threshold enforcement, production
identity setup, arbitrary backend guarantees or protected authority administration
as working tutorial features.

## Experience and Recovery

- Starting is explicit; dismissal never prevents normal Studio use. The launch
  action announces that practice opens in a new tab.
- Preserve the original live tab, selected context, unsaved draft and graph
  history. Do not pause, cancel or replay existing real executions.
- Keep a visible Practice ribbon, simulated labels on status/output and distinct
  **Save in practice** wording. Never report a real deployment, audit receipt or
  conformance result from simulation.
- Provide Back, Pause, Resume, Restart lesson and Exit. Skipped steps remain
  skipped, not completed. No automatic next lesson or silent live-mode fallback.
- Re-evaluate prerequisites after Back, edits or resume. An error or missing
  anchor keeps the learner on the step with a useful retry/reset option.
- Reset affects practice data only. Do not offer automatic promotion, export-to-
  live or deployment of tutorial fixtures.

## Architecture and Integration

The proposed boundary is application isolation against accidental effects,
not an OS/container sandbox against arbitrary code execution in the CLI process.

| Surface | Planned change |
|---|---|
| [Studio server](../../../../cmd/kognitionctl/cmd/studio.go) and [local listener](../../../../cmd/kognitionctl/cmd/local_ui_server.go) | Own one bounded practice service on a different ephemeral loopback port. A user-activated new-tab link can prepare/redirect to it without asynchronous popup assumptions. Keep lifecycle/shutdown explicit. |
| Practice-only launch | `kognitionctl studio --tutorial` bypasses live backend initialization; the first lesson uses the common/standalone resource view. |
| Practice router/store, new files | Register only synthetic CRUD/preview and reviewed pure parse/digest helpers. Never register the privileged generic resource/invoke handlers or pass live clients, credential resolvers, controllers or provider factories. Unknown/unsupported requests fail explicitly. |
| [Client API helpers](../../../../cmd/kognitionctl/cmd/studio_assets/app.js) | Centralize all request entry points behind an explicit environment/client contract, selected before boot. Keep production behavior unchanged. Tutorial activation cannot be a cosmetic query-string flag on the live backend. |
| [Resource forms](../../../../cmd/kognitionctl/cmd/studio_assets/resources.js) and generated catalog | Reuse the same forms, scope rules and supported-field definitions. Cover direct profile-digest fetches as well as ordinary CRUD. |
| Lesson engine, new client module | Versioned lesson IDs, stable semantic target anchors, prerequisite/completion predicates and bounded recovery. Re-resolve targets after renders instead of storing detached DOM nodes or relying on translated label text. |
| [Navigation/styles](../../../../cmd/kognitionctl/cmd/studio_assets/index.html) | Add Learn, the first-use offer, the lesson panel, practice labels and responsive layout without changing the existing theme. |

Inventory every current request path: metadata/namespaces, CRUD, chat/workflow/
tool invokes, run polling, schema discovery, gallery/import, YAML parsing and
profile pinning. An overlooked path must hit an unavailable practice route,
never a real service. Preseeded Models use synthetic configuration and reserved
invalid example endpoints; never resolve a real SecretRef or register a fake
provider in the live CPI registry.

Keep the trusted practice environment marker separate from the resource-schema
mode (`standalone` or `kubernetes`). Use a virtual namespace, not the user's real
context; exercise both schema modes without querying either live backend.

Require practice-origin network restrictions, no remote assets/CDNs, validated
launch/session ownership, same-origin write checks and no live URL forwarding.
Different ports isolate origin storage, not cookies; do not reuse or forward
live cookies/authorization. See the architectural review for release controls.

## Progress and Privacy

Persist only curriculum version, lesson and lesson-checkpoint IDs, completed/skipped step
IDs, dismissal preference and a revision. Use a CLI-owned, per-user local progress
store: browser storage alone cannot survive ephemeral port changes reliably.

- Never persist prompts, chat messages, YAML, drafts, credentials, real resource
  names, namespaces or backend addresses as learning progress.
- Resume reconstructs synthetic checkpoints. Explain that custom practice edits
  are temporary and do not survive a fresh session; do not promise draft recovery.
- Validate progress IDs/versions, bound payloads, use safe atomic writes and
  explicit reset/concurrency semantics. Report persistence failures visibly;
  continuing without persistence is an explicit degraded mode.
- No external analytics or tracking by default. Assess learning through an
  opt-in pilot; tutorial completion is not operational or authorization evidence.

## Accessibility and Presentation

Target WCAG 2.2 AA and add reduced-motion support:

- All actions work by keyboard; no focus trap in the lesson panel. Preserve
  graph/editor shortcuts. Escape closes the innermost open control before
  pausing guidance; do not intercept typing or arrow-key editing globally.
- Announce meaningful step completion through a polite status region without
  moving focus. **Show me** moves focus only on explicit request.
- Keep focused controls visible at narrow widths and zoom. Reserve panel space
  on mobile rather than covering fields or the software keyboard.
- Any hover/focus hint must be dismissible, hoverable and persistent while
  needed. Provide a text-only checklist when highlights are unavailable.
- Respect `prefers-reduced-motion`; no mandatory animation, pulsing or timed
  advancement. Animation-from-interaction is a related AAA criterion adopted
  here as an additional requirement, not mislabeled as AA.

References: [keyboard](https://www.w3.org/WAI/WCAG22/Understanding/keyboard.html),
[focus visibility](https://www.w3.org/WAI/WCAG22/Understanding/focus-not-obscured-minimum.html),
[status messages](https://www.w3.org/WAI/WCAG22/Understanding/status-messages.html),
[hover/focus content](https://www.w3.org/WAI/WCAG22/Understanding/content-on-hover-or-focus.html),
[reduced motion](https://www.w3.org/WAI/WCAG22/Understanding/animation-from-interactions.html).

## Component Choice

| Option | Assessment |
|---|---|
| Native lesson panel and application-owned state machine | Required baseline; fits current vanilla JavaScript and keeps progress/safety independent of a tour library. |
| [Driver.js](https://driverjs.com/docs/configuration) as an optional highlight adapter | Preferred candidate for the spike: vanilla JavaScript, MIT [license](https://github.com/nilbuild/driver.js/blob/master/license), positioning/lifecycle hooks. Keep step completion in Studio; disable conflicting global keyboard control. Verify non-modal behavior, dynamic targets, mobile and reduced motion before adoption. |
| [Shepherd](https://www.shepherdjs.dev/pricing) | Viable tour tooling, but commercial-distribution licensing adds review obligations. Not the preferred default for this plan. |

Pin and bundle any adopted dependency locally with its required license notices.
Do not use CDN `latest`, introduce React or a hosted onboarding service, or patch
vendor internals to obtain required accessibility. If the highlight adapter
fails its spike, ship the fully usable panel without it.

## Delivery and Acceptance

ADR-0026 is accepted. Initial local implementation is tracked under US-0099 in
the backlog. This evidence does not close the human pilot, manual accessibility
qualification or follow-on chapters.

| Phase | Deliverable | Exit gate |
|---|---|---|
| Foundation | Accept ADR; prototype unprivileged practice boot, shared UI and synthetic round trip; spike highlight adapter. | Works without a real backend/kubeconfig; trap clients observe zero real API/provider/DB/process effects. |
| Core lesson | First-Agent journey, semantic predicates, stable anchors, practice labels and recovery. | Completion requires actual synthetic task outcomes; errors, missing targets and skipped steps cannot produce false completion. |
| Resume and access | Local progress, version/reset handling, keyboard/screen-reader/mobile behavior. | Port changes, reload, unavailable storage, reduced motion and focus-order cases pass. |
| Pilot | Observe five to eight first-time users completing the practice task and a short unprompted variant. | Every participant distinguishes practice from live effects; task failures and excessive friction feed revisions. Duration/completion targets remain hypotheses until measured. |
| Extensions | Add focused chapters based on pilot needs, with their own goals and predicates. | Each chapter remains optional, isolated and testable; do not expand into a mandatory full-platform tour. |

Automated acceptance must exercise real served practice routes plus browser UI:

1. Intercept/trap every privileged backend dependency and outbound destination;
   exercise all reachable requests, including gallery, refresh and profile pinning.
2. Verify unknown routes, forged context flags, blocked network, failed saves and
   missing fixture references fail closed with no live fallback.
3. Keep the original live draft/namespace/history unchanged while practice is
   started, reset, reloaded and exited; existing independent live work is not cancelled.
4. Verify progress contains no learner content, versions cannot silently skip new
   requirements, and resets cannot delete real resources or restore stale progress.
5. Test keyboard-only, screen-reader announcements, desktop/mobile, zoom, missing
   anchors, re-rendering and reduced motion. Run existing Studio regressions.

## Terminology and Review

| Term | Meaning |
|---|---|
| Tutorial mode | The optional guided learning experience; distinct from the existing repository tutorial document. |
| Practice workspace | The same Studio UI with synthetic resources and no live backend forwarding; not an execution sandbox. |
| Lesson | One bounded learning outcome with explicit prerequisite and completion predicates. |
| Lesson checkpoint | A versioned synthetic reconstruction point, not a saved user draft or Runtime Contract checkpoint. |

Before broad release: review the first lesson with new users, finish manual
accessibility qualification and revise the copy based on the pilot. Review any
future highlighting library before adoption. The user-approved learning/safety
goals remain the boundary for extensions.
