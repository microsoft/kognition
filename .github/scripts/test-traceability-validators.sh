#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMMIT_VALIDATOR="${ROOT_DIR}/.github/scripts/validate-commit-messages.sh"
PR_VALIDATOR="${ROOT_DIR}/.github/scripts/validate-pr-description.sh"

fail() {
  echo "traceability validator test failed: $1"
  exit 1
}

expect_commit_pass() {
  local output
  if ! output="$(bash "${COMMIT_VALIDATOR}" "$1" 2>&1)"; then
    fail "expected $2 to pass: ${output}"
  fi
  if [[ "${output}" != "Commit message validation passed." ]]; then
    fail "expected a successful validation result for $2: ${output}"
  fi
}

expect_commit_fail() {
  local output
  if output="$(bash "${COMMIT_VALIDATOR}" "$1" 2>&1)"; then
    fail "expected $2 to fail"
  fi
  if [[ "${output}" != *"Invalid commit message"* ]]; then
    fail "expected actionable invalid commit message output for $2: ${output}"
  fi
}

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

pushd "${TMP_DIR}" >/dev/null

git init -q
git config user.name "traceability-test"
git config user.email "traceability-test@example.com"

printf "ok\n" > sample.txt
git add sample.txt
git commit -q -m "chore: initialize Kognition repository"
ROOT_SHA="$(git rev-parse HEAD)"
ROOT_TREE="$(git rev-parse HEAD^{tree})"
expect_commit_pass "${ROOT_SHA}^!" "a conventional initial import"

printf "changed\n" >> sample.txt
git add sample.txt
CHANGED_TREE="$(git write-tree)"

for SUBJECT in \
  "T-0001: implement API server" \
  "T-0191 verify CI" \
  "T-0001-fix traceability" \
  "fix: validate commit ranges" \
  "chore(deps-dev): Bump qs from 6.16.0 to 6.17.1" \
  "ci(security): update scanner action" \
  "feat(api)!: change the resource contract" \
  "fix!: reject invalid inputs" \
  "Merge branch 'main'" \
  'Revert "fix: validate commit ranges"'; do
  SHA="$(git commit-tree "${CHANGED_TREE}" -p "${ROOT_SHA}" -m "${SUBJECT}")"
  expect_commit_pass "${SHA}^!" "'${SUBJECT}'"
done

EMPTY_PLAN_SHA="$(git commit-tree "${ROOT_TREE}" -p "${ROOT_SHA}" -m "Initial plan")"
expect_commit_pass "${EMPTY_PLAN_SHA}^!" "an empty planning commit"

for SUBJECT in \
  "Initial plan" \
  "implement API server" \
  "T-00010: invalid task ID" \
  "T-0001suffix" \
  "fix: " \
  "fix:no separator" \
  "fix(): empty scope" \
  "chore changes without a colon"; do
  SHA="$(git commit-tree "${CHANGED_TREE}" -p "${ROOT_SHA}" -m "${SUBJECT}")"
  expect_commit_fail "${SHA}^!" "'${SUBJECT}' with file changes"
done

INVALID_ROOT_SHA="$(git commit-tree "${ROOT_TREE}" -m "untracked initial import")"
expect_commit_fail "${INVALID_ROOT_SHA}^!" "an unrelated invalid root commit"
expect_commit_fail "${ROOT_SHA}..${SHA}" "a range containing an invalid commit"

for RANGE in "not-a-revision" "${ROOT_SHA}..not-a-revision"; do
  if OUTPUT="$(bash "${COMMIT_VALIDATOR}" "${RANGE}" 2>&1)"; then
    fail "expected an invalid revision to fail: ${OUTPUT}"
  fi
done

if ! bash "${COMMIT_VALIDATOR}" "${ROOT_SHA}..${ROOT_SHA}" >/dev/null; then
  fail "expected an empty commit range to pass"
fi

popd >/dev/null

for BODY in "Implements US-0001 and US-0054" "Update dependencies and verify CI"; do
  if ! PR_BODY="${BODY}" bash "${PR_VALIDATOR}" >/dev/null; then
    fail "expected a non-empty PR description to pass"
  fi
done

for BODY in "" " " $'\t\r\n'; do
  if PR_OUTPUT="$(PR_BODY="${BODY}" bash "${PR_VALIDATOR}" 2>&1)"; then
    fail "expected an empty or whitespace-only PR description to fail"
  fi
  if [[ "${PR_OUTPUT}" != *"PR description is empty"* ]]; then
    fail "expected actionable PR validation output: ${PR_OUTPUT}"
  fi
done

echo "Traceability validator tests passed."
