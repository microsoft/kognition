#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

PROFILE="coverage.out"
MINIMUM="80"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)
      PROFILE="$2"
      shift 2
      ;;
    --min)
      MINIMUM="$2"
      shift 2
      ;;
    *)
      echo "Unknown argument: $1"
      echo "usage: $0 [--profile COVERAGE_PROFILE] [--min PERCENT]"
      exit 2
      ;;
  esac
done

if [[ ! -f "${PROFILE}" ]]; then
  echo "Coverage profile not found: ${PROFILE}"
  exit 2
fi

coverage="$(awk '
  NR == 1 && $0 ~ /^mode: / { next }
  {
    split($1, location, ":")
    file = location[1]
    if (file ~ /(^|\/)proto\/gen\// || file ~ /\/zz_generated[^\/]*\.go$/) {
      next
    }
    key = $1 " " $2
    if (!(key in statements)) {
      statements[key] = $2
      covered[key] = ($3 > 0)
    } else if ($3 > 0) {
      covered[key] = 1
    }
  }
  END {
    for (key in statements) {
      total += statements[key]
      if (covered[key]) {
        hit += statements[key]
      }
    }
    if (total == 0) {
      exit 2
    }
    printf "%.1f", (100 * hit / total)
  }
' "${PROFILE}")"

if [[ -z "${coverage}" ]]; then
  echo "Coverage profile has no authored Go statements: ${PROFILE}"
  exit 2
fi

echo "Authored Go statement coverage: ${coverage}% (minimum: ${MINIMUM}%)"
if ! awk -v actual="${coverage}" -v minimum="${MINIMUM}" 'BEGIN { exit !(actual + 0 >= minimum + 0) }'; then
  echo "Authored Go coverage is below the required minimum."
  exit 1
fi