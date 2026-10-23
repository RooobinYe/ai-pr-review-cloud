// Summarizes bench/results into the Markdown tables of docs/cost-report.md.
//
//   node bench/report.mjs            # every variant found under bench/results
//
// Percentiles interpolate linearly between ranks. Costs are pi's catalog prices
// (deepseek-flash: input $0.30, cache read $0.006, output $1.20 per million tokens).
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

const root = join(import.meta.dirname, "results");
const TIERS = ["small", "medium", "large"];
const TIER_LABEL = { small: "小（<100 行）", medium: "中（100–500 行）", large: "大（>500 行）" };
// Per million tokens, from pi's model catalog (runner/node_modules/@earendil-works/pi-ai).
const PRICES = {
  "deepseek/deepseek-flash": { input: 0.3, cacheRead: 0.006, output: 1.2 },
  "deepseek/deepseek-v4-pro": { input: 1.32, cacheRead: 0.044, output: 3.96 },
};

const variants = Object.fromEntries(
  readdirSync(root, { withFileTypes: true })
    .filter((d) => d.isDirectory())
    .map((d) => [d.name, readdirSync(join(root, d.name)).filter((f) => f.endsWith(".json")).map((f) => JSON.parse(readFileSync(join(root, d.name, f), "utf8")))]),
);

function pct(values, p) {
  if (!values.length) return NaN;
  const s = [...values].sort((a, b) => a - b);
  const i = (s.length - 1) * p;
  const lo = Math.floor(i);
  return s[lo] + (s[Math.min(lo + 1, s.length - 1)] - s[lo]) * (i - lo);
}
const mean = (v) => v.reduce((a, b) => a + b, 0) / v.length;
const usd = (v) => `$${v.toFixed(4)}`;
const sec = (v) => `${v.toFixed(0)}s`;
const k = (v) => `${(v / 1000).toFixed(1)}k`;
const share = (v) => `${(v * 100).toFixed(0)}%`;
const table = (head, rows) => [`| ${head.join(" | ")} |`, `| ${head.map(() => "---").join(" | ")} |`, ...rows.map((r) => `| ${r.join(" | ")} |`)].join("\n");
const ok = (runs) => runs.filter((r) => r.verdict);
const serious = (r) => r.comments.critical + r.comments.high;
const noCacheCost = (r) => {
  const p = PRICES[r.model];
  return p ? r.cost_usd + (r.tokens.cache_read * (p.input - p.cacheRead)) / 1e6 : NaN;
};
const byPR = (runs) => Object.groupBy(runs, (r) => r.pr);
const majority = (runs) => {
  const counts = Object.groupBy(ok(runs), (r) => r.verdict);
  return Object.entries(counts).sort((a, b) => b[1].length - a[1].length)[0]?.[0];
};

const out = [];
const base = variants.baseline ?? [];

if (base.length) {
  out.push("### 基线：按改动规模分档", "");
  const rows = [];
  for (const tier of [...TIERS, "all"]) {
    const runs = base.filter((r) => tier === "all" || r.tier === tier);
    const good = ok(runs);
    const cost = runs.map((r) => r.cost_usd);
    const total = good.map((r) => r.seconds.total);
    rows.push([
      tier === "all" ? "**全部**" : TIER_LABEL[tier],
      `${new Set(runs.map((r) => r.pr)).size} × 3 = ${runs.length}`,
      `${pct(runs.map((r) => r.changed_lines), 0.5).toFixed(0)}`,
      usd(pct(cost, 0.5)),
      usd(pct(cost, 0.95)),
      usd(mean(cost)),
      sec(pct(total, 0.5)),
      sec(pct(total, 0.95)),
      mean(runs.map((r) => r.turns)).toFixed(1),
      `${runs.length - good.length}`,
    ]);
  }
  out.push(table(["档位", "PR × 次数", "改动行数 P50", "成本 P50", "成本 P95", "成本均值", "端到端 P50", "端到端 P95", "平均轮数", "失败"], rows), "");

  out.push("### 基线：时间和 token 花在哪里", "");
  const phaseRows = [];
  for (const tier of TIERS) {
    const runs = ok(base.filter((r) => r.tier === tier));
    const t = (f) => mean(runs.map(f));
    const input = t((r) => r.tokens.input);
    const cacheRead = t((r) => r.tokens.cache_read);
    const output = t((r) => r.tokens.output);
    const p = PRICES["deepseek/deepseek-flash"];
    const outputShare = mean(runs.map((r) => (r.tokens.output * p.output) / 1e6 / r.cost_usd));
    phaseRows.push([
      TIER_LABEL[tier],
      sec(t((r) => r.seconds.fetch + r.seconds.clone)),
      sec(t((r) => r.seconds.review)),
      t((r) => r.tool_calls).toFixed(1),
      k(input),
      k(cacheRead),
      share(cacheRead / (cacheRead + input)),
      k(output),
      share(outputShare),
      usd(mean(runs.map(noCacheCost))),
      usd(t((r) => r.cost_usd)),
    ]);
  }
  out.push(
    table(["档位", "拉取 + 克隆", "评审（模型）", "工具调用", "未缓存输入", "缓存读取", "缓存命中率", "输出", "输出占成本", "无缓存时成本", "实际成本"], phaseRows),
    "",
  );

  out.push("### 基线：同一 PR 跑 3 次的稳定性", "");
  const groups = Object.values(byPR(base));
  const unanimous = groups.filter((g) => new Set(g.map((r) => r.verdict)).size === 1).length;
  const cv = groups.map((g) => {
    const c = g.map((r) => r.cost_usd);
    const m = mean(c);
    return Math.sqrt(mean(c.map((x) => (x - m) ** 2))) / m;
  });
  out.push(
    `- ${groups.length} 个 PR 中 ${unanimous} 个三次结论一致。`,
    `- 同一 PR 三次成本的变异系数：中位数 ${share(pct(cv, 0.5))}，最大 ${share(Math.max(...cv))}。`,
    "",
  );
  const split = groups.filter((g) => new Set(g.map((r) => r.verdict)).size > 1);
  if (split.length) {
    out.push(table(["PR", "改动行数", "三次结论"], split.map((g) => [g[0].pr, g[0].changed_lines, g.map((r) => r.verdict ?? "失败").join(" / ")])), "");
  }
}

const others = Object.keys(variants).filter((v) => v !== "baseline");
if (base.length && others.length) {
  out.push("### 优化对比（同一批 20 个 PR）", "");
  const baseMajority = Object.fromEntries(Object.entries(byPR(base)).map(([pr, runs]) => [pr, majority(runs)]));
  const rows = [];
  for (const name of ["baseline", ...others.sort()]) {
    const runs = variants[name];
    const good = ok(runs);
    const cost = runs.map((r) => r.cost_usd);
    const agree = good.filter((r) => r.verdict === baseMajority[r.pr]).length;
    rows.push([
      name === "baseline" ? "基线（每 PR 3 次）" : name,
      `${runs.length}`,
      usd(mean(cost)),
      usd(pct(cost, 0.5)),
      usd(pct(cost, 0.95)),
      sec(pct(good.map((r) => r.seconds.total), 0.5)),
      sec(pct(good.map((r) => r.seconds.total), 0.95)),
      mean(runs.map((r) => r.turns)).toFixed(1),
      `${agree}/${good.length}`,
      (good.reduce((n, r) => n + serious(r), 0) / good.length).toFixed(2),
      `${runs.filter((r) => r.nudged).length}`,
      `${runs.length - good.length}`,
    ]);
  }
  out.push(
    table(["方案", "次数", "成本均值", "成本 P50", "成本 P95", "端到端 P50", "端到端 P95", "平均轮数", "与基线多数结论一致", "每次 critical+high 评论", "触发预算提示", "失败"], rows),
    "",
  );
}

console.log(out.join("\n"));
