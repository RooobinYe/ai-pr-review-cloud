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
import { acceptedSubmission, review, type TurnSnapshot } from "./review.ts";
import { commentableLines } from "./diff.ts";

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

async function run(resumeFrom: ReturnType<typeof parseSessionEntries> | undefined, responses: Parameters<ReturnType<typeof createFauxCore>["setResponses"]>[0]) {
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
  const first = await run(undefined, [fauxAssistantMessage(fauxToolCall("submit_review", submission))]);
  const submitted = parseSessionEntries(first.snapshots.at(-1)!.jsonl);
  const second = await run(submitted, []);
  assert.deepEqual(second.outcome.submission, submission);
  assert.equal(second.faux.state.callCount, 0);
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
