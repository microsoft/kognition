#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

BACKLOG_PATH="BACKLOG.md"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --backlog)
      BACKLOG_PATH="$2"
      shift 2
      ;;
    *)
      echo "Unknown argument: $1"
      echo "usage: $0 [--backlog BACKLOG.md]"
      exit 2
      ;;
  esac
done

if [[ ! -f "${BACKLOG_PATH}" ]]; then
  echo "Backlog file not found: ${BACKLOG_PATH}"
  exit 2
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

TC_IDS_FILE="${TMP_DIR}/tc_ids.txt"
TC_NFR_PAIRS_FILE="${TMP_DIR}/tc_nfr_pairs.txt"
NFR_PLAN_FILE="${TMP_DIR}/nfr_plan_pairs.txt"

awk -F'|' '
/^[|] TC-[0-9]{4} / {
  if (match($2, /TC-[0-9]{4}/)) {
    print substr($2, RSTART, RLENGTH)
  }
}
' "${BACKLOG_PATH}" | sort -u > "${TC_IDS_FILE}"

awk -F'|' '
/^[|] TC-[0-9]{4} / {
  if (!match($2, /TC-[0-9]{4}/)) {
    next
  }
  tc = substr($2, RSTART, RLENGTH)
  # Parse only the Traces column (4th field in TC rows), not the description.
  traces = $4
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", traces)
  while (match(traces, /NFR-[0-9]{4}/)) {
    nfr = substr(traces, RSTART, RLENGTH)
    print tc "|" nfr
    traces = substr(traces, RSTART + RLENGTH)
  }
}
' "${BACKLOG_PATH}" | sort -u > "${TC_NFR_PAIRS_FILE}"

awk -F'|' '
/^## 5\. NFR Verification Plan/ { in_nfr_plan = 1; next }
in_nfr_plan && /^## / { in_nfr_plan = 0 }
in_nfr_plan && $0 ~ /^\| NFR-[0-9]{4} / {
  nfr = $2
  related = $5
  gsub(/[[:space:]]/, "", nfr)
  gsub(/^[[:space:]]+|[[:space:]]+$/, "", related)

  if (index(related, "..") > 0) {
    print "RANGE|" nfr "|" related
    next
  }

  line = related
  found = 0
  while (match(line, /TC-[0-9]{4}/)) {
    tc = substr(line, RSTART, RLENGTH)
    print "PAIR|" nfr "|" tc
    line = substr(line, RSTART + RLENGTH)
    found = 1
  }

  if (found == 0) {
    print "MISSING|" nfr "|"
  }
}
' "${BACKLOG_PATH}" > "${NFR_PLAN_FILE}"

RANGE_ERRORS=()
MISSING_MAPPING_ERRORS=()
UNKNOWN_TC_ERRORS=()
MISALIGNED_ERRORS=()

while IFS='|' read -r kind nfr tc; do
  [[ -z "${kind}" ]] && continue

  if [[ "${kind}" == "RANGE" ]]; then
    RANGE_ERRORS+=("${nfr} uses unsupported TC range '${tc}'. Use explicit TC IDs.")
    continue
  fi

  if [[ "${kind}" == "MISSING" ]]; then
    MISSING_MAPPING_ERRORS+=("${nfr} has no Related TCs entry.")
    continue
  fi

  if [[ "${kind}" == "PAIR" ]]; then
    if ! grep -qx "${tc}" "${TC_IDS_FILE}"; then
      UNKNOWN_TC_ERRORS+=("${nfr} references unknown test case ${tc} in NFR plan.")
      continue
    fi

    if ! grep -qx "${tc}|${nfr}" "${TC_NFR_PAIRS_FILE}"; then
      MISALIGNED_ERRORS+=("${nfr} maps to ${tc}, but ${tc} does not trace ${nfr} in its Traces column.")
    fi
  fi
done < "${NFR_PLAN_FILE}"

NFR_ROW_COUNT="$(grep -Ec '^\| NFR-[0-9]{4} ' "${BACKLOG_PATH}")"
PAIR_COUNT="$(grep -Ec '^PAIR\|' "${NFR_PLAN_FILE}" || true)"

echo "NFR plan rows analyzed: ${NFR_ROW_COUNT}"
echo "NFR->TC mappings analyzed: ${PAIR_COUNT}"

if (( ${#RANGE_ERRORS[@]} > 0 )); then
  echo "Unsupported TC range usage in NFR plan:"
  for e in "${RANGE_ERRORS[@]}"; do
    echo "  - ${e}"
  done
fi

if (( ${#MISSING_MAPPING_ERRORS[@]} > 0 )); then
  echo "Missing NFR mappings:"
  for e in "${MISSING_MAPPING_ERRORS[@]}"; do
    echo "  - ${e}"
  done
fi

if (( ${#UNKNOWN_TC_ERRORS[@]} > 0 )); then
  echo "Unknown TCs referenced by NFR plan:"
  for e in "${UNKNOWN_TC_ERRORS[@]}"; do
    echo "  - ${e}"
  done
fi

if (( ${#MISALIGNED_ERRORS[@]} > 0 )); then
  echo "NFR/TC trace misalignment:"
  for e in "${MISALIGNED_ERRORS[@]}"; do
    echo "  - ${e}"
  done
fi

if (( ${#RANGE_ERRORS[@]} > 0 || ${#MISSING_MAPPING_ERRORS[@]} > 0 || ${#UNKNOWN_TC_ERRORS[@]} > 0 || ${#MISALIGNED_ERRORS[@]} > 0 )); then
  echo "NFR verification-plan semantic check failed."
  exit 1
fi

echo "NFR verification-plan semantic check passed."
