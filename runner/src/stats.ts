// Machine-readable summary of one local-mode run, written by --stats for the
// cost benchmark (docs/cost-report.md).
import type { SessionStats } from "@earendil-works/pi-coding-agent";
import type { PRContext } from "./pr.ts";
import { SEVERITIES, type ReviewResult, type Severity } from "./schema.ts";

export interface RunStats {
  pr: string;
  head_sha: string;
  model: string;
  thinking: string;
  budget_usd: number | null;
  files: number;
  /** Added plus deleted lines, the size measure of the benchmark tiers. */
  changed_lines: number;
  seconds: { fetch: number; clone: number; review: number; total: number };
  turns: number;
  tool_calls: number;
  tokens: { input: number; output: number; cache_read: number; cache_write: number };
  cost_usd: number;
  nudged: boolean;
  verdict: ReviewResult["verdict"] | null;
  comments: Record<Severity, number>;
  error: string | null;
}

export interface StatsInput {
  pr: PRContext;
  model: string;
  thinking: string;
  budgetUSD?: number;
  seconds: RunStats["seconds"];
  session?: SessionStats;
  nudged?: boolean;
  result?: ReviewResult;
  error?: string;
}

export function buildRunStats(s: StatsInput): RunStats {
  const comments = Object.fromEntries(SEVERITIES.map((sev) => [sev, 0])) as Record<Severity, number>;
  for (const c of s.result?.comments ?? []) comments[c.severity]++;
  const t = s.session?.tokens;
  return {
    pr: `${s.pr.repo}#${s.pr.number}`,
    head_sha: s.pr.headSha,
    model: s.model,
    thinking: s.thinking,
    budget_usd: s.budgetUSD ?? null,
    files: s.pr.files.length,
    changed_lines: s.pr.files.reduce((n, f) => n + f.additions + f.deletions, 0),
    seconds: s.seconds,
    turns: s.session?.assistantMessages ?? 0,
    tool_calls: s.session?.toolCalls ?? 0,
    tokens: { input: t?.input ?? 0, output: t?.output ?? 0, cache_read: t?.cacheRead ?? 0, cache_write: t?.cacheWrite ?? 0 },
    cost_usd: s.session?.cost ?? 0,
    nudged: s.nudged ?? false,
    verdict: s.result?.verdict ?? null,
    comments,
    error: s.error ?? null,
  };
}
