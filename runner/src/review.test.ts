// Crash recovery of the review pipeline (docs/architecture.md §4.3), driven by
// pi's scripted faux model instead of a real provider.
import assert from "node:assert/strict";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { createFauxCore, fauxAssistantMessage, fauxToolCall } from "@earendil-works/pi-ai";
import { ModelRuntime, parseSessionEntries } from "@earendil-works/pi-coding-agent";
import type { PRContext } from "./pr.ts";
import { RESUME_PROMPT } from "./prompts.ts";
import type { SessionStats } from "@earendil-works/pi-coding-agent";
import { BudgetExceededError, ModelError, modelError, acceptedSubmission, ModelRequestError, review, type TurnSnapshot } from "./review.ts";
import { commentableLines } from "./diff.ts";

const stats = {} as SessionStats;

test("modelError recognizes LiteLLM's budget rejection", () => {
  // As pi records it for LiteLLM v1.104.
  const raw =
    '422: {"message":"Budget has been exceeded! Key=review-x-1 (sk-...xDtA) Current cost: 0.08399999999999999, Max budget: 0.05","type":"budget_exceeded","param":null,"code":"422"}';
  const err = modelError(raw, stats);
  assert.ok(err instanceof BudgetExceededError);
  assert.equal(err.message, "budget_exceeded: the model proxy refused further requests; the run spent $0.084 of its $0.05 budget");
  assert.equal(modelError('{"type":"budget_exceeded"}', stats).message, "budget_exceeded: the model proxy refused further requests; the run spent its budget");
});

test("modelError keeps other failures generic", () => {
  const err = modelError("401: Authentication Error, Invalid proxy server token passed.", stats);
  assert.ok(err instanceof ModelError && !(err instanceof BudgetExceededError));
  assert.match(err.message, /^model request failed: 401: Authentication Error/);
});


const pr: PRContext = {
  repo: "o/r",
  number: 1,
  title: "t",
  author: "a",
  body: "",
  baseRef: "main",
  headRef: "f",
  baseSha: "1".repeat(40),
  headSha: "2".repeat(40),
  files: [{ path: "a.go", status: "modified", additions: 1, deletions: 1 }],
  diff: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new",
};

const submission = {
  verdict: "request_changes" as const,
  body: "Found a bug.",
  comments: [{ path: "a.go", line: 1, severity: "high" as const, confidence: "high" as const, body: "`new` is wrong" }],
};

/** A runtime whose only model answers with the scripted responses. */
async function fauxModel(dir: string) {
  const faux = createFauxCore({ provider: "faux", models: [{ id: "faux-1" }] });
  const modelRuntime = await ModelRuntime.create({ authPath: join(dir, "auth.json"), modelsPath: null, refreshOnCreate: false });
  modelRuntime.registerProvider("faux", {
    api: faux.api,
    baseUrl: "http://faux.invalid",
    streamSimple: faux.streamSimple,
    models: [{ id: "faux-1", name: "faux-1", input: ["text"], reasoning: false, contextWindow: 128_000, maxTokens: 16_384, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
  });
  await modelRuntime.setRuntimeApiKey("faux", "k");
  return { faux, modelRuntime, model: modelRuntime.getModel("faux", "faux-1")! };
}

async function run(resumeFrom: ReturnType<typeof parseSessionEntries> | undefined, responses: Parameters<ReturnType<typeof createFauxCore>["setResponses"]>[0], budgetUSD?: number) {
  const dir = await mkdtemp(join(tmpdir(), "review-test-"));
  await writeFile(join(dir, "a.go"), "new\n");
  const { faux, modelRuntime, model } = await fauxModel(dir);
  faux.setResponses(responses);
  const snapshots: TurnSnapshot[] = [];
  const outcome = await review({
    pr,
    cwd: dir,
    agentDir: dir,
    model,
    modelRuntime,
    thinkingLevel: "off",
    language: "English",
    resumeFrom,
    budgetUSD,
    onTurn: async (s) => {
      snapshots.push(s);
    },
  });
  return { outcome, snapshots, faux };
}

test("a crashed review resumes from its last snapshot", async () => {
  // First attempt: one exploring turn, then the process dies mid-way.
  const first = await run(undefined, [
    fauxAssistantMessage(fauxToolCall("read", { path: "a.go" })),
    fauxAssistantMessage(fauxToolCall("submit_review", submission)),
  ]);
  assert.deepEqual(first.snapshots.map((s) => s.turn), [1, 2]);
  assert.equal(first.snapshots[0]!.model, "faux/faux-1");
  const afterExploring = parseSessionEntries(first.snapshots[0]!.jsonl);

  let resumed: { role: string; content: unknown }[] = [];
  const second = await run(afterExploring, [
    (context) => {
      // pi records an empty system entry before each prompt; skip those.
      resumed = (context.messages as typeof resumed).filter((m) => m.role !== "system");
      return fauxAssistantMessage(fauxToolCall("submit_review", submission));
    },
  ]);
  assert.deepEqual(second.outcome.submission, submission);
  assert.equal(second.faux.state.callCount, 1);
  // The model sees the first attempt's conversation, then the resume note.
  assert.deepEqual(resumed.map((m) => m.role), ["user", "assistant", "toolResult", "user"]);
  assert.match(JSON.stringify(resumed[0]!.content), /<review_request>/);
  assert.match(JSON.stringify(resumed[2]!.content), /new/);
  assert.equal(JSON.stringify(resumed[3]!.content).includes(RESUME_PROMPT), true);
  assert.deepEqual(second.snapshots.map((s) => s.turn), [2], "turns are numbered across attempts");
});

test("a review submitted before the crash is reported without calling the model", async () => {
  const first = await run(undefined, [fauxAssistantMessage(fauxToolCall("submit_review", {
    ...submission,
    comments: [{ ...submission.comments[0]!, path: "./b/a.go" }],
  }))]);
  const submitted = parseSessionEntries(first.snapshots.at(-1)!.jsonl);
  const second = await run(submitted, []);
  assert.deepEqual(second.outcome.submission, submission);
  assert.equal(second.faux.state.callCount, 0);
});

test("budget reminders include spending from the restored session", async () => {
  const exploring = fauxAssistantMessage(fauxToolCall("read", { path: "a.go" }));
  const first = await run(undefined, [exploring, fauxAssistantMessage(fauxToolCall("submit_review", submission))]);
  const saved = parseSessionEntries(first.snapshots[0]!.jsonl);
  for (const entry of saved) {
    if (entry.type === "message" && entry.message.role === "assistant") entry.message.usage.cost.total = 0.9;
  }
  const second = await run(saved, [
    fauxAssistantMessage(fauxToolCall("read", { path: "a.go" })),
    fauxAssistantMessage(fauxToolCall("submit_review", submission)),
  ], 1);
  assert.equal(second.outcome.nudged, true);
});

test("acceptedSubmission ignores rejected calls", async () => {
  const first = await run(undefined, [
    fauxAssistantMessage(fauxToolCall("submit_review", { ...submission, comments: [{ ...submission.comments[0]!, line: 99 }] })),
    fauxAssistantMessage(fauxToolCall("read", { path: "a.go" })),
    fauxAssistantMessage(fauxToolCall("submit_review", submission)),
  ]);
  const commentable = commentableLines(pr.diff);
  assert.equal(acceptedSubmission(parseSessionEntries(first.snapshots[1]!.jsonl), commentable), undefined, "rejected anchor counted as submitted");
  assert.deepEqual(acceptedSubmission(parseSessionEntries(first.snapshots.at(-1)!.jsonl), commentable), submission);
});

test("a proxy budget rejection survives the recovery pipeline", async () => {
  const failed = fauxAssistantMessage("", { stopReason: "error", errorMessage: '{"type":"budget_exceeded"}' });
  await assert.rejects(run(undefined, [failed, failed, failed]), BudgetExceededError);
});

test("a failed model request is retryable, not a missing submission", async () => {
  const failed = fauxAssistantMessage("", { stopReason: "error", errorMessage: "402 Insufficient Balance" });
  await assert.rejects(run(undefined, [fauxAssistantMessage(fauxToolCall("read", { path: "a.go" })), failed, failed, failed]), (err: Error) => {
    assert.ok(err instanceof ModelRequestError, `got ${err.name}: ${err.message}`);
    assert.match(err.message, /Insufficient Balance/);
    return true;
  });
});
