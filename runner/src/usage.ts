// Per-turn model usage and the budget nudge (docs/architecture.md §7.3).
// Cluster mode uploads each turn's usage to the controller, which turns it
// into the token and cost metrics of §7.1.

/** One turn's usage, in the shape PUT /runs/{id}/usage accepts. */
export interface TurnUsage {
  turn: number;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_usd: number;
}

/** Share of the budget at which the runner asks the model to wrap up. */
export const BUDGET_NUDGE_SHARE = 0.8;

/**
 * Extracts the usage of an assistant message, the message of a turn_end event.
 * Other messages carry no usage and yield undefined.
 */
export function turnUsage(turn: number, message: unknown): TurnUsage | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  const m = message as { role?: unknown; provider?: unknown; model?: unknown; usage?: unknown };
  if (m.role !== "assistant" || typeof m.usage !== "object" || m.usage === null) return undefined;
  const u = m.usage as { input?: unknown; output?: unknown; cacheRead?: unknown; cacheWrite?: unknown; cost?: { total?: unknown } };
  const n = (v: unknown) => (typeof v === "number" && Number.isFinite(v) && v > 0 ? v : 0);
  return {
    turn,
    model: typeof m.provider === "string" && typeof m.model === "string" ? `${m.provider}/${m.model}` : "unknown",
    input_tokens: n(u.input),
    output_tokens: n(u.output),
    cache_read_tokens: n(u.cacheRead),
    cache_write_tokens: n(u.cacheWrite),
    cost_usd: n(u.cost?.total),
  };
}

export interface TurnProblem {
  text: string;
  /** The provider refused the call after pi's retries, e.g. 402 insufficient balance; reminding the model cannot help. */
  providerError: boolean;
}

/**
 * Describes an assistant turn that ended abnormally (a provider error, the
 * output limit, an abort), which otherwise looks like the model stopping
 * without submitting. Normal turns yield undefined.
 */
export function turnProblem(message: unknown): TurnProblem | undefined {
  if (typeof message !== "object" || message === null) return undefined;
  const m = message as { role?: unknown; stopReason?: unknown; errorMessage?: unknown };
  if (m.role !== "assistant" || (m.stopReason !== "error" && m.stopReason !== "length" && m.stopReason !== "aborted")) return undefined;
  const detail = typeof m.errorMessage === "string" && m.errorMessage ? `: ${m.errorMessage.split("\n")[0]!.slice(0, 300)}` : "";
  return { text: `model turn ended with ${m.stopReason}${detail}`, providerError: m.stopReason === "error" };
}

/** Reports whether spending has crossed the nudge threshold of a positive budget. */
export function overNudgeThreshold(spentUSD: number, budgetUSD: number | undefined): boolean {
  return budgetUSD !== undefined && budgetUSD > 0 && spentUSD >= budgetUSD * BUDGET_NUDGE_SHARE;
}

/** Parses a budget in USD; empty means no budget. */
export function parseBudget(value: string | undefined): number | undefined {
  if (value === undefined || value.trim() === "") return undefined;
  const budget = Number(value);
  if (!Number.isFinite(budget) || budget <= 0) throw new Error(`budget must be a positive number of USD, got ${value}`);
  return budget;
}
