// Cluster mode: report results and usage to the controller (docs/architecture.md §4.2).
// The runner has no GitHub credentials; the controller performs every write.
import type { ReviewResult } from "./schema.ts";
import type { TurnUsage } from "./usage.ts";

export interface PostOptions {
  /** The run's callback token (RUN_TOKEN), sent as a bearer token. */
  token: string;
  attempts?: number;
  /** Delay before the first retry; doubles after each attempt. */
  retryDelayMs?: number;
  log?: (line: string) => void;
}

/** A response the controller will not change its mind about, such as a result for the wrong commit. */
export class ResultRejectedError extends Error {}

const retryable = (status: number) => status >= 500 || status === 408 || status === 429;

/**
 * Sends body to {controllerUrl}/runs/{runId}/{path}, retrying network errors
 * and 5xx responses. Both endpoints are idempotent, so retrying after a lost
 * response is safe.
 */
async function send(controllerUrl: string, runId: string, path: string, method: string, body: unknown, opts: PostOptions, defaultAttempts: number): Promise<void> {
  const url = `${controllerUrl.replace(/\/$/, "")}/runs/${encodeURIComponent(runId)}/${path}`;
  const attempts = opts.attempts ?? defaultAttempts;
  let delay = opts.retryDelayMs ?? 1000;
  for (let attempt = 1; ; attempt++) {
    let problem: string;
    try {
      const res = await fetch(url, {
        method,
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${opts.token}` },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(30_000),
      });
      if (res.ok) return;
      problem = `${res.status} ${(await res.text()).trim().slice(0, 300)}`;
      if (!retryable(res.status)) throw new ResultRejectedError(`controller rejected the ${path}: ${problem}`);
    } catch (err) {
      if (err instanceof ResultRejectedError) throw err;
      problem = err instanceof Error ? `${err.message}${err.cause instanceof Error ? `: ${err.cause.message}` : ""}` : String(err);
    }
    if (attempt >= attempts) throw new Error(`could not send ${path} after ${attempts} attempts: ${problem}`);
    opts.log?.(`sending ${path} failed (${problem}); retrying in ${delay}ms`);
    await new Promise((resolve) => setTimeout(resolve, delay));
    delay *= 2;
  }
}

/** POSTs the result. The controller accepts a repeated POST of the same result. */
export function postResult(controllerUrl: string, runId: string, result: ReviewResult, opts: PostOptions): Promise<void> {
  // 8 attempts back off for about two minutes in total, enough to outlast a
  // controller restart (it is unready, so unreachable, until it can accept results).
  return send(controllerUrl, runId, "result", "POST", result, opts, 8);
}

/** PUTs one turn's usage. The controller keeps the latest report per turn, so a repeat overwrites. */
export function putUsage(controllerUrl: string, runId: string, usage: TurnUsage, opts: PostOptions): Promise<void> {
  // Usage only feeds metrics: a few quick attempts, never holding up the review.
  return send(controllerUrl, runId, "usage", "PUT", usage, opts, 3);
}
