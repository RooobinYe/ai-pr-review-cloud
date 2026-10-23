// Contract test: contract/review-result.schema.json is generated from
// ReviewResultSchema, and go/internal/review checks the same files.
// Regenerate with UPDATE_CONTRACT=1 npm test.
import assert from "node:assert/strict";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { Value } from "typebox/value";
import { ReviewResultSchema } from "./schema.ts";

const CONTRACT = join(import.meta.dirname, "../../contract");
const SCHEMA_FILE = join(CONTRACT, "review-result.schema.json");

test("committed JSON schema matches ReviewResultSchema", () => {
  const generated = `${JSON.stringify(ReviewResultSchema, null, 2)}\n`;
  if (process.env.UPDATE_CONTRACT) writeFileSync(SCHEMA_FILE, generated);
  assert.equal(readFileSync(SCHEMA_FILE, "utf8"), generated, "run UPDATE_CONTRACT=1 npm test and commit the result");
});

test("contract examples validate as their names say", () => {
  const dir = join(CONTRACT, "examples");
  const files = readdirSync(dir).filter((f) => f.endsWith(".json"));
  assert.ok(files.some((f) => f.startsWith("valid-")) && files.some((f) => f.startsWith("invalid-")));
  for (const f of files) {
    const value: unknown = JSON.parse(readFileSync(join(dir, f), "utf8"));
    assert.equal(Value.Check(ReviewResultSchema, value), f.startsWith("valid-"), f);
  }
});
