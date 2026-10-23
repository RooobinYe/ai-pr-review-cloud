// Cost benchmark driver (docs/architecture.md §7.2, docs/cost-report.md).
// Runs the runner in local mode over bench/prs.tsv and keeps one --stats file
// per run under bench/results/<variant>/. Finished runs are skipped, so an
// interrupted benchmark resumes where it stopped; runs the model API refused
// are not finished and run again.
//
//   node --env-file=.env bench/run.mjs --variant baseline --runs 3
//   node --env-file=.env bench/run.mjs --variant thinking-low --thinking low
//
// LLM_API_KEY comes from the environment; GITHUB_TOKEN defaults to `gh auth token`.
import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const { values } = parseArgs({
  options: {
    variant: { type: "string", default: "baseline" },
    runs: { type: "string", default: "1" },
    concurrency: { type: "string", default: "6" },
    model: { type: "string" },
    thinking: { type: "string" },
    budget: { type: "string" },
    only: { type: "string" },
  },
});

const prs = readFileSync(join(root, "bench/prs.tsv"), "utf8")
  .split("\n")
  .filter((l) => l.trim() && !l.startsWith("#"))
  .map((l) => {
    const [tier, url] = l.split("\t");
    return { tier, url };
  })
  .filter((p) => !values.only || values.only.split(",").includes(p.tier));

const env = { ...process.env };
if (!env.GITHUB_TOKEN) env.GITHUB_TOKEN = execFileSync("gh", ["auth", "token"], { encoding: "utf8" }).trim();
if (values.model) env.LLM_MODEL = values.model;
if (values.thinking) env.LLM_THINKING = values.thinking;

const outDir = join(root, "bench/results", values.variant);
const logDir = join(root, "bench/logs", values.variant);
mkdirSync(outDir, { recursive: true });
mkdirSync(logDir, { recursive: true });

const jobs = [];
for (const pr of prs) {
  const [, owner, repo, number] = pr.url.match(/github\.com\/([^/]+)\/([^/]+)\/pull\/(\d+)/);
  for (let i = 1; i <= Number(values.runs); i++) {
    const name = `${owner}-${repo}-${number}-${i}`;
    if (!finished(join(outDir, `${name}.json`))) jobs.push({ ...pr, name });
  }
}

// A run counts as finished once it has a stats file, unless the model API
// refused it (no credit, bad key): that says nothing about the review, so it
// runs again. A model that never submitted is a real result and is kept.
function finished(statsFile) {
  if (!existsSync(statsFile)) return false;
  const { error } = JSON.parse(readFileSync(statsFile, "utf8"));
  return !error?.startsWith("model API error");
}
console.log(`${values.variant}: ${jobs.length} runs to go`);

function runOne(job) {
  const statsFile = join(outDir, `${job.name}.json`);
  const args = [join(root, "runner/src/main.ts"), "--pr", job.url, "--format", "json", "--stats", statsFile];
  if (values.budget) args.push("--budget", values.budget);
  return new Promise((resolve) => {
    const child = spawn(process.execPath, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("close", (code) => {
      writeFileSync(join(logDir, `${job.name}.log`), stderr);
      writeFileSync(join(logDir, `${job.name}.review.json`), stdout);
      if (existsSync(statsFile)) {
        const s = JSON.parse(readFileSync(statsFile, "utf8"));
        s.tier = job.tier;
        writeFileSync(statsFile, `${JSON.stringify(s, null, 2)}\n`);
        console.log(`${job.name}: exit ${code}, ${s.verdict ?? "no verdict"}, $${s.cost_usd.toFixed(4)}, ${s.seconds.total.toFixed(0)}s`);
      } else {
        console.log(`${job.name}: exit ${code} before reviewing; see ${join("bench/logs", values.variant, `${job.name}.log`)}`);
      }
      resolve();
    });
  });
}

let next = 0;
await Promise.all(
  Array.from({ length: Number(values.concurrency) }, async () => {
    while (next < jobs.length) await runOne(jobs[next++]);
  }),
);
