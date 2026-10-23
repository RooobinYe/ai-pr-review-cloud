// Runner entry point. Two modes share one review pipeline:
//   - cluster mode: RUN_ID is set; inputs come from the fetch init container's
//     /workspace and results go back to the controller.
//   - local mode: --pr <url>; fetches the PR itself and prints the result,
//     for development and CI use without a cluster.
import { writeFileSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { Value } from "typebox/value";
import { postResult } from "./controller.ts";
import { clonePR, fetchPR } from "./github.ts";
import { type ModelConfig, modelConfigFromEnv, parseThinking, resolveModel } from "./model.ts";
import { parsePRContext, type PRContext } from "./pr.ts";
import { parsePRUrl } from "./prurl.ts";
import { formatUsage, renderMarkdown } from "./render.ts";
import { ReviewFailedError, review } from "./review.ts";
import { ReviewResultSchema, SCHEMA_VERSION, type ReviewResult } from "./schema.ts";

const USAGE = `usage: runner --pr <github-pr-url> [--format markdown|json] [--language <name>] [--keep]

environment:
  LLM_API_KEY    model API key (required)
  LLM_PROVIDER   pi provider id (default deepseek)
  LLM_MODEL      model id (default deepseek-flash)
  LLM_BASE_URL   override the provider endpoint, e.g. a LiteLLM proxy
  LLM_THINKING   off|minimal|low|medium|high|xhigh|max (default high)
  GITHUB_TOKEN   optional; needed for private repositories and higher rate limits

cluster mode (set by the controller's Job):
  RUN_ID, RUN_TOKEN, CONTROLLER_URL, WORKSPACE (default /workspace), REVIEW_LANGUAGE`;

const { values } = parseArgs({
  options: {
    pr: { type: "string" },
    format: { type: "string", default: "markdown" },
    language: { type: "string", default: "English" },
    keep: { type: "boolean", default: false },
    help: { type: "boolean", short: "h", default: false },
  },
});

const log = (line: string) => console.error(`runner: ${line}`);

/** Reviews a checked-out PR and returns the validated result; shared by both modes. */
async function reviewPR(pr: PRContext, cwd: string, agentDir: string, cfg: ModelConfig, language: string): Promise<ReviewResult> {
  const started = Date.now();
  const thinkingLevel = parseThinking(cfg.thinking);
  const { model, modelRuntime } = await resolveModel(cfg, agentDir);
  log(`reviewing ${pr.repo}#${pr.number} at ${pr.headSha.slice(0, 7)} with ${model.provider}/${model.id} (thinking ${thinkingLevel})`);
  try {
    const { submission, stats } = await review({
      pr,
      cwd,
      agentDir,
      model,
      modelRuntime,
      thinkingLevel,
      language,
      onProgress: (line) => log(`  ${line}`),
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
      throw new Error(`result does not match schema v${SCHEMA_VERSION}: ${[...Value.Errors(ReviewResultSchema, result)].map((e) => `${e.instancePath} ${e.message}`).join("; ")}`);
    }
    return result;
  } catch (err) {
    if (err instanceof ReviewFailedError) {
      log(formatUsage({ ...err.stats.tokens, cost: err.stats.cost }, (Date.now() - started) / 1000));
    }
    throw err;
  }
}

async function local(url: string, format: string): Promise<number> {
  if (format !== "markdown" && format !== "json") throw new Error(`--format must be markdown or json, got ${format}`);
  const ref = parsePRUrl(url);
  const cfg = modelConfigFromEnv(process.env);
  parseThinking(cfg.thinking);
  const token = process.env.GITHUB_TOKEN || undefined;

  const scratch = await mkdtemp(join(tmpdir(), "ai-pr-review-"));
  const cwd = join(scratch, "repo");
  try {
    log(`fetching ${ref.owner}/${ref.repo}#${ref.number}`);
    const pr = await fetchPR(ref, token);
    await mkdir(cwd);
    await clonePR(pr, cwd, token);
    log(`checked out ${pr.headSha.slice(0, 7)} into ${cwd}; ${pr.files.length} files changed`);
    const result = await reviewPR(pr, cwd, join(scratch, "pi"), cfg, values.language!);
    process.stdout.write(format === "json" ? `${JSON.stringify(result, null, 2)}\n` : renderMarkdown(result));
    return 0;
  } finally {
    if (values.keep) log(`kept ${scratch}`);
    else await rm(scratch, { recursive: true, force: true });
  }
}

async function cluster(runId: string): Promise<number> {
  const { CONTROLLER_URL: controllerUrl, RUN_TOKEN: token } = process.env;
  if (!controllerUrl || !token) throw new Error("CONTROLLER_URL and RUN_TOKEN must be set");
  const cfg = modelConfigFromEnv(process.env);
  const workspace = process.env.WORKSPACE || "/workspace";
  const pr = parsePRContext(JSON.parse(await readFile(join(workspace, "pr.json"), "utf8")));
  const agentDir = await mkdtemp(join(tmpdir(), "pi-"));
  log(`run ${runId}: ${pr.files.length} files changed`);
  const result = await reviewPR(pr, join(workspace, "repo"), agentDir, cfg, process.env.REVIEW_LANGUAGE || values.language!);
  await postResult(controllerUrl, runId, result, { token, log });
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

main().then(
  (code) => process.exit(code),
  (err: unknown) => {
    const message = err instanceof Error ? err.message : String(err);
    log(message);
    reportFailure(message);
    process.exit(1);
  },
);
