# Studio Authoring Security Review

- Mode: Code.
- Date: 2026-10-29.
- Scope: Catalog generation, Studio resource forms/navigation, profile-digest
  helper and shared namespace-error reporting.
- Verdict: Passed for the reviewed local source scope. This is not deployment,
  dependency or production security certification.

## Reviewed Controls

| Boundary | Control and evidence |
|---|---|
| Resource authority | RuntimeApproval and ControlPlaneRole resources remain excluded from generic authoring. Generated, bound, running and controller-owned resources have read-only UI treatment. Browser tests assert disabled mutation controls. Backend authorization remains authoritative. |
| Secret references | Forms edit reference configuration only. Tests verify no secret resolution or browser-storage persistence; no credential manifests were read or modified. |
| Untrusted content | New labels, descriptions, errors and observed data use DOM text/value APIs. Map edits reject prototype-related keys. No new shell execution, SQL construction or arbitrary external URL resolver was added. |
| Profile binding | The POST digest helper reads at most four MiB and reuses the existing offline CLI parser, strict spec validation and canonical digest implementation. It performs no provider request or persistence. Pending pins block deployment; stale selections cannot apply results to another draft. |
| Operational effects | Launch/indexing-sensitive changes require explicit confirmation. Reference creation preserves the calling draft and never deploys a prerequisite implicitly. |
| Scope and failures | Catalogs retain canonical standalone fields and shipped Kubernetes schemas. Namespace discovery and resource-list failures are explicit errors, not successful empty-list responses. |

No confirmed introduced vulnerability was identified in these scoped checks.
The existing local UI listener and backend credential/authorization model were
not replaced. Client-side read-only controls do not replace server enforcement.
Installed Kubernetes authorization and additional protected human-review flows
remain separate acceptance work.

## Validation and Limits

### Tutorial Follow-Up, 2026-10-29

The approved tutorial now has its own synthetic router, loopback listener,
offline CLI boot path and metadata-only progress store. It does not register
the live resource/invocation adapters. Host/origin checks, nonce-bound writes,
practice-only CSP, size/state limits and HTTP deadlines constrain the new surface.
Reset generations fence resource writes and previews as well as lesson actions.
Progress uses atomic replacement and OS file locks; content and credentials
are excluded by the progress schema. Storage degradation is visible.

Six real-server desktop/mobile tutorial cases, race-enabled practice tests,
CLI lifecycle/offline boot tests and the existing Studio suite pass locally.
The source-composition guard rejects direct privileged adapter imports, and
HTTP traps observe no live calls. No tour library/dependency was added.
No confirmed introduced vulnerability was identified in this scoped review.
Manual assistive-technology qualification, the new-user pilot and production
assurance remain separate; this is not a whole-repository security clearance.

- Local Go tests cover catalog parity, scalar JSON encoding, canonical digest
  validation, embedded assets and actual Studio-to-API-server create/update/read
  round trips against an isolated test store.
- Browser tests cover redaction/reference-only configuration, protected-resource
  exclusions, read-only records, explicit launch confirmation and visible denials.
- Environment/credential manifests and browser dependencies remain ignored.
  Dependency manifests and lockfiles did not change.
- GitHub secret-scanning alerts returned an empty list. Dependabot alerts were
  disabled/unavailable with HTTP 403; code scanning reported no analysis with
  HTTP 404. Those responses are not clean vulnerability scans.
- No new dependency audit, whole-history secret scan, live provider request,
  cluster change or production authorization attestation was performed.
