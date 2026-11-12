#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHECKER="${ROOT_DIR}/.github/scripts/check-go-coverage.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

PROFILE="${TMP_DIR}/coverage.out"
cat > "${PROFILE}" <<'EOF'
mode: set
github.com/kognition-ai/kognition/internal/cost/store.go:10.1,11.2 4 0
github.com/kognition-ai/kognition/internal/cost/store.go:10.1,11.2 4 1
github.com/kognition-ai/kognition/internal/cost/store.go:12.1,13.2 2 0
github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1/cost.pb.go:10.1,20.2 100 0
github.com/kognition-ai/kognition/api/k8s/v1alpha1/zz_generated.deepcopy.go:10.1,20.2 50 0
EOF

output="$(bash "${CHECKER}" --profile "${PROFILE}" --min 66.6)"
if [[ "${output}" != *"66.7%"* ]]; then
  echo "Go coverage checker test failed: expected 66.7%, got: ${output}"
  exit 1
fi

set +e
bash "${CHECKER}" --profile "${PROFILE}" --min 66.8 >/dev/null 2>&1
status=$?
set -e
if [[ ${status} -eq 0 ]]; then
  echo "Go coverage checker test failed: expected minimum coverage failure"
  exit 1
fi

echo "Go coverage checker tests passed."