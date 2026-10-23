// Runner entry point. Two modes share one review pipeline:
//   - cluster mode: RUN_ID is set; inputs come from the fetch init container's
//     /workspace and results go back to the controller.
//   - local mode: --pr <url>; fetches the PR itself and prints the result,
//     for development and CI use without a cluster.
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { Value } from "typebox/value";
import { clonePR, fetchPR } from "./github.ts";
import { modelConfigFromEnv, parseThinking, resolveModel } from "./model.ts";
import { parsePRUrl } from "./prurl.ts";
import { formatUsage, renderMarkdown } from "./render.ts";
import { ReviewNotSubmittedError, review } from "./review.ts";
import { ReviewResultSchema, SCHEMA_VERSION, type ReviewResult } from "./schema.ts";

const USAGE = `usage: runner --pr <github-pr-url> [--format markdown|json] [--language <name>] [--keep]

environment:
  LLM_API_KEY    model API key (required)
  LLM_PROVIDER   pi provider id (default deepseek)
  LLM_MODEL      model id (default deepseek-flash)
  LLM_BASE_URL   override the provider endpoint, e.g. a LiteLLM proxy
  LLM_THINKING   off|minimal|low|medium|high|xhigh|max (default high)
  GITHUB_TOKEN   optional; needed for private repositories and higher rate limits`;

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

async function local(url: string, format: string): Promise<number> {
  if (format !== "markdown" && format !== "json") throw new Error(`--format must be markdown or json, got ${format}`);
  const ref = parsePRUrl(url);
  const cfg = modelConfigFromEnv(process.env);
  const thinkingLevel = parseThinking(cfg.thinking);
  const token = process.env.GITHUB_TOKEN || undefined;
  const started = Date.now();

  const scratch = await mkdtemp(join(tmpdir(), "ai-pr-review-"));
  const cwd = join(scratch, "repo");
  const agentDir = join(scratch, "pi");
  try {
    log(`fetching ${ref.owner}/${ref.repo}#${ref.number}`);
    const pr = await fetchPR(ref, token);
    await mkdir(cwd);
    await clonePR(pr, cwd, token);
    log(`checked out ${pr.headSha.slice(0, 7)} into ${cwd}; ${pr.files.length} files changed`);

    const { model, modelRuntime } = await resolveModel(cfg, agentDir);
    log(`reviewing with ${model.provider}/${model.id} (thinking ${thinkingLevel})`);
    try {
      const { submission, stats } = await review({
        pr,
        cwd,
        agentDir,
        model,
        modelRuntime,
        thinkingLevel,
        language: values.language!,
        onProgress: (line) => log(`  ${line}`),
      });
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
      process.stdout.write(format === "json" ? `${JSON.stringify(result, null, 2)}\n` : renderMarkdown(result));
      log(formatUsage({ ...stats.tokens, cost: stats.cost }, (Date.now() - started) / 1000));
      return 0;
    } catch (err) {
      if (err instanceof ReviewNotSubmittedError) {
        log(formatUsage({ ...err.stats.tokens, cost: err.stats.cost }, (Date.now() - started) / 1000));
      }
      throw err;
    }
  } finally {
    if (values.keep) log(`kept ${scratch}`);
    else await rm(scratch, { recursive: true, force: true });
  }
}

async function main(): Promise<number> {
  if (values.help) {
    console.error(USAGE);
    return 0;
  }
  if (process.env.RUN_ID) {
    log(`cluster mode for run ${process.env.RUN_ID} is not implemented yet (milestone M1)`);
    return 2;
  }
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
