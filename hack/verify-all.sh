#!/usr/bin/env bash

# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o nounset
set -o pipefail

REPO_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
cd "${REPO_ROOT}"

ret=0
failures=()

run_check() {
  local name="$1"
  local script="$2"
  echo "=== Running ${name} ==="
  if ! "${script}"; then
    failures+=("${name}")
    ret=1
  fi
  echo ""
}

run_check "verify-gofmt" "hack/verify-gofmt.sh"
run_check "verify-go-mod" "hack/verify-go-mod.sh"

if [ "${ret}" -ne 0 ]; then
  echo "=== Verification Failed ==="
  echo "The following checks failed:"
  for f in "${failures[@]}"; do
    echo "  - ${f}"
  done
  exit 1
else
  echo "=== All checks passed ==="
fi
