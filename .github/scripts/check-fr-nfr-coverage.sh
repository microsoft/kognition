#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

PROJECT_PATH="PROJECT.md"
BACKLOG_PATH="BACKLOG.md"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --project)
      PROJECT_PATH="$2"
      shift 2
      ;;
    --backlog)
      BACKLOG_PATH="$2"
      shift 2
      ;;
    *)
      echo "Unknown argument: $1"
      echo "usage: $0 [--project PROJECT.md] [--backlog BACKLOG.md]"
      exit 2
      ;;
  esac
done

if [[ ! -f "${PROJECT_PATH}" ]]; then
  echo "Project file not found: ${PROJECT_PATH}"
  exit 2
fi
if [[ ! -f "${BACKLOG_PATH}" ]]; then
  echo "Backlog file not found: ${BACKLOG_PATH}"
  exit 2
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

MVP_REQS_FILE="${TMP_DIR}/mvp_requirements.txt"
US_REFS_FILE="${TMP_DIR}/us_refs.txt"
TC_REFS_FILE="${TMP_DIR}/tc_refs.txt"

awk -F'|' '
/^### 29\.1 Functional Requirements catalog/ { in_fr = 1; next }
in_fr && /^### 29\.2 Non-Functional Requirements catalog/ { in_fr = 0 }
in_fr && $0 ~ /^\| `FR-[0-9]{4}`/ {
  id = $2
  mvp = $4
  gsub(/[`[:space:]]/, "", id)
  # Treat FRs as MVP-scoped when the MVP marker includes either ✅ or 🟡.
  if (index(mvp, "✅") > 0 || index(mvp, "🟡") > 0) {
    print id
  }
}
' "${PROJECT_PATH}" > "${TMP_DIR}/mvp_fr.txt"

awk '
/^### 29\.2 Non-Functional Requirements catalog/ { in_nfr = 1; next }
in_nfr && /^### 29\.3 Constraints/ { in_nfr = 0 }
in_nfr && $0 ~ /^- `NFR-[0-9]{4}`/ {
  if (match($0, /NFR-[0-9]{4}/)) {
    print substr($0, RSTART, RLENGTH)
  }
}
' "${PROJECT_PATH}" > "${TMP_DIR}/mvp_nfr.txt"

cat "${TMP_DIR}/mvp_fr.txt" "${TMP_DIR}/mvp_nfr.txt" | sort -u > "${MVP_REQS_FILE}"

awk '
BEGIN { current_us = "" }
/^#### / {
  current_us = ""
  if (match($0, /US-[0-9]{4}/)) {
    current_us = substr($0, RSTART, RLENGTH)
  }
  next
}
current_us != "" && /\*\*Traces:\*\*/ {
  line = $0
  while (match(line, /(FR|NFR)-[0-9]{4}/)) {
    print substr(line, RSTART, RLENGTH)
    line = substr(line, RSTART + RLENGTH)
  }
}
' "${BACKLOG_PATH}" | sort -u > "${US_REFS_FILE}"

awk '
/^\| TC-[0-9]{4} / {
  line = $0
  while (match(line, /(FR|NFR)-[0-9]{4}/)) {
    print substr(line, RSTART, RLENGTH)
    line = substr(line, RSTART + RLENGTH)
  }
}
' "${BACKLOG_PATH}" | sort -u > "${TC_REFS_FILE}"

MISSING_US=()
MISSING_TC=()

while IFS= read -r req; do
  [[ -z "${req}" ]] && continue
  if ! grep -qx "${req}" "${US_REFS_FILE}"; then
    MISSING_US+=("${req}")
  fi
  if ! grep -qx "${req}" "${TC_REFS_FILE}"; then
    MISSING_TC+=("${req}")
  fi
done < "${MVP_REQS_FILE}"

TOTAL_REQS="$(wc -l < "${MVP_REQS_FILE}" | tr -d ' ')"
COVERED_US=$(( TOTAL_REQS - ${#MISSING_US[@]} ))
COVERED_TC=$(( TOTAL_REQS - ${#MISSING_TC[@]} ))

echo "MVP requirements analyzed: ${TOTAL_REQS}"
echo "US coverage: ${COVERED_US}/${TOTAL_REQS}"
echo "TC coverage: ${COVERED_TC}/${TOTAL_REQS}"

if (( ${#MISSING_US[@]} > 0 )); then
  echo "Missing US coverage:"
  for id in "${MISSING_US[@]}"; do
    echo "  - ${id}"
  done
fi

if (( ${#MISSING_TC[@]} > 0 )); then
  echo "Missing TC coverage:"
  for id in "${MISSING_TC[@]}"; do
    echo "  - ${id}"
  done
fi

if (( ${#MISSING_US[@]} > 0 || ${#MISSING_TC[@]} > 0 )); then
  echo "FR/NFR coverage check failed."
  exit 1
fi

echo "FR/NFR coverage check passed."
