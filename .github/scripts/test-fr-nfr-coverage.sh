#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHECKER="${ROOT_DIR}/.github/scripts/check-fr-nfr-coverage.sh"
PROJECT_FILE="${ROOT_DIR}/PROJECT.md"
BACKLOG_FILE="${ROOT_DIR}/BACKLOG.md"

fail() {
  echo "FR/NFR coverage test failed: $1"
  exit 1
}

# TC-0146: baseline backlog should pass.
if ! bash "${CHECKER}" --project "${PROJECT_FILE}" --backlog "${BACKLOG_FILE}" >/dev/null; then
  fail "expected baseline backlog coverage to pass"
fi

# TC-0147: remove TC references for FR-0001 and ensure checker reports a gap.
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
MUTATED_BACKLOG="${TMP_DIR}/BACKLOG-no-fr0001-tc.md"

awk '
!( $0 ~ /^\| TC-[0-9]{4} / && $0 ~ /FR-0001/ ) { print }
' "${BACKLOG_FILE}" > "${MUTATED_BACKLOG}"

set +e
OUTPUT="$(bash "${CHECKER}" --project "${PROJECT_FILE}" --backlog "${MUTATED_BACKLOG}" 2>&1)"
STATUS=$?
set -e

if [[ ${STATUS} -eq 0 ]]; then
  fail "expected mutated backlog coverage to fail"
fi
if [[ "${OUTPUT}" != *"Missing TC coverage:"* ]]; then
  fail "expected missing TC coverage section in output"
fi
if [[ "${OUTPUT}" != *"FR-0001"* ]]; then
  fail "expected FR-0001 gap in output"
fi

# Regression: mixed MVP marker (e.g., "✅ ... / ⏳ ...") must still be treated as MVP-scoped.
PARTIAL_PROJECT="${TMP_DIR}/PROJECT-partial-mvp.md"
PARTIAL_BACKLOG="${TMP_DIR}/BACKLOG-partial-mvp.md"

cat > "${PARTIAL_PROJECT}" <<'EOF'
### 29.1 Functional Requirements catalog
| ID | Requirement | MVP? | Covered by | Source §§ |
|---|---|---|---|---|
| `FR-0001` | Fully MVP | ✅ | MVP-1 | §x |
| `FR-0002` | Partially MVP | ✅ pgvector / ⏳ others | MVP-1 | §x |
### 29.2 Non-Functional Requirements catalog
- `NFR-0001` sample
### 29.3 Constraints
EOF

cat > "${PARTIAL_BACKLOG}" <<'EOF'
#### US-0001
**Traces:** FR-0001, NFR-0001

| ID | Test | Traces |
|---|---|---|
| TC-0001 | test | FR-0001, NFR-0001 |
EOF

set +e
OUTPUT="$(bash "${CHECKER}" --project "${PARTIAL_PROJECT}" --backlog "${PARTIAL_BACKLOG}" 2>&1)"
STATUS=$?
set -e

if [[ ${STATUS} -eq 0 ]]; then
  fail "expected partial-MVP fixture to fail due to uncovered FR-0002"
fi
if [[ "${OUTPUT}" != *"Missing US coverage:"* ]]; then
  fail "expected missing US coverage section for partial-MVP fixture"
fi
if [[ "${OUTPUT}" != *"Missing TC coverage:"* ]]; then
  fail "expected missing TC coverage section for partial-MVP fixture"
fi
if [[ "${OUTPUT}" != *"FR-0002"* ]]; then
  fail "expected FR-0002 gap for partial-MVP fixture"
fi

echo "FR/NFR coverage tests passed."
