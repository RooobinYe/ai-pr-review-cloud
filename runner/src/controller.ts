// Cluster mode: report to the controller (docs/architecture.md §4.2, §4.3).
// The runner has no GitHub credentials; the controller performs every write.
import type { ReviewResult } from "./schema.ts";

export interface CallOptions {
  /** The run's callback token (RUN_TOKEN), sent as a bearer token. */
  token: string;
  attempts?: number;
  /** Delay before the first retry; doubles after each attempt. */
  retryDelayMs?: number;
  log?: (line: string) => void;
}

/** A response the controller will not change its mind about, such as a result for the wrong commit. */
export class RejectedError extends Error {}

/** One turn's model usage, as PUT /runs/{id}/usage takes it. */
export interface TurnUsage {
  turn: number;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  cost_usd: number;
}

const retryable = (status: number) => status >= 500 || status === 408 || status === 429;

/**
 * Calls the controller, retrying network errors and 5xx responses. Every
 * endpoint is idempotent, so retrying after a lost response is safe. Returns
 * the response when it is ok or one of the accepted statuses.
 */
async function call(
  controllerUrl: string,
  path: string,
  init: { method: string; body?: string; contentType?: string },
  opts: CallOptions,
  defaultAttempts: number,
  accept: number[] = [],
): Promise<Response> {
  const url = `${controllerUrl.replace(/\/$/, "")}${path}`;
  const attempts = opts.attempts ?? defaultAttempts;
  let delay = opts.retryDelayMs ?? 1000;
  for (let attempt = 1; ; attempt++) {
    let problem: string;
    try {
      const res = await fetch(url, {
        method: init.method,
        headers: { Authorization: `Bearer ${opts.token}`, ...(init.contentType ? { "Content-Type": init.contentType } : {}) },
        body: init.body,
        signal: AbortSignal.timeout(30_000),
      });
      if (res.ok || accept.includes(res.status)) return res;
      problem = `${res.status} ${(await res.text()).trim().slice(0, 300)}`;
      if (!retryable(res.status)) throw new RejectedError(`controller rejected ${init.method} ${path}: ${problem}`);
    } catch (err) {
      if (err instanceof RejectedError) throw err;
      problem = err instanceof Error ? `${err.message}${err.cause instanceof Error ? `: ${err.cause.message}` : ""}` : String(err);
    }
    if (attempt >= attempts) throw new Error(`${init.method} ${path} failed after ${attempts} attempts: ${problem}`);
    opts.log?.(`${init.method} ${path} failed (${problem}); retrying in ${delay}ms`);
    await new Promise((resolve) => setTimeout(resolve, delay));
    delay *= 2;
  }
}

const runPath = (runId: string, rest: string) => `/runs/${encodeURIComponent(runId)}/${rest}`;

/**
 * POSTs the result to {controllerUrl}/runs/{runId}/result. 8 attempts back off
 * for about two minutes in total, enough to outlast a controller restart (it
 * is unready, so unreachable, until it can accept results).
 */
export async function postResult(controllerUrl: string, runId: string, result: ReviewResult, opts: CallOptions): Promise<void> {
  await call(controllerUrl, runPath(runId, "result"), { method: "POST", body: JSON.stringify(result), contentType: "application/json" }, opts, 8);
}

/**
 * Fetches the session snapshot an earlier attempt of this run left, or
 * undefined if it left none. Retried as long as a result: resuming saves the
 * turns already paid for.
 */
export async function getSession(controllerUrl: string, runId: string, opts: CallOptions): Promise<string | undefined> {
  const res = await call(controllerUrl, runPath(runId, "session"), { method: "GET" }, opts, 8, [404]);
  return res.status === 404 ? undefined : res.text();
}

/** Uploads the session after a turn, as JSONL. The controller keeps the newest turn. */
export async function putSession(controllerUrl: string, runId: string, turn: number, jsonl: string, opts: CallOptions): Promise<void> {
  await call(controllerUrl, runPath(runId, `session?turn=${turn}`), { method: "PUT", body: jsonl, contentType: "application/x-ndjson" }, opts, 3);
}

/** Reports one turn's usage; reporting a turn again replaces it. */
export async function putUsage(controllerUrl: string, runId: string, usage: TurnUsage, opts: CallOptions): Promise<void> {
  await call(controllerUrl, runPath(runId, "usage"), { method: "PUT", body: JSON.stringify(usage), contentType: "application/json" }, opts, 3);
}
