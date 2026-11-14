# Change-Based Validation Gates

- Status: user-approved process revision.
- Date: 2026-10-02.
- Automation: [planner](../tools/securitygate/impact.go), [CI](../.github/workflows/ci.yml).

Routine changes require validation of affected behavior and security controls.
Changing the repository tree invalidates its whole-tree attestation; it does not
require repeating every unaffected SG-01 through SG-13 campaign step.

## Publication Boundaries

| Boundary | Required evidence | Approval scope |
| --- | --- | --- |
| Development and source push | Existing automated CI, affected tests, secret check, reviewed change plan and security deltas when applicable | Source publication; no release or deployment certification |
| Release candidate and artifact publication | Broad quality, dependency, configuration, image and packaging checks for supported release profiles; verified reuse of unchanged control evidence | Exact released artifacts and exact residual-risk decisions |
| Installation and activation | Guarded installation, fresh target-specific acceptance reports, exact inventory/UID/image bindings and administrator approval | Only the reviewed installation and transaction |

The planner selects checks. Exit zero means the plan was generated successfully;
`clearanceGranted` is always false. Existing CI checks remain required. A passing
plan does not replace tests, scanner findings, risk review or activation approval.
Source publication must not be described as current SG-11/SG-12 certification.

## Generate A Plan

Select the most recent reviewed baseline for the intended assurance scope. CI uses
the event's base commit for change detection; that commit is not automatically a
certified baseline. Local planning can use an older recorded acceptance commit.

The planner commands work in Bash and PowerShell:

```sh
go run ./tools/securitygate plan --base 261c5038f1c578bdc493b5bd4cb5fb7160fc0b88 --head HEAD
go run ./tools/securitygate plan --base 261c5038f1c578bdc493b5bd4cb5fb7160fc0b88 --head HEAD --format markdown

# Add release-candidate checks only when preparing an artifact release.
go run ./tools/securitygate plan --base 261c5038f1c578bdc493b5bd4cb5fb7160fc0b88 --head HEAD --release
```

The tool compares committed Git trees without mutating the repository. Added,
deleted, renamed and mode-changed files are included; renames are represented as
deletion plus addition so the old security surface cannot disappear unnoticed.
Uncommitted/untracked changes are excluded and explicitly require worktree review.
Ignored local credentials and outputs remain outside the committed candidate.

CI publishes a JSON artifact and job summary. If the event has no usable baseline,
`--base EMPTY` inspects the whole candidate and requires classification review.
The new job is advisory: it does not alter dependencies or thresholds of existing
test, coverage, integration, audit, secret/misconfiguration scan or build jobs.

## Change Classes

| Class | Required local work |
| --- | --- |
| Documentation | Links, examples and secret checks; review changes to operational or trust contracts |
| Ordinary feature or bug fix | Changed-package tests and affected public consumers, applicable lint/runtime checks, existing CI |
| Security surface | Above plus targeted admission/authentication/authorization/lifecycle/isolation tests and privilege/finding delta review |
| Dependencies or images | Relevant dependency audits, fresh image/build checks and affected runtime tests |
| Chart or configuration | Affected profile rendering/scanning, chart/archive parity and relevant permission/network tests |
| Gate policy, unknown path, executable/symlink/submodule | Human classification and selected-check review; no automatic clearance and no automatic full campaign |

The classifier is deliberately conservative and cannot infer all transitive code
dependencies or semantic changes inside documentation. Review new coupling and
update the declared control surfaces when needed. Security-sensitive paths and
shared API, authentication, store and SDK inputs invalidate relevant fingerprints.
Policy, workflow and verifier edits require review because the tool cannot approve
changes to its own assurance rules.

Use `requiredChecks` and `checkGuidance` with [testing commands](testing.md).
Select deployed tests only when the affected behavior depends on an actual API
server, mounted credential, kubelet stream, enforcing CNI or producer lifecycle.
Run deployed acceptance only on an explicitly reviewed isolated target. Never
stop an active demo or production installation merely to validate a docs change.

## Reuse Evidence

Each control has independent baseline/candidate input fingerprints and declared
input paths. The fingerprints hash sorted Git paths, modes and blob identities,
the planner schema version and control identity. They are input-change detectors,
not signatures, executable attestations or a complete dependency graph.

Reuse a previous passing result only after reviewing all of these conditions:

1. The trusted receipt identifies the test/check, passing outcome, raw report hash
   and the reviewed baseline source/artifact scope.
2. Required control implementation, consumers, test code, generated inputs,
   dependency locks and configuration inputs are unchanged.
3. Commands, toolchain, scanner/database versions, base images, feature profile
   and relevant environment identities match the accepted evidence scope.
4. Applicable freshness and incident/advisory constraints are satisfied. A new
   advisory or changed scanner database can require a new scan without source drift.
5. The receipt's residual-risk approval still covers the exact rule, target,
   source and finding hashes. Changed findings need a separate owner decision.

The current planner reports `inputsUnchanged` and `changedControlInputs`; it does
not import receipts or automatically mark old evidence accepted. Record selected
reruns and inherited receipts in the candidate review. Missing evidence requires
the missing check or an explicit scope decision, not an invented passing result.

Functional acceptance remains useful while its relevant inputs and environment
stay within the reviewed scope. Scanner freshness remains governed by its existing
rules: the strict scan verifier's exact candidate/report/input checks and 24-hour
window are unchanged. Activation still uses its existing exact-target freshness,
approval lifetime, writer freeze and compensation/recovery requirements.

Timestamp-only chart archive changes can reuse payload-specific evidence after
comparing every extracted member and reviewing archive metadata. Record the new
archive hash; raw artifact hashes and scanner approvals cannot be silently carried
forward. Executable/chart payload changes require their affected checks.

## Existing Security Backlog

[SG-01 through SG-10](development-plans/features/knowledge-provider-execution/security-gate-backlog.md)
remain the control baseline and regression catalog. Their historical closure is
limited to recorded candidates, profiles and environments.

- SG-11 for later changes: affected functional/quality reruns and explicitly
  verified inherited results; broad automation still runs in CI.
- SG-12 for later changes: scans of changed security inputs and exact finding
  deltas, retaining residual risks and freshness rules. A broad fresh release scan
  remains required when preparing release artifacts.
- SG-13 for later changes: review the exact staged/committed candidate and source
  publication requirements. When authorized, push normally and verify both remotes.

Reopen the affected controls after a security regression, trust-boundary redesign,
unexplained evidence discrepancy or changed deployment profile. Determine the
necessary scope explicitly; a new whole-tree hash alone does not reopen all gates.
