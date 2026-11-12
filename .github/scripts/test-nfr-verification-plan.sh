#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHECKER="${ROOT_DIR}/.github/scripts/check-nfr-verification-plan.sh"
BACKLOG_FILE="${ROOT_DIR}/BACKLOG.md"

fail() {
  echo "NFR semantic checker test failed: $1"
  exit 1
}

# Baseline should pass.
if ! bash "${CHECKER}" --backlog "${BACKLOG_FILE}" >/dev/null; then
  fail "expected baseline NFR semantic check to pass"
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

# Negative test 1: mapped TC exists but no longer traces the mapped NFR.
MUTATED_MISALIGNED="${TMP_DIR}/BACKLOG-misaligned.md"
awk '
{
  line = $0
  if (line ~ /^\| TC-0194 /) {
    gsub(/NFR-0006, /, "", line)
    gsub(/, NFR-0006/, "", line)
    gsub(/NFR-0006/, "", line)
  }
  print line
}
' "${BACKLOG_FILE}" > "${MUTATED_MISALIGNED}"

set +e
OUTPUT="$(bash "${CHECKER}" --backlog "${MUTATED_MISALIGNED}" 2>&1)"
STATUS=$?
set -e

if [[ ${STATUS} -eq 0 ]]; then
  fail "expected misaligned backlog to fail"
fi
if [[ "${OUTPUT}" != *"NFR/TC trace misalignment:"* ]]; then
  fail "expected misalignment section"
fi
if [[ "${OUTPUT}" != *"NFR-0006 maps to TC-0194"* ]]; then
  fail "expected NFR-0006 -> TC-0194 misalignment detail"
fi

# Negative test 1b: NFR token in TC description must not satisfy Traces alignment.
MUTATED_DESC_ONLY="${TMP_DIR}/BACKLOG-desc-only.md"
awk '
{
  line = $0
  if (line ~ /^\| TC-0194 /) {
    gsub(/all references\./, "all references for NFR-0006.", line)
    gsub(/\| NFR-0006, MVP-3 \|/, "| MVP-3 |", line)
  }
  print line
}
' "${BACKLOG_FILE}" > "${MUTATED_DESC_ONLY}"

set +e
OUTPUT="$(bash "${CHECKER}" --backlog "${MUTATED_DESC_ONLY}" 2>&1)"
STATUS=$?
set -e

if [[ ${STATUS} -eq 0 ]]; then
  fail "expected description-only NFR mutation to fail"
fi
if [[ "${OUTPUT}" != *"NFR/TC trace misalignment:"* ]]; then
  fail "expected misalignment section for description-only mutation"
fi
if [[ "${OUTPUT}" != *"NFR-0006 maps to TC-0194"* ]]; then
  fail "expected NFR-0006 -> TC-0194 misalignment detail for description-only mutation"
fi

# Negative test 2: NFR plan references an unknown TC.
MUTATED_UNKNOWN="${TMP_DIR}/BACKLOG-unknown-tc.md"
awk '
{
  line = $0
  if (line ~ /^\| NFR-0006 /) {
    gsub(/TC-0194/, "TC-9999", line)
  }
  print line
}
' "${BACKLOG_FILE}" > "${MUTATED_UNKNOWN}"

set +e
OUTPUT="$(bash "${CHECKER}" --backlog "${MUTATED_UNKNOWN}" 2>&1)"
STATUS=$?
set -e

if [[ ${STATUS} -eq 0 ]]; then
  fail "expected unknown-TC backlog to fail"
fi
if [[ "${OUTPUT}" != *"Unknown TCs referenced by NFR plan:"* ]]; then
  fail "expected unknown-TC section"
fi
if [[ "${OUTPUT}" != *"NFR-0006 references unknown test case TC-9999"* ]]; then
  fail "expected unknown TC-9999 detail"
fi

echo "NFR verification-plan semantic tests passed."
