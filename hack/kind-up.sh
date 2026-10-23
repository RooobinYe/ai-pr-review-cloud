#!/usr/bin/env bash
# Creates the local Kind cluster if it does not exist yet.
set -euo pipefail

cluster="${1:-ai-pr-review-cloud}"
if kind get clusters | grep -qx "${cluster}"; then
  echo "kind cluster ${cluster} already exists"
else
  kind create cluster --name "${cluster}"
fi
