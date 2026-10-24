#!/usr/bin/env bash
# Three-minute live demo against the local Kind deployment (docs/demo.md).
#
#   demo/demo.sh prep           open a draft PR with a planted bug and wait for its preview image
#   demo/demo.sh run <pr>       mark the PR ready for review and follow the run until it ends
#   demo/demo.sh cleanup <pr>   close the PR and wait for its preview namespace to be deleted
#
# KILL_AFTER=<seconds> makes `run` delete the runner Pod that many seconds into
# the review, to show recovery from the session snapshot. BASE picks the PR's
# target branch (default main). Reads run state from the dev PostgreSQL.
set -euo pipefail

namespace="${NAMESPACE:-ai-pr-review-cloud}"
base="${BASE:-main}"
repo="$(gh repo view --json nameWithOwner -q .nameWithOwner)"
started="$(date +%s)"

say() {
  local t=$(($(date +%s) - started))
  printf '[%02d:%02d] %s\n' $((t / 60)) $((t % 60)) "$*"
}

sql() {
  kubectl -n "${namespace}" exec deploy/ai-pr-review-cloud-postgres -- psql -U postgres -tA -F '|' -c "$1"
}

preview_namespace() {
  local name
  name="pr-$(printf '%s' "${repo}" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g')-$1"
  # Names over 63 characters are hashed by the controller; this repo's are short.
  printf '%s' "${name}"
}

# The demo PR's version of demo/preview/server.mjs: a routing refactor with a
# bug planted in it. It is encoded because the reviewing agent reads the whole
# repository, this script included, and must not find the answer here.
write_buggy_server() {
  base64 -d >"$1/demo/preview/server.mjs" <<'EOF'
aW1wb3J0IHsgY3JlYXRlU2VydmVyIH0gZnJvbSAnbm9kZTpodHRwJzsKCi8vIFBhdGhzIGFuc3dl
cmVkIHdpdGggMjAwOyBldmVyeXRoaW5nIGVsc2UgaXMgNDA0Lgpjb25zdCByb3V0ZXMgPSBuZXcg
TWFwKFsKICBbJy8nLCAncHJldmlldyBpcyBoZWFsdGh5XG4nXSwKICBbJy9oZWFsdGgnLCAncHJl
dmlldyBpcyBoZWFsdGh5XG4nXSwKICBbJy92ZXJzaW9uJywgYCR7cHJvY2Vzcy5lbnYuQVBQX1ZF
UlNJT04gPz8gJ2Rldid9XG5gXSwKXSk7CgpjcmVhdGVTZXJ2ZXIoKHJlcSwgcmVzKSA9PiB7CiAg
Y29uc29sZS5sb2coYCR7cmVxLm1ldGhvZH0gJHtyZXEudXJsfWApOwogIGNvbnN0IGJvZHkgPSBy
b3V0ZXMuZ2V0KHJlcS51cmwpOwogIHJlcy53cml0ZUhlYWQoYm9keSA/IDIwMCA6IDQwNCwgeyAn
Q29udGVudC1UeXBlJzogJ3RleHQvcGxhaW4nIH0pOwogIHJlcy5lbmQoYm9keSA/PyAnbm90IGZv
dW5kXG4nKTsKfSkubGlzdGVuKDgwODAsICcwLjAuMC4wJyk7Cg==
EOF
}

prep() {
  local branch dir pr id
  branch="demo/version-endpoint-$(date +%m%d%H%M%S)"
  dir="$(mktemp -d)"
  git fetch -q origin "${base}"
  git worktree add -q -b "${branch}" "${dir}" "origin/${base}"
  write_buggy_server "${dir}"
  git -C "${dir}" commit -qam "feat: add /version endpoint to the preview app"
  git -C "${dir}" push -q -u origin "${branch}" 2>/dev/null
  git worktree remove --force "${dir}"
  git branch -q -D "${branch}"

  # A draft is not reviewed (architecture §6.4), but its CI still publishes the
  # preview image, so `run` starts with the image already in the registry.
  gh pr create --draft --base "${base}" --head "${branch}" \
    --title "feat: add /version endpoint to the preview app" \
    --body "Adds a \`/version\` endpoint and moves the routes into a table." >/dev/null
  pr="$(gh pr view "${branch}" --json number -q .number)"
  say "draft PR #${pr} opened: $(gh pr view "${pr}" --json url -q .url)"

  until id="$(gh run list --workflow preview-image.yaml --branch "${branch}" --json databaseId -q '.[0].databaseId')" && [ -n "${id}" ]; do
    sleep 3
  done
  say "waiting for the preview image (workflow run ${id})"
  gh run watch "${id}" --exit-status >/dev/null
  say "preview image published; start the demo with: demo/demo.sh run ${pr}"
}

follower=""
stop_follower() {
  if [ -n "${follower}" ]; then
    kill "${follower}" 2>/dev/null || true
    follower=""
  fi
}

# Streams the agent's progress lines (tool calls, preview status) of one Job.
follow() {
  local job="$1"
  (
    until kubectl -n "${namespace}" logs -f "job/${job}" -c runner 2>/dev/null; do sleep 1; done |
      while IFS= read -r line; do say "  agent  ${line#runner: }"; done
  ) &
  follower=$!
}

run() {
  local pr="$1" head ns row id="" state="" attempt="" job="" verdict=""
  local last="" last_job="" pods="" last_pods="" running_since="" killed=""
  head="$(gh pr view "${pr}" --json headRefOid -q .headRefOid)"
  ns="$(preview_namespace "${pr}")"
  trap 'stop_follower' EXIT

  gh pr ready "${pr}" >/dev/null 2>&1
  say "PR #${pr} marked ready for review: $(gh pr view "${pr}" --json url -q .url)"

  while :; do
    row="$(sql "select id, state, attempt, coalesce(job_name, ''), coalesce(verdict, '') from review_run
      where repo = '${repo}' and pr_number = ${pr} and head_sha = '${head}'" || true)"
    if [ -n "${row}" ]; then
      IFS='|' read -r id state attempt job verdict <<<"${row}"
      if [ "${state}|${attempt}" != "${last}" ]; then
        say "run ${id:0:8} ${state} (attempt ${attempt})"
        last="${state}|${attempt}"
      fi
      if [ -n "${job}" ] && [ "${job}" != "${last_job}" ]; then
        stop_follower
        say "runner Job ${job}"
        follow "${job}"
        last_job="${job}"
        running_since="$(date +%s)"
      fi
    fi

    pods="$(kubectl -n "${ns}" get pods --no-headers \
      -o 'custom-columns=NAME:.metadata.name,READY:.status.containerStatuses[0].ready,RESTARTS:.status.containerStatuses[0].restartCount' 2>/dev/null || true)"
    if [ -n "${pods}" ] && [ "${pods}" != "${last_pods}" ]; then
      say "preview ${ns}: $(printf '%s' "${pods}" | awk '{printf "%s ready=%s restarts=%s", $1, $2, $3}')"
      last_pods="${pods}"
    fi

    if [ -n "${KILL_AFTER:-}" ] && [ -z "${killed}" ] && [ "${state}" = running ] && [ -n "${running_since}" ] &&
      [ $(($(date +%s) - running_since)) -ge "${KILL_AFTER}" ]; then
      kubectl -n "${namespace}" delete pod -l "ai-pr-review.dev/run-id=${id}" --wait=false >/dev/null
      say "chaos: deleted the runner Pod of attempt ${attempt}"
      killed=1
    fi

    case "${state}" in done | failed | superseded | awaiting_approval) break ;; esac
    sleep 2
  done
  sleep 1
  stop_follower
  summary "${id}" "${pr}"
}

summary() {
  local id="$1" pr="$2" review
  echo
  sql "select 'verdict ' || coalesce(verdict, '-') || ', attempts ' || attempt
         || coalesce(', error: ' || error, '') from review_run where id = '${id}'"
  sql "select format('phases  queue %ss · startup %ss · fetch %ss · review %ss · publish %ss · total %ss',
           round(extract(epoch from started_at - created_at)), round(extract(epoch from fetch_started_at - started_at)),
           round(extract(epoch from fetch_finished_at - fetch_started_at)), round(extract(epoch from reviewed_at - fetch_finished_at)),
           round(extract(epoch from finished_at - reviewed_at)), round(extract(epoch from finished_at - created_at)))
         from review_run where id = '${id}'"
  sql "select format('cost    \$%s over %s turns · input %s · cache read %s · output %s tokens',
           round(sum(cost_usd), 4), count(*), sum(input_tokens), sum(cache_read_tokens), sum(output_tokens))
         from run_usage where run_id = '${id}'"
  sql "select format('comment %s:%s [%s] %s', c->>'path', c->>'line', c->>'severity', left(split_part(c->>'body', E'\n', 1), 110))
         from review_run, jsonb_array_elements(result->'comments') c where id = '${id}'"
  review="$(sql "select external_id from github_action where run_id = '${id}' and kind = 'review'")"
  [ -n "${review}" ] && echo "review  https://github.com/${repo}/pull/${pr}#pullrequestreview-${review}"
  return 0
}

cleanup() {
  local pr="$1" ns
  ns="$(preview_namespace "${pr}")"
  gh pr close "${pr}" --delete-branch >/dev/null 2>&1
  say "PR #${pr} closed; the controller deletes ${ns} within a minute"
  while kubectl get namespace "${ns}" >/dev/null 2>&1; do sleep 3; done
  say "preview namespace ${ns} deleted"
}

case "${1:-}" in
  prep) prep ;;
  run) run "${2:?usage: demo/demo.sh run <pr>}" ;;
  cleanup) cleanup "${2:?usage: demo/demo.sh cleanup <pr>}" ;;
  *)
    sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
