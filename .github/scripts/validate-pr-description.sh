#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

PR_BODY="${PR_BODY:-}"

if [[ ! "${PR_BODY}" =~ [^[:space:]] ]]; then
  echo "PR description is empty. Describe the changes and how they were tested."
  exit 1
fi

echo "PR description validation passed."
