# Kognition Studio

Studio provides local visual resource authoring through the existing CLI backend.
It does not install or activate a platform, grant permissions, or certify an
installation.

## Start Studio

Build the current source or run it directly:

```powershell
go run .\cmd\kognitionctl studio --mode standalone --server http://127.0.0.1:8080
```

**Bash (Linux/macOS):**

```bash
go run ./cmd/kognitionctl studio --mode standalone --server http://127.0.0.1:8080
```

Use the printed local URL. Select Kubernetes mode only with the intended existing
kubeconfig, context and namespace. Studio uses that caller's existing access;
Deploy changes the selected backend and remains subject to its admission,
authorization and installation controls.

## Author Resources

- **Create** opens a searchable picker with common choices first. Use **Common**
  for eight frequent resource types, or filter by **Build**, **Test**, **Govern**
  and **Operate**. **All** retains every supported authoring and launch type.
- Search matches readable labels, canonical kind names and descriptions within
  the selected category. Each result shows its catalog category and a short
  description; **Run** identifies an execution resource. Choosing a result opens
  its editor without deploying or starting work.
- Search receives focus when the picker opens. Up/Down navigate matching
  results; Home/End work within results. Tab leaves the results without visiting
  every resource, and Escape closes the picker and returns focus to Create.
  Search/filters remain visible while the result list scrolls on desktop/mobile.
- Use **Create** for AutonomyClass, MemoryClaim, SecretRef, Prompt, Model,
  ModelInvocationProfile, storage/model classes, policies, quotas, roles, trust,
  workloads, services and supported scaling.
- Existing Agent, Workflow, Tool, grant, knowledge and evaluation builders remain
  available. **All API fields** switches to structured controls derived from the
  canonical resource types and shipped CRD schemas; **Guided builder** returns.
- When creating an OpenAPI Tool, open **Schema** in the guided builder and select
  **Edit schema in YAML**. Add `spec.schema`, select **Apply YAML**, then deploy.
  Live schema introspection is available after deployment.
- Objects expand into nested controls. Arrays have explicit add/remove actions;
  maps have named entries. Domain text and arbitrary JSON payloads retain
  text/code inputs.
- Cluster-scoped classes omit the namespace. Standalone requests use the existing
  default-namespace transport while the API canonicalizes class scope.
- **Create referenced resource** preserves the calling draft. Deploy the
  prerequisite explicitly, then return to the original draft to use its name.
  Returning without deploying does not silently deploy or reference that draft.
- For a Model's invocation profile, use **Pin selected profile digest**. Studio
  reads the selected namespace-local profile and uses the same offline canonical
  validation/digest code as `invocation-profile-digest`. It performs no inference.
- Invocation profiles are immutable in the editor. Use **Create new profile
  revision** to copy a spec into a new named resource.
- In Kubernetes mode the Workflow builder's **Team** step edits a collaboration
  step: strategy, members, allowed interactions and limits. It shows the
  admission rules as problems and updates references when a member is renamed.
  In standalone mode it is read-only. See
  [Workflow collaboration](workflow-collaboration.md).

YAML view and structured editing preserve unrelated imported fields, explicit
zero/false values and empty collections. Server-owned identity/version fields
remain attached to updates. API conflicts and validation errors remain visible.
Manifests with integers outside JavaScript's safe range cannot be redeployed by
Studio; use the CLI to preserve exact values.

## Safety Boundaries

| Resource or action | Studio behavior |
|---|---|
| SecretRef | Edits backend reference/key configuration only; never resolves secret values or adds Vault support. |
| Generated or bound resources | Read-only when controller-owned, already executing, bound, or carrying issued identity state. |
| Run requests, Solutions, knowledge indexing/schedules | Require explicit confirmation before applying changes that can start work or consume provider budget. Existing runs are read-only. |
| RuntimeApproval and ControlPlaneRole resources | Unavailable through generic Studio CRUD. Use the supported authenticated authority interface. |
| Human decisions | Existing Workflow approval UI remains available. Additional AutonomyReview/authority administration is not implemented by a generic form; inspection is read-only. |
| ModelAutoscaler | Explicitly unavailable while backend model-tier scaling is deferred. |
| Broader autonomy cost thresholds and shared-memory guarantees | Compatibility fields are preserved but not offered as newly implemented enforcement. |

The catalog accounts for every registered kind. This does not imply every kind
supports generic creation or that every provider/backend capability is deployed.
Local browser and API-adapter tests are not installed Kubernetes authorization,
identity, provider or release certification.

## Tutorial Mode

Use **Learn** in local Studio to open the same interface in a separate practice
tab. A nonblocking first-use card also offers the lesson; **Not now** remembers
the preference locally, while Learn remains available.

To learn without an API server, kubeconfig or cloud account:

```powershell
go run .\cmd\kognitionctl studio --tutorial --open
```

**Bash (Linux/macOS):**

```bash
go run ./cmd/kognitionctl studio --tutorial --open
```

Tutorial-only startup bypasses live backend initialization, even if a live CLI
mode was configured. It binds to `127.0.0.1`; remote/public binding is rejected.
The first lesson uses the common/standalone resource view and the virtual
`tutorial` namespace.

The lesson covers creating an Agent, selecting a synthetic Model, writing
instructions, viewing YAML, saving to the practice store, previewing a clearly
simulated response and making an independent change. **Show me** locates controls
without filling answers or submitting work. Back, Pause, Resume, Skip and
Restart are explicit; skipped steps do not count as completed tasks.

- The Practice banner and **Save in practice** identify the environment.
- No live resource, provider, credential, tool process or production audit/usage
  store is used. Unsupported operations fail instead of falling back to real
  Studio services.
- The original Studio tab, unsaved draft and existing executions stay separate.
- Only versioned lesson progress/preferences are stored under the current user's
  configuration directory in `kognition/studio/tutorial-progress.json`.
  Prompts, messages, manifests, resource names and credentials are not progress.
- Custom practice data stays in memory. Exit/Restart discards it; closing a tab
  without Exit may leave it in memory until the CLI stops. Restarting the CLI
  resumes a generic lesson checkpoint, not custom text. Storage failures are
  reported and practice can continue explicitly in memory-only mode.

The initial implementation uses a native, non-modal lesson panel and keyboard
focus hints, with no external onboarding library or CDN. Further Prompt, memory,
autonomy, SecretRef and workflow chapters, a first-time-user pilot and manual
screen-reader qualification remain follow-on work. See the
[plan and evidence](development-plans/features/studio/tutorial-mode-plan.md).

## Maintain and Validate

After changing resource types or CRD schemas, regenerate the checked-in catalog:

```powershell
go run .\tools\studio-catalog
go test .\tools\studio-catalog .\cmd\kognitionctl\cmd -run 'Test(Catalog|Studio|Dashboard|LocalUI)' -count=1
npm run test:studio:browser -- --workers=2
npm run test:studio:tutorial
go test -race .\internal\studiotutorial -count=1
go build .\cmd\kognitionctl
```

**Bash (Linux/macOS):**

```bash
go run ./tools/studio-catalog
go test ./tools/studio-catalog ./cmd/kognitionctl/cmd -run 'Test(Catalog|Studio|Dashboard|LocalUI)' -count=1
npm run test:studio:browser -- --workers=2
npm run test:studio:tutorial
go test -race ./internal/studiotutorial -count=1
go build ./cmd/kognitionctl
```

The generator checks registry coverage and preserves standalone/Kubernetes wire
shapes, including resources with fields outside `spec`. The catalog test rejects
stale checked-in assets. Browser tests cover desktop/mobile authoring, references,
round trips, explicit launches, visible failures and read-only protections.
