#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

fail() {
  echo "trace CLI git integration test failed: $1"
  exit 1
}

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
REPO_DIR="${TMP_DIR}/repo"
mkdir -p "${REPO_DIR}/tools"

cp "${ROOT_DIR}/tools/trace" "${REPO_DIR}/tools/trace"
chmod +x "${REPO_DIR}/tools/trace"

cat > "${REPO_DIR}/BACKLOG.md" <<'EOF'
#### US-0001
**Traces:** FR-0001, FR-0002 | NFR-0001

**Tasks:**

| ID | Task | US |
|---|---|---|
| T-0001 | test task | US-0001 |

**Test Cases:**

| ID | Test | Traces |
|---|---|---|
| TC-0001 | test case | FR-0001, FR-0002, NFR-0001 |
EOF

pushd "${REPO_DIR}" >/dev/null
git init -q
git config user.name "trace-cli-test"
git config user.email "trace-cli-test@example.com"

git commit -q --allow-empty -m "unrelated initial history"
OUTPUT="$(bash tools/trace FR-0001)"
if [[ "${OUTPUT}" != *"  - (none found)"* ]]; then
  fail "expected explicit no-match result for unrelated history"
fi

printf "x\n" > a.txt
git add a.txt BACKLOG.md tools/trace
git commit -q -m "T-0001: seed trace fixtures"

printf "y\n" >> a.txt
git add a.txt
git commit -q -m "FR-0001: add trace-linked change"

git commit -q --allow-empty -m "FR-0001 T-0001: linked through multiple IDs"
git commit -q --allow-empty -m "unrelated final history"
OUTPUT="$(bash tools/trace FR-0001)"

if [[ "${OUTPUT}" != *"Commits:"* ]]; then
  fail "expected commits section in FR-0001 output"
fi
if [[ "${OUTPUT}" != *"FR-0001: add trace-linked change"* ]]; then
  fail "expected commit match by FR id"
fi
if [[ "${OUTPUT}" == *"(git history unavailable)"* ]]; then
  fail "expected git history to be available in temp repo test"
fi
if [[ "${OUTPUT}" == *"unrelated"* ]]; then
  fail "expected unrelated commits to be excluded"
fi
MULTI_MATCH_COUNT="$(grep -c 'linked through multiple IDs' <<<"${OUTPUT}")"
if [[ "${MULTI_MATCH_COUNT}" != "1" ]]; then
  fail "expected each matching commit only once"
fi
FIRST_COMMIT="$(awk '/^Commits:$/ { getline; print; exit }' <<<"${OUTPUT}")"
if [[ "${FIRST_COMMIT}" != *"linked through multiple IDs"* ]]; then
  fail "expected newest matching commit first"
fi
if [[ "${OUTPUT}" != *"T-0001: seed trace fixtures"* ]]; then
  fail "expected the oldest matching commit to be included"
fi

OUTPUT="$(bash tools/trace T-0001)"
if [[ "${OUTPUT}" == *"(git history unavailable)"* ]]; then
  fail "expected git history to be available for T-0001 query"
fi

popd >/dev/null

echo "Trace CLI git integration test passed."
