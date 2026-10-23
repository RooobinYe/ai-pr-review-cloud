// Runner entry point. Two modes share one review pipeline:
//   - cluster mode: RUN_ID is set; inputs come from the fetch init container's
//     /workspace and results go back to the controller.
//   - local mode: --pr <url>; fetches the PR itself and prints the result,
//     for development and CI use without a cluster.
import { previewBundle, preparePreview } from "./preview.ts";
import { writeFileSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { parseSessionEntries, type SessionStats } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { getSession, postResult, putCheckpoint, RejectedError } from "./controller.ts";
import { clonePR, fetchPR } from "./github.ts";
import { type ModelConfig, modelConfigFromEnv, parseThinking, resolveModel } from "./model.ts";
import { parsePRContext, type PRContext } from "./pr.ts";
import { parsePRUrl } from "./prurl.ts";
import { formatUsage, renderMarkdown } from "./render.ts";
import { BudgetExceededError, ReviewFailedError, ReviewNotSubmittedError, type ReviewOptions, review } from "./review.ts";
import { ReviewResultSchema, SCHEMA_VERSION, type ReviewResult } from "./schema.ts";
import { buildRunStats } from "./stats.ts";
import { parseBudget } from "./usage.ts";

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

/**
 * Exit code for failures another attempt cannot fix; the controller retries
 * any other exit. Mirrors run.ExitFinal in go/internal/run/exit.go.
 */
const EXIT_FINAL = 3;

/** A failure that is final however often the run is retried. */
class FinalError extends Error {}

const isFinal = (err: unknown) => err instanceof FinalError || err instanceof RejectedError || err instanceof ReviewNotSubmittedError || err instanceof BudgetExceededError;

interface ReviewRun {
  result: ReviewResult;
  session: SessionStats;
  nudged: boolean;
}

interface ReviewPROptions extends Pick<ReviewOptions, "resumeFrom" | "onTurn" | "preparePreview"> {
  language: string;
  budgetUSD?: number;
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
      resumeFrom: opts.resumeFrom,
      onTurn: opts.onTurn,
      preparePreview: opts.preparePreview,
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
    if (err instanceof ReviewFailedError) {
      log(formatUsage({ ...err.stats.tokens, cost: err.stats.cost }, (Date.now() - started) / 1000));
      throw err;
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
        session: run?.session ?? (failure instanceof ReviewFailedError ? failure.stats : undefined),
        nudged: run?.nudged,
        result: run?.result,
        error: failure === undefined ? undefined : failure instanceof Error ? failure.message : String(failure),
      });
      // A stats problem must not hide why the review failed or skip the cleanup.
      await writeFile(values.stats, `${JSON.stringify(stats, null, 2)}\n`).catch((err: unknown) => log(`could not write ${values.stats}: ${String(err)}`));
    }
    if (values.keep) log(`kept ${scratch}`);
    else await rm(scratch, { recursive: true, force: true });
  }
}

async function cluster(runId: string): Promise<number> {
  const { CONTROLLER_URL: controllerUrl, RUN_TOKEN: token } = process.env;
  if (!controllerUrl || !token) throw new FinalError("CONTROLLER_URL and RUN_TOKEN must be set");
  const cfg = modelConfigFromEnv(process.env);
  const budgetUSD = parseBudget(process.env.REVIEW_BUDGET_USD);
  const workspace = process.env.WORKSPACE || "/workspace";
  let pr: PRContext;
  try {
    pr = parsePRContext(JSON.parse(await readFile(join(workspace, "pr.json"), "utf8")));
  } catch (err) {
    throw new FinalError(`read pr.json: ${err instanceof Error ? err.message : String(err)}`);
  }
  const agentDir = await mkdtemp(join(tmpdir(), "pi-"));
  const remote = { token, log };
  log(`run ${runId}: ${pr.files.length} files changed, ${pr.previousComments?.length ?? 0} earlier comments`);

  // A failed earlier attempt left its session behind; continue from there.
  const saved = await getSession(controllerUrl, runId, remote);
  const resumeFrom = saved ? parseSessionEntries(saved) : undefined;
  if (resumeFrom) log(`run ${runId}: resuming from a snapshot with ${resumeFrom.length} entries`);
  const bundle = await previewBundle(join(workspace, "repo"));
  const { result } = await reviewPR(pr, join(workspace, "repo"), agentDir, cfg, {
    preparePreview: bundle ? () => preparePreview(controllerUrl, runId, bundle, join(agentDir, "preview-kubeconfig.json"), remote) : undefined,
    language: process.env.REVIEW_LANGUAGE || values.language!,
    budgetUSD,
    resumeFrom,
    onTurn: (snapshot) => putCheckpoint(controllerUrl, runId, snapshot, remote),
  });
  await postResult(controllerUrl, runId, result, remote);
  log(`run ${runId}: posted ${result.verdict} with ${result.comments.length} comments`);
  return 0;
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

/**
 * In cluster mode the failure reason goes to the termination log, which the
 * controller copies to the run's Check Run; Kubernetes keeps up to 4 KiB.
 */
function reportFailure(message: string): void {
  if (!process.env.RUN_ID) return;
  try {
    writeFileSync(process.env.TERMINATION_LOG || "/dev/termination-log", message.slice(0, 4000));
  } catch {
    // Outside Kubernetes there is no termination log; the message is logged anyway.
  }
}
// Node as PID 1 ignores SIGTERM, so a deleted Pod (a superseded run, or one
// being evicted) would keep reviewing, and paying, through its grace period.
// The exit is retryable; the next attempt resumes from the last snapshot.
process.on("SIGTERM", () => {
  log("terminated");
  process.exit(143);
});

main().then(
  (code) => process.exit(code),
  (err: unknown) => {
    const message = err instanceof Error ? err.message : String(err);
    log(message);
    reportFailure(message);
    process.exit(isFinal(err) ? EXIT_FINAL : 1);
  },
);
