import assert from "node:assert/strict";
import { test } from "node:test";
import { overNudgeThreshold, parseBudget, turnUsage } from "./usage.ts";

test("turnUsage reads an assistant message's usage", () => {
  const message = {
    role: "assistant",
    provider: "deepseek",
    model: "deepseek-flash",
    usage: { input: 1200, output: 340, cacheRead: 15000, cacheWrite: 0, totalTokens: 16540, cost: { total: 0.0009 } },
  };
  assert.deepEqual(turnUsage(3, message), {
    turn: 3,
    model: "deepseek/deepseek-flash",
    input_tokens: 1200,
    output_tokens: 340,
    cache_read_tokens: 15000,
    cache_write_tokens: 0,
    cost_usd: 0.0009,
  });
});

test("turnUsage ignores messages without usage", () => {
  for (const message of [undefined, null, "text", { role: "user", content: "hi" }, { role: "assistant" }, { role: "toolResult", usage: {} }]) {
    assert.equal(turnUsage(1, message), undefined, JSON.stringify(message));
  }
});

test("turnUsage clamps missing or invalid numbers to zero", () => {
  const usage = turnUsage(1, { role: "assistant", usage: { input: -5, output: Number.NaN, cost: {} } });
  assert.deepEqual(usage, {
    turn: 1,
    model: "unknown",
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    cost_usd: 0,
  });
});

test("overNudgeThreshold fires at 80% of a positive budget", () => {
  const cases: [number, number | undefined, boolean][] = [
    [0.39, 0.5, false],
    [0.4, 0.5, true],
    [0.6, 0.5, true],
    [10, undefined, false],
    [10, 0, false],
  ];
  for (const [spent, budget, want] of cases) assert.equal(overNudgeThreshold(spent, budget), want, `${spent} of ${budget}`);
});

test("parseBudget accepts positive amounts and treats empty as none", () => {
  assert.equal(parseBudget(undefined), undefined);
  assert.equal(parseBudget(""), undefined);
  assert.equal(parseBudget("0.50"), 0.5);
  for (const bad of ["0", "-1", "abc", "Infinity"]) assert.throws(() => parseBudget(bad), /positive number/, bad);
});
