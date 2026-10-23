// Contract test: go/internal/fetch writes pr.json from the same fixtures.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { buildDiff } from "./diff.ts";
import { parsePRContext } from "./pr.ts";

const DIR = join(import.meta.dirname, "../../contract/pr-context");
const read = (name: string): unknown => JSON.parse(readFileSync(join(DIR, name), "utf8"));

test("contract pr.json parses", () => {
  const pr = parsePRContext(read("pr.json"));
  assert.equal(pr.repo, "octo-org/demo");
  assert.equal(pr.files.find((f) => f.status === "renamed")?.previousPath, "pkg/old_name.go");
});

test("buildDiff produces the contract diff from GitHub's files response", () => {
  const raw = read("github-files.json") as { filename: string; previous_filename?: string; status: string; patch?: string }[];
  const files = raw.map((f) => ({
    path: f.filename,
    status: f.status,
    ...(f.previous_filename ? { previousPath: f.previous_filename } : {}),
    ...(f.patch !== undefined ? { patch: f.patch } : {}),
  }));
  assert.equal(buildDiff(files), parsePRContext(read("pr.json")).diff);
});

test("contract previousComments parse", () => {
  const pr = parsePRContext({ ...(read("pr.json") as object), previousComments: read("previous-comments.json") });
  assert.equal(pr.previousComments?.length, 2);
  assert.equal(pr.previousComments?.[1]?.resolved, true);
  assert.throws(() => parsePRContext({ ...(read("pr.json") as object), previousComments: [{ path: "a" }] }), /invalid PR context/);
});

test("parsePRContext rejects malformed input", () => {
  const pr = read("pr.json") as Record<string, unknown>;
  assert.throws(() => parsePRContext({ ...pr, number: 0 }), /number/);
  assert.throws(() => parsePRContext({ ...pr, files: [{ path: "a" }] }), /invalid PR context/);
  assert.throws(() => parsePRContext(null), /invalid PR context/);
});
