import assert from "node:assert/strict";
import { test } from "node:test";
import type { ReviewComment, ReviewSubmission } from "./schema.ts";
import { checkGitArgs, invalidAnchors, submitReviewTool } from "./tools.ts";

test("checkGitArgs allows read-only subcommands only", () => {
  for (const ok of [["log", "--oneline", "-10"], ["blame", "-L", "1,5", "a.go"], ["show", "HEAD:go.mod"], ["diff", "abc", "HEAD", "--", "a.go"]]) {
    assert.equal(checkGitArgs(ok), undefined, ok.join(" "));
  }
  for (const bad of [[], ["push"], ["config", "core.pager", "sh"], ["-c", "x=y", "log"], ["diff", "--output=/tmp/x"], ["diff", "--no-index", "/etc/passwd", "a"], ["grep", "-O", "x"], ["log", "--ext-diff"]]) {
    assert.notEqual(checkGitArgs(bad), undefined, bad.join(" "));
  }
});

const commentable = new Map([["src/a.ts", new Set([10, 11])]]);
const comment = (path: string, line: number): ReviewComment => ({ path, line, severity: "high", body: "x" });

test("invalidAnchors reports files and lines outside the diff", () => {
  assert.deepEqual(invalidAnchors([comment("src/a.ts", 10), comment("./src/a.ts", 11), comment("b/src/a.ts", 10)], commentable), []);
  assert.deepEqual(invalidAnchors([comment("src/a.ts", 12), comment("README.md", 1)], commentable), [
    "comments[0]: src/a.ts:12 is not an added or context line of the diff",
    "comments[1]: README.md is not a changed file in this PR",
  ]);
});

test("submit_review rejects bad anchors, then accepts once and terminates", async () => {
  const got: ReviewSubmission[] = [];
  const tool = submitReviewTool(commentable, (s) => got.push(s));
  const call = (params: ReviewSubmission) => tool.execute("id", params, undefined, undefined, undefined as never);

  await assert.rejects(call({ verdict: "approve", body: "ok", comments: [comment("src/a.ts", 99)] }), /src\/a\.ts:99/);
  assert.equal(got.length, 0);

  const result = await call({ verdict: "request_changes", body: "fix", comments: [comment("./src/a.ts", 10)] });
  assert.equal(result.terminate, true);
  assert.deepEqual(got, [{ verdict: "request_changes", body: "fix", comments: [comment("src/a.ts", 10)] }]);

  await assert.rejects(call({ verdict: "approve", body: "again", comments: [] }), /already submitted/);
});
