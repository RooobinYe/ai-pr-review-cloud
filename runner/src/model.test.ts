import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { modelConfigFromEnv, parseThinking, resolveModel } from "./model.ts";

test("modelConfigFromEnv defaults to DeepSeek Flash", () => {
  assert.throws(() => modelConfigFromEnv({}), /LLM_API_KEY/);
  assert.deepEqual(modelConfigFromEnv({ LLM_API_KEY: "k" }), {
    provider: "deepseek",
    model: "deepseek-flash",
    apiKey: "k",
    thinking: "high",
  });
  assert.equal(modelConfigFromEnv({ LLM_API_KEY: "k", LLM_BASE_URL: "http://litellm:4000" }).baseUrl, "http://litellm:4000");
});

test("parseThinking", () => {
  assert.equal(parseThinking("low"), "low");
  assert.throws(() => parseThinking("extreme"), /LLM_THINKING/);
});

test("resolveModel uses the catalog for built-in providers and registers others", async () => {
  const dir = await mkdtemp(join(tmpdir(), "runner-model-"));
  try {
    const builtin = await resolveModel({ provider: "deepseek", model: "deepseek-flash", apiKey: "k", baseUrl: "http://proxy/v1", thinking: "high" }, dir);
    assert.equal(builtin.model.baseUrl, "http://proxy/v1");
    assert.ok(builtin.model.cost.input > 0, "catalog pricing is kept");

    const custom = await resolveModel({ provider: "my-proxy", model: "qwen-coder", apiKey: "k", baseUrl: "http://proxy/v1", thinking: "off" }, dir);
    assert.equal(custom.model.api, "openai-completions");
    await assert.rejects(resolveModel({ provider: "my-proxy", model: "x", apiKey: "k", thinking: "off" }, dir), /LLM_BASE_URL/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
