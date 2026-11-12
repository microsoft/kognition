#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TRACE_CLI="${ROOT_DIR}/tools/trace"

fail() {
  echo "trace CLI test failed: $1"
  exit 1
}

if [[ ! -x "${TRACE_CLI}" ]]; then
  fail "expected tools/trace to be executable"
fi

OUTPUT="$(bash "${TRACE_CLI}" FR-0001)"
for expected in US-0001 T-0001 T-0002 T-0003 T-0004 T-0005 TC-0001 TC-0002 TC-0003 TC-0004; do
  if [[ "${OUTPUT}" != *"${expected}"* ]]; then
    fail "expected FR-0001 chain output to include ${expected}"
  fi
done

for expected in "Trace Chain for FR-0001" "US:" "Tasks:" "Test Cases:" "FR:" "NFR:" "Commits:"; do
  if [[ "${OUTPUT}" != *"${expected}"* ]]; then
    fail "expected FR-0001 output to include section '${expected}'"
  fi
done

US_LINE="$(grep -n '^US:$' <<<"${OUTPUT}" | head -n1 | cut -d: -f1)"
TASKS_LINE="$(grep -n '^Tasks:$' <<<"${OUTPUT}" | head -n1 | cut -d: -f1)"
TCS_LINE="$(grep -n '^Test Cases:$' <<<"${OUTPUT}" | head -n1 | cut -d: -f1)"
FR_LINE="$(grep -n '^FR:$' <<<"${OUTPUT}" | head -n1 | cut -d: -f1)"
NFR_LINE="$(grep -n '^NFR:$' <<<"${OUTPUT}" | head -n1 | cut -d: -f1)"
COMMITS_LINE="$(grep -n '^Commits:$' <<<"${OUTPUT}" | head -n1 | cut -d: -f1)"

if [[ -z "${US_LINE}" || -z "${TASKS_LINE}" || -z "${TCS_LINE}" || -z "${FR_LINE}" || -z "${NFR_LINE}" || -z "${COMMITS_LINE}" ]]; then
  fail "expected all trace sections to be present exactly once"
fi
if ! (( US_LINE < TASKS_LINE && TASKS_LINE < TCS_LINE && TCS_LINE < FR_LINE && FR_LINE < NFR_LINE && NFR_LINE < COMMITS_LINE )); then
  fail "expected stable section order in FR-0001 output"
fi

TASKS_BLOCK="$(awk '/^Tasks:$/,/^Test Cases:$/' <<<"${OUTPUT}")"
for expected in "  - T-0001" "  - T-0002" "  - T-0003" "  - T-0004" "  - T-0005"; do
  if [[ "${TASKS_BLOCK}" != *"${expected}"* ]]; then
    fail "expected ordered tasks block to include '${expected}'"
  fi
done

OUTPUT="$(bash "${TRACE_CLI}" T-0001)"
for expected in US-0001 FR-0001 FR-0002 TC-0001 TC-0002 TC-0003 TC-0004; do
  if [[ "${OUTPUT}" != *"${expected}"* ]]; then
    fail "expected T-0001 chain output to include ${expected}"
  fi
done

for expected in "Trace Chain for T-0001" "US:" "Tasks:" "Test Cases:" "FR:" "NFR:" "Commits:"; do
  if [[ "${OUTPUT}" != *"${expected}"* ]]; then
    fail "expected T-0001 output to include section '${expected}'"
  fi
done

OUTPUT="$(bash "${TRACE_CLI}" US-0013)"
for expected in T-0044 T-0047 TC-0035 FR-0250; do
  if [[ "${OUTPUT}" != *"  - ${expected}"* ]]; then
    fail "expected decorated US-0013 heading to preserve ${expected}"
  fi
done

OUTPUT="$(bash "${TRACE_CLI}" US-0040)"
for expected in T-0138 T-0142 TC-0108 TC-0111 FR-0600; do
  if [[ "${OUTPUT}" != *"  - ${expected}"* ]]; then
    fail "expected decorated ID cells to preserve ${expected}"
  fi
done

OUTPUT="$(bash "${TRACE_CLI}" T-0236)"
for expected in US-0007 US-0067 FR-0101 FR-0320; do
  if [[ "${OUTPUT}" != *"  - ${expected}"* ]]; then
    fail "expected historical duplicate task to retain ${expected}"
  fi
done

OUTPUT="$(bash "${TRACE_CLI}" TC-0194)"
for expected in US-0033 US-0067 US-0068 NFR-0006 FR-0001; do
  if [[ "${OUTPUT}" != *"  - ${expected}"* ]]; then
    fail "expected historical duplicate test to retain ${expected}"
  fi
done

if bash "${TRACE_CLI}" US-9999 >/dev/null 2>&1; then
  fail "expected an undefined story to return a nonzero exit"
fi

echo "Trace CLI tests passed."
