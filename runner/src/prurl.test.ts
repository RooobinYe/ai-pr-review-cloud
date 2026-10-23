import assert from "node:assert/strict";
import { test } from "node:test";
import { parsePRUrl } from "./prurl.ts";

test("parses a pull request URL", () => {
  assert.deepEqual(parsePRUrl("https://github.com/octo/demo.js/pull/7"), {
    owner: "octo",
    repo: "demo.js",
    number: 7,
  });
  assert.deepEqual(parsePRUrl(" https://github.com/octo/demo/pull/12/ ").number, 12);
});

test("rejects other URLs", () => {
  for (const url of [
    "https://github.com/octo/demo/issues/7",
    "https://github.com/octo/demo/pull/0",
    "https://gitlab.com/octo/demo/pull/7",
    "https://github.com/octo/demo/pull/7/files",
  ]) {
    assert.throws(() => parsePRUrl(url), /not a GitHub pull request URL/);
  }
});
