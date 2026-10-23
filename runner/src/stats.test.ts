import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import type { SessionStats } from "@earendil-works/pi-coding-agent";
import { parsePRContext } from "./pr.ts";
import type { ReviewResult } from "./schema.ts";
import { buildRunStats } from "./stats.ts";

const contract = join(import.meta.dirname, "../../contract");
const PR = parsePRContext(JSON.parse(readFileSync(join(contract, "pr-context/pr.json"), "utf8")));
const RESULT = JSON.parse(readFileSync(join(contract, "examples/valid-request-changes.json"), "utf8")) as ReviewResult;
const SESSION = {
  assistantMessages: 7,
  toolCalls: 12,
  tokens: { input: 9000, output: 2100, cacheRead: 98000, cacheWrite: 0, total: 109100 },
  cost: 0.0118,
} as SessionStats;
const SECONDS = { fetch: 0.8, clone: 2.1, review: 64.2, total: 67.3 };

test("buildRunStats summarizes a submitted review", () => {
  const stats = buildRunStats({ pr: PR, model: "deepseek/deepseek-flash", thinking: "high", seconds: SECONDS, session: SESSION, nudged: false, result: RESULT });
  assert.equal(stats.pr, `${PR.repo}#${PR.number}`);
  assert.equal(stats.changed_lines, PR.files.reduce((n, f) => n + f.additions + f.deletions, 0));
  assert.equal(stats.turns, 7);
  assert.equal(stats.tool_calls, 12);
  assert.deepEqual(stats.tokens, { input: 9000, output: 2100, cache_read: 98000, cache_write: 0 });
  assert.equal(stats.cost_usd, 0.0118);
  assert.equal(stats.verdict, RESULT.verdict);
  assert.equal(Object.values(stats.comments).reduce((a, b) => a + b, 0), RESULT.comments.length);
  assert.equal(stats.budget_usd, null);
  assert.equal(stats.error, null);
});

test("buildRunStats records a failed review with what it spent", () => {
  const stats = buildRunStats({ pr: PR, model: "deepseek/deepseek-flash", thinking: "low", budgetUSD: 0.05, seconds: SECONDS, session: SESSION, error: "model did not call submit_review" });
  assert.equal(stats.verdict, null);
  assert.deepEqual(stats.comments, { critical: 0, high: 0, medium: 0, low: 0 });
  assert.equal(stats.cost_usd, 0.0118);
  assert.equal(stats.budget_usd, 0.05);
  assert.equal(stats.error, "model did not call submit_review");
});
