// Runner entry point. Two modes share one review pipeline:
//   - cluster mode: RUN_ID is set; inputs come from the fetch init container's
//     /workspace and results go back to the controller.
//   - local mode: --pr <url>; fetches the PR itself and prints the result,
//     for development and CI use without a cluster.
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { SessionStats } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { postResult, putUsage } from "./controller.ts";
import { clonePR, fetchPR } from "./github.ts";
import { type ModelConfig, modelConfigFromEnv, parseThinking, resolveModel } from "./model.ts";
import { parsePRContext, type PRContext } from "./pr.ts";
import { parsePRUrl } from "./prurl.ts";
import { formatUsage, renderMarkdown } from "./render.ts";
import { ReviewNotSubmittedError, review } from "./review.ts";
import { ReviewResultSchema, SCHEMA_VERSION, type ReviewResult } from "./schema.ts";
import { buildRunStats } from "./stats.ts";
import { parseBudget, type TurnUsage } from "./usage.ts";

const USAGE = `usage: runner --pr <github-pr-url> [--format markdown|json] [--language <name>] [--budget <usd>] [--stats <file>] [--keep]

  --budget <usd>  ask the model to submit once 80% of this is spent
  --stats <file>  write tokens, cost and phase timings as JSON (the cost benchmark reads it)

environment:
  LLM_API_KEY    model API key (required)
  LLM_PROVIDER   pi provider id (default deepseek)
  LLM_MODEL      model id (default deepseek-flash)
  LLM_BASE_URL   override the provider endpoint, e.g. a LiteLLM proxy
  LLM_THINKING   off|minimal|low|medium|high|xhigh|max (default high)
  GITHUB_TOKEN   optional; needed for private repositories and higher rate limits

cluster mode (set by the controller's Job):
  RUN_ID, RUN_TOKEN, CONTROLLER_URL, WORKSPACE (default /workspace), REVIEW_LANGUAGE, REVIEW_BUDGET_USD`;

const { values } = parseArgs({
  options: {
    pr: { type: "string" },
    format: { type: "string", default: "markdown" },
    language: { type: "string", default: "English" },
    budget: { type: "string" },
    stats: { type: "string" },
    keep: { type: "boolean", default: false },
    help: { type: "boolean", short: "h", default: false },
  },
});

const log = (line: string) => console.error(`runner: ${line}`);

interface ReviewRun {
  result: ReviewResult;
  session: SessionStats;
  nudged: boolean;
}

interface ReviewPROptions {
  language: string;
  budgetUSD?: number;
  onTurn?: (usage: TurnUsage) => void;
}

/** A review that ended without a result, carrying what it spent. */
class ReviewFailedError extends Error {
  readonly session?: SessionStats;
  constructor(message: string, session?: SessionStats) {
    super(message);
    this.session = session;
  }
}

/** Reviews a checked-out PR and returns the validated result; shared by both modes. */
async function reviewPR(pr: PRContext, cwd: string, agentDir: string, cfg: ModelConfig, opts: ReviewPROptions): Promise<ReviewRun> {
  const started = Date.now();
  const thinkingLevel = parseThinking(cfg.thinking);
  const { model, modelRuntime } = await resolveModel(cfg, agentDir);
  const budget = opts.budgetUSD === undefined ? "" : ` budget $${opts.budgetUSD}`;
  log(`reviewing ${pr.repo}#${pr.number} at ${pr.headSha.slice(0, 7)} with ${model.provider}/${model.id} (thinking ${thinkingLevel})${budget}`);
  try {
    const { submission, stats, nudged } = await review({
      pr,
      cwd,
      agentDir,
      model,
      modelRuntime,
      thinkingLevel,
      language: opts.language,
      budgetUSD: opts.budgetUSD,
      onProgress: (line) => log(`  ${line}`),
      onTurn: opts.onTurn,
    });
    log(formatUsage({ ...stats.tokens, cost: stats.cost }, (Date.now() - started) / 1000));
    const result: ReviewResult = {
      schema_version: SCHEMA_VERSION,
      pr: { repo: pr.repo, number: pr.number, head_sha: pr.headSha },
      model: `${model.provider}/${model.id}`,
      generated_at: new Date().toISOString(),
      ...submission,
    };
    if (!Value.Check(ReviewResultSchema, result)) {
      const errors = [...Value.Errors(ReviewResultSchema, result)].map((e) => `${e.instancePath} ${e.message}`).join("; ");
      throw new ReviewFailedError(`result does not match schema v${SCHEMA_VERSION}: ${errors}`, stats);
    }
    return { result, session: stats, nudged };
  } catch (err) {
    if (err instanceof ReviewNotSubmittedError) {
      log(formatUsage({ ...err.stats.tokens, cost: err.stats.cost }, (Date.now() - started) / 1000));
      throw new ReviewFailedError(err.message, err.stats);
    }
    throw err;
  }
}

const since = (t: number) => (Date.now() - t) / 1000;

async function local(url: string, format: string): Promise<number> {
  if (format !== "markdown" && format !== "json") throw new Error(`--format must be markdown or json, got ${format}`);
  const ref = parsePRUrl(url);
  const cfg = modelConfigFromEnv(process.env);
  parseThinking(cfg.thinking);
  const budgetUSD = parseBudget(values.budget);
  const token = process.env.GITHUB_TOKEN || undefined;

  const scratch = await mkdtemp(join(tmpdir(), "ai-pr-review-"));
  const cwd = join(scratch, "repo");
  const seconds = { fetch: 0, clone: 0, review: 0, total: 0 };
  const started = Date.now();
  let pr: PRContext | undefined;
  let run: ReviewRun | undefined;
  let failure: unknown;
  try {
    log(`fetching ${ref.owner}/${ref.repo}#${ref.number}`);
    let t = Date.now();
    pr = await fetchPR(ref, token);
    seconds.fetch = since(t);
    await mkdir(cwd);
    t = Date.now();
    await clonePR(pr, cwd, token);
    seconds.clone = since(t);
    log(`checked out ${pr.headSha.slice(0, 7)} into ${cwd}; ${pr.files.length} files changed`);
    t = Date.now();
    try {
      run = await reviewPR(pr, cwd, join(scratch, "pi"), cfg, { language: values.language!, budgetUSD });
    } finally {
      seconds.review = since(t);
    }
    process.stdout.write(format === "json" ? `${JSON.stringify(run.result, null, 2)}\n` : renderMarkdown(run.result));
    return 0;
  } catch (err) {
    failure = err;
    throw err;
  } finally {
    seconds.total = since(started);
    if (values.stats && pr) {
      const stats = buildRunStats({
        pr,
        model: `${cfg.provider}/${cfg.model}`,
        thinking: cfg.thinking,
        budgetUSD,
        seconds,
        session: run?.session ?? (failure instanceof ReviewFailedError ? failure.session : undefined),
        nudged: run?.nudged,
        result: run?.result,
        error: failure === undefined ? undefined : failure instanceof Error ? failure.message : String(failure),
      });
      await writeFile(values.stats, `${JSON.stringify(stats, null, 2)}\n`);
    }
    if (values.keep) log(`kept ${scratch}`);
    else await rm(scratch, { recursive: true, force: true });
  }
}

async function cluster(runId: string): Promise<number> {
  const { CONTROLLER_URL: controllerUrl, RUN_TOKEN: token } = process.env;
  if (!controllerUrl || !token) throw new Error("CONTROLLER_URL and RUN_TOKEN must be set");
  const cfg = modelConfigFromEnv(process.env);
  const budgetUSD = parseBudget(process.env.REVIEW_BUDGET_USD);
  const workspace = process.env.WORKSPACE || "/workspace";
  const pr = parsePRContext(JSON.parse(await readFile(join(workspace, "pr.json"), "utf8")));
  const agentDir = await mkdtemp(join(tmpdir(), "pi-"));
  log(`run ${runId}: ${pr.files.length} files changed`);
  // Usage uploads run in the background, in order; a lost one only costs a
  // metrics sample, so failures are logged and the review goes on.
  let uploads = Promise.resolve();
  const onTurn = (usage: TurnUsage) => {
    uploads = uploads.then(() => putUsage(controllerUrl, runId, usage, { token, log }).catch((err: unknown) => log(`usage for turn ${usage.turn} not recorded: ${String(err)}`)));
  };
  try {
    const { result } = await reviewPR(pr, join(workspace, "repo"), agentDir, cfg, {
      language: process.env.REVIEW_LANGUAGE || values.language!,
      budgetUSD,
      onTurn,
    });
    // The last turn's usage lands before the result, so a done run's cost is complete.
    await uploads;
    await postResult(controllerUrl, runId, result, { token, log });
    log(`run ${runId}: posted ${result.verdict} with ${result.comments.length} comments`);
    return 0;
  } finally {
    await uploads;
  }
}

async function main(): Promise<number> {
  if (values.help) {
    console.error(USAGE);
    return 0;
  }
  if (process.env.RUN_ID) return cluster(process.env.RUN_ID);
  if (values.pr) return local(values.pr, values.format!);
  console.error(USAGE);
  return 2;
}

main().then(
  (code) => process.exit(code),
  (err: unknown) => {
    log(err instanceof Error ? err.message : String(err));
    process.exit(1);
  },
);
