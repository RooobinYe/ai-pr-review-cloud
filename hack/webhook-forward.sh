#!/usr/bin/env bash
# Forwards GitHub App webhooks from a smee.io channel to the gateway in Kind.
# Usage: hack/webhook-forward.sh https://smee.io/<channel>
set -euo pipefail

url="${1:?usage: hack/webhook-forward.sh <smee-url>}"
namespace="${NAMESPACE:-ai-pr-review-cloud}"
port="${PORT:-18080}"

# port-forward pins one Pod and exits when it goes away (e.g. on redeploy);
# keep reconnecting to the Service.
while true; do
  kubectl -n "${namespace}" port-forward svc/ai-pr-review-cloud-gateway "${port}:80" >/dev/null 2>&1 || true
  sleep 1
done &
trap 'kill 0' EXIT
until curl -fsS -o /dev/null "http://127.0.0.1:${port}/healthz" 2>/dev/null; do sleep 1; done

npx --yes smee-client@5.0.0 --url "${url}" --target "http://127.0.0.1:${port}/webhook"
