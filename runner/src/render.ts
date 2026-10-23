// Human-readable output for local mode.
import { SEVERITIES, type ReviewResult } from "./schema.ts";

const VERDICT_LABEL: Record<ReviewResult["verdict"], string> = {
  approve: "✅ Approve",
  request_changes: "❌ Request changes",
};

/** Renders a result roughly as it will appear on GitHub: the body, then inline comments by file. */
export function renderMarkdown(r: ReviewResult): string {
  const out = [`# AI Review: ${r.pr.repo}#${r.pr.number}`, "", `**${VERDICT_LABEL[r.verdict]}** · \`${r.model}\` · ${r.pr.head_sha.slice(0, 7)}`, "", r.body.trim()];
  if (r.comments.length) {
    out.push("", `## Inline comments (${r.comments.length})`);
    const sorted = [...r.comments].sort(
      (a, b) => SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity) || a.path.localeCompare(b.path) || a.line - b.line,
    );
    for (const c of sorted) {
      out.push("", `### \`${c.path}:${c.line}\` · ${c.severity} · confidence ${c.confidence}`, "", c.body.trim());
    }
  }
  return `${out.join("\n")}\n`;
}

export interface UsageSummary {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  cost: number;
}

export function formatUsage(u: UsageSummary, seconds: number): string {
  const k = (n: number) => (n >= 10_000 ? `${(n / 1000).toFixed(1)}k` : String(n));
  return `tokens: input ${k(u.input)} (cache read ${k(u.cacheRead)}, cache write ${k(u.cacheWrite)}), output ${k(u.output)} · cost $${u.cost.toFixed(4)} · ${seconds.toFixed(0)}s`;
}
