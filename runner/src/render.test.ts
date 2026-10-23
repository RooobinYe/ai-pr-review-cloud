import assert from "node:assert/strict";
import { test } from "node:test";
import { formatUsage, renderMarkdown } from "./render.ts";
import type { ReviewResult } from "./schema.ts";

test("renderMarkdown lists comments by severity", () => {
  const r: ReviewResult = {
    schema_version: 3,
    pr: { repo: "octo/demo", number: 7, head_sha: "0123456789abcdef0123456789abcdef01234567" },
    model: "deepseek/deepseek-flash",
    generated_at: "2026-10-23T08:00:00Z",
    verdict: "request_changes",
    body: "Summary.",
    comments: [
      { path: "b.ts", line: 3, severity: "low", confidence: "low", body: "minor" },
      { path: "a.ts", line: 9, severity: "critical", confidence: "high", body: "major" },
    ],
  };
  const md = renderMarkdown(r);
  assert.match(md, /^# AI Review: octo\/demo#7\n\n\*\*❌ Request changes\*\* · `deepseek\/deepseek-flash` · 0123456\n\nSummary\./);
  assert.ok(md.indexOf("`a.ts:9` · critical") < md.indexOf("`b.ts:3` · low"));
});

test("formatUsage", () => {
  assert.equal(
    formatUsage({ input: 16_100, output: 900, cacheRead: 219_500, cacheWrite: 0, cost: 0.01984 }, 70.4),
    "tokens: input 16.1k (cache read 219.5k, cache write 0), output 900 · cost $0.0198 · 70s",
  );
});
