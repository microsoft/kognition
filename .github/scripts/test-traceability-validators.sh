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

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

pushd "${TMP_DIR}" >/dev/null

git init -q
# Configure identity required for test commits.
git config user.name "traceability-test"
git config user.email "traceability-test@example.com"

printf "ok\n" > sample.txt
git add sample.txt
git commit -q -m "T-0001: initial valid commit"
VALID_SHA="$(git rev-parse HEAD)"

printf "changed\n" >> sample.txt
git add sample.txt
git commit -q -m "fix stuff"
INVALID_SHA="$(git rev-parse HEAD)"

if ! bash "${COMMIT_VALIDATOR}" "${VALID_SHA}^!" >/dev/null; then
  fail "expected valid commit subject to pass"
fi

set +e
OUTPUT="$(bash "${COMMIT_VALIDATOR}" "${INVALID_SHA}^!" 2>&1)"
STATUS=$?
set -e
if [[ ${STATUS} -eq 0 ]]; then
  fail "expected invalid commit subject to fail"
fi
if [[ "${OUTPUT}" != *"Invalid commit message"* ]]; then
  fail "expected actionable invalid commit message output"
fi

popd >/dev/null

if ! PR_BODY="Implements US-0001 and US-0054" bash "${PR_VALIDATOR}" >/dev/null; then
  fail "expected PR validator pass with US reference"
fi

set +e
PR_OUTPUT="$(PR_BODY="No story reference" bash "${PR_VALIDATOR}" 2>&1)"
PR_STATUS=$?
set -e
if [[ ${PR_STATUS} -eq 0 ]]; then
  fail "expected PR validator to fail without US reference"
fi
if [[ "${PR_OUTPUT}" != *"Expected at least one user story reference"* ]]; then
  fail "expected actionable PR validation output"
fi

echo "Traceability validator tests passed."
