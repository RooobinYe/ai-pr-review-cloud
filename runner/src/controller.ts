// Cluster mode: report results to the controller (docs/architecture.md §4.2).
// The runner has no GitHub credentials; the controller performs every write.
import type { ReviewResult } from "./schema.ts";

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
 * POSTs the result to {controllerUrl}/runs/{runId}/result, retrying network
 * errors and 5xx responses. The controller accepts a repeated POST of the same
 * result, so retrying after a lost response is safe.
 */
export async function postResult(controllerUrl: string, runId: string, result: ReviewResult, opts: PostOptions): Promise<void> {
  const url = `${controllerUrl.replace(/\/$/, "")}/runs/${encodeURIComponent(runId)}/result`;
  // 8 attempts back off for about two minutes in total, enough to outlast a
  // controller restart (it is unready, so unreachable, until it can accept results).
  const attempts = opts.attempts ?? 8;
  let delay = opts.retryDelayMs ?? 1000;
  for (let attempt = 1; ; attempt++) {
    let problem: string;
    try {
      const res = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${opts.token}` },
        body: JSON.stringify(result),
        signal: AbortSignal.timeout(30_000),
      });
      if (res.ok) return;
      problem = `${res.status} ${(await res.text()).trim().slice(0, 300)}`;
      if (!retryable(res.status)) throw new ResultRejectedError(`controller rejected the result: ${problem}`);
    } catch (err) {
      if (err instanceof ResultRejectedError) throw err;
      problem = err instanceof Error ? `${err.message}${err.cause instanceof Error ? `: ${err.cause.message}` : ""}` : String(err);
    }
    if (attempt >= attempts) throw new Error(`could not post result after ${attempts} attempts: ${problem}`);
    opts.log?.(`posting result failed (${problem}); retrying in ${delay}ms`);
    await new Promise((resolve) => setTimeout(resolve, delay));
    delay *= 2;
  }
}
