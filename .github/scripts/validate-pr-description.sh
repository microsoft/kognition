#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

PR_BODY="${PR_BODY:-}"

if [[ -z "${PR_BODY}" ]]; then
  echo "PR description is empty. Add at least one user story reference like US-0001."
  exit 1
fi

if [[ ! "${PR_BODY}" =~ US-[0-9]{4} ]]; then
  echo "PR description validation failed."
  echo "Expected at least one user story reference in the PR description (example: US-0001)."
  exit 1
fi

echo "PR description validation passed."
