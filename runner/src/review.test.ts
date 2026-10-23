import assert from "node:assert/strict";
import { test } from "node:test";
import type { SessionStats } from "@earendil-works/pi-coding-agent";
import { BudgetExceededError, ModelError, modelError } from "./review.ts";

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
