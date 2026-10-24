import assert from "node:assert/strict";
import { test } from "node:test";
import { parseLine } from "./console.mjs";

test("parseLine", () => {
  const cases = [
    ["[00:04] PR #17 marked ready for review: https://github.com/o/r/pull/17", { t: 4, type: "ready", pr: 17, url: "https://github.com/o/r/pull/17" }],
    ["[00:07] run e52f39f2 running (attempt 1)", { t: 7, type: "state", run: "e52f39f2", state: "running", attempt: 1 }],
    ["[01:56] run e52f39f2 awaiting_approval (attempt 2)", { t: 116, type: "state", run: "e52f39f2", state: "awaiting_approval", attempt: 2 }],
    ["[00:07] runner Job review-e52f39f2-1", { t: 7, type: "job", job: "review-e52f39f2-1" }],
    ["[00:33] chaos: deleted the runner Pod of attempt 1", { t: 33, type: "chaos", attempt: 1 }],
    [
      "[00:48] preview pr-o-r-17: preview-demo-5b4c8b847-sxzjj ready=false restarts=0",
      { t: 48, type: "pod", namespace: "pr-o-r-17", pod: "preview-demo-5b4c8b847-sxzjj", ready: false, restarts: 0 },
    ],
    ["[00:50] preview pr-o-r-17: preview-demo-1 ready=<none> restarts=<none>", { t: 50, type: "pod", namespace: "pr-o-r-17", pod: "preview-demo-1", ready: false, restarts: 0 }],
    ["[01:00] no run after a minute: is the webhook forwarder (make webhook-forward) running?", { t: 60, type: "warn", text: "no run after a minute: is the webhook forwarder (make webhook-forward) running?" }],
    ["[00:15]   agent    read demo/preview/server.mjs", { t: 15, type: "tool", tool: "read", text: "demo/preview/server.mjs" }],
    ["[00:16]   agent    grep healthz|/version", { t: 16, type: "tool", tool: "grep", text: "healthz|/version" }],
    ["[01:33]   agent    kubectl get pods --output=wide", { t: 93, type: "tool", tool: "kubectl", text: "get pods --output=wide" }],
    ["[01:51]   agent    submit_review", { t: 111, type: "tool", tool: "submit_review", text: "" }],
    [
      "[01:37]   agent      kubectl failed: only bounded read-only preview kubectl commands are allowed",
      { t: 97, type: "toolFailed", tool: "kubectl", text: "only bounded read-only preview kubectl commands are allowed" },
    ],
    ["[00:33]   agent  terminated", { t: 33, type: "terminated" }],
    ["[00:42]   agent  run e52f39f2-0274: resuming from a snapshot with 22 entries", { t: 42, type: "snapshot", entries: 22 }],
    ["[00:42]   agent    resuming after turn 5 of an earlier attempt", { t: 42, type: "resumed", turn: 5 }],
    ["[00:46]   agent  deploying preview; waiting for Ready or timeout", { t: 46, type: "deploying" }],
    [
      "[01:32]   agent  preview pr-o-r-17: timeout: inspect pods, events and logs to determine the cause",
      { t: 92, type: "previewStatus", namespace: "pr-o-r-17", status: "timeout: inspect pods, events and logs to determine the cause" },
    ],
    [
      "[00:14]   agent  reviewing o/r#17 at 54b4e29 with deepseek/deepseek-flash (thinking high) budget $0.5",
      { t: 14, type: "reviewing", model: "deepseek/deepseek-flash", text: "reviewing o/r#17 at 54b4e29 with deepseek/deepseek-flash (thinking high) budget $0.5" },
    ],
    ["[01:51]   agent  run e52f39f2-0274: posted request_changes with 1 comments", { t: 111, type: "posted", verdict: "request_changes", comments: 1 }],
    ["[00:14]   agent    model stopped without submitting; reminding it", { t: 14, type: "agentLog", text: "model stopped without submitting; reminding it" }],
    ["[00:14]   agent  run e52f39f2: 1 files changed, 0 earlier comments", { t: 14, type: "agentLog", text: "run e52f39f2: 1 files changed, 0 earlier comments" }],
    ["[00:01] something new", { t: 1, type: "log", text: "something new" }],
    ["", null],
    [
      "verdict request_changes, attempts 2, retried after: runner job failed: BackoffLimitExceeded",
      { type: "verdict", verdict: "request_changes", attempts: 2, error: "runner job failed: BackoffLimitExceeded" },
    ],
    ["verdict approve, attempts 1", { type: "verdict", verdict: "approve", attempts: 1, error: "" }],
    [
      "phases  queue 1s · startup 30s · fetch 5s · review 71s · publish 2s · total 109s",
      { type: "phases", phases: { queue: 1, startup: 30, fetch: 5, review: 71, publish: 2, total: 109 } },
    ],
    ["cost    $0.0139 over 10 turns · input 22959 · cache read 84864 · output 5460 tokens", { type: "cost", cost: 0.0139, turns: 10 }],
    ["comment demo/preview/server.mjs:6 [high] The route table drops a path", { type: "comment", path: "demo/preview/server.mjs", line: "6", severity: "high", text: "The route table drops a path" }],
    ["review  https://github.com/o/r/pull/17#pullrequestreview-1", { type: "review", url: "https://github.com/o/r/pull/17#pullrequestreview-1" }],
  ];
  for (const [line, want] of cases) assert.deepEqual(parseLine(line), want, line);
});
