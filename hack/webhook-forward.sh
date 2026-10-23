#!/usr/bin/env bash
# Forwards GitHub App webhooks from a smee.io channel to the gateway in Kind.
# Usage: hack/webhook-forward.sh https://smee.io/<channel>
set -euo pipefail

url="${1:?usage: hack/webhook-forward.sh <smee-url>}"
namespace="${NAMESPACE:-ai-pr-review-cloud}"
port="${PORT:-18080}"

kubectl -n "${namespace}" port-forward svc/ai-pr-review-cloud-gateway "${port}:80" >/dev/null &
forward=$!
trap 'kill "${forward}"' EXIT
sleep 2

npx --yes smee-client@5.0.0 --url "${url}" --target "http://127.0.0.1:${port}/webhook"
