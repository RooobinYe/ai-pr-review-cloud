// Browser view of the live demo (docs/demo.md). Runs `demo/demo.sh run <pr>`
// when the page's start button is pressed, parses each output line into an
// event, and streams the events to the page. No dependencies.
//
//   node demo/console.mjs <pr> [--port 8787] [--kill-after 25]
//   node demo/console.mjs --replay <saved run output> [--speed 4]
//
// --replay plays back a saved `demo.sh run` output on its own timestamps, for
// rehearsing the talk without the cluster or model spend.
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

const TOOLS = new Set(["read", "grep", "find", "ls", "git", "kubectl", "submit_review"]);

/** Parses one line of `demo.sh run` output; null for blank lines. */
export function parseLine(line) {
  const m = /^\[(\d+):(\d{2})\] (.*)$/.exec(line);
  if (!m) return parseSummary(line);
  const t = Number(m[1]) * 60 + Number(m[2]);
  const text = m[3];
  let x;
  if ((x = /^PR #(\d+) marked ready for review: (\S+)$/.exec(text))) return { t, type: "ready", pr: Number(x[1]), url: x[2] };
  if ((x = /^run (\w+) (\w+) \(attempt (\d+)\)$/.exec(text))) return { t, type: "state", run: x[1], state: x[2], attempt: Number(x[3]) };
  if ((x = /^runner Job (\S+)$/.exec(text))) return { t, type: "job", job: x[1] };
  if ((x = /^chaos: deleted the runner Pod of attempt (\d+)$/.exec(text))) return { t, type: "chaos", attempt: Number(x[1]) };
  if ((x = /^preview (\S+): (\S+) ready=(\S+) restarts=(\S+)$/.exec(text))) {
    return { t, type: "pod", namespace: x[1], pod: x[2], ready: x[3] === "true", restarts: Number(x[4]) || 0 };
  }
  if (text.startsWith("no run after a minute")) return { t, type: "warn", text };
  if ((x = /^ {2}agent {2}(.*)$/.exec(text))) return { t, ...parseAgent(x[1]) };
  return { t, type: "log", text };
}

// Runner log lines: tool calls are indented by two spaces, their failures by four.
function parseAgent(s) {
  let x;
  if ((x = /^ {4}(\w+) failed(?:: (.*))?$/.exec(s)) && TOOLS.has(x[1])) return { type: "toolFailed", tool: x[1], text: x[2] ?? "" };
  if ((x = /^ {2}(\w+)(?: (.*))?$/.exec(s)) && TOOLS.has(x[1])) return { type: "tool", tool: x[1], text: x[2] ?? "" };
  if ((x = /^ {2}resuming after turn (\d+) /.exec(s))) return { type: "resumed", turn: Number(x[1]) };
  if ((x = /^run \S+: resuming from a snapshot with (\d+) entries$/.exec(s))) return { type: "snapshot", entries: Number(x[1]) };
  if ((x = /^run \S+: posted (\w+) with (\d+) comments$/.exec(s))) return { type: "posted", verdict: x[1], comments: Number(x[2]) };
  if ((x = /^reviewing \S+ at \w+ with (\S+)/.exec(s))) return { type: "reviewing", model: x[1], text: s };
  if (s === "terminated") return { type: "terminated" };
  if (s.startsWith("deploying preview")) return { type: "deploying" };
  if ((x = /^preview (\S+): (.*)$/.exec(s))) return { type: "previewStatus", namespace: x[1], status: x[2] };
  return { type: "agentLog", text: s.trim() };
}

// The summary `demo.sh run` prints after the run ends has no timestamps.
function parseSummary(line) {
  let x;
  if ((x = /^verdict (\S+), attempts (\d+)(?:, (?:retried after|error): (.*))?$/.exec(line))) {
    return { type: "verdict", verdict: x[1], attempts: Number(x[2]), error: x[3] ?? "" };
  }
  if ((x = /^phases\s+(.*)$/.exec(line))) {
    return { type: "phases", phases: Object.fromEntries([...x[1].matchAll(/(\w+) (\d+)s/g)].map((p) => [p[1], Number(p[2])])) };
  }
  if ((x = /^cost\s+\$([\d.]+) over (\d+) turns/.exec(line))) return { type: "cost", cost: Number(x[1]), turns: Number(x[2]) };
  if ((x = /^comment (.+?):(\d*) \[(\w+)\] (.*)$/.exec(line))) return { type: "comment", path: x[1], line: x[2], severity: x[3], text: x[4] };
  if ((x = /^review\s+(\S+)$/.exec(line))) return { type: "review", url: x[1] };
  return line.trim() ? { type: "log", text: line } : null;
}

function main() {
  const { values, positionals } = parseArgs({
    allowPositionals: true,
    options: {
      port: { type: "string", default: "8787" },
      "kill-after": { type: "string", default: "25" },
      replay: { type: "string" },
      speed: { type: "string", default: "1" },
    },
  });
  const pr = positionals[0];
  if (!values.replay && !/^\d+$/.test(pr ?? "")) {
    console.error("usage: node demo/console.mjs <pr> [--port 8787] [--kill-after 25]\n       node demo/console.mjs --replay <file> [--speed 4]");
    process.exit(2);
  }
  const port = Number(values.port);
  const speed = Number(values.speed) || 1;
  const root = dirname(dirname(fileURLToPath(import.meta.url)));
  const info = { pr: pr ?? null, killAfter: values["kill-after"], replay: Boolean(values.replay), speed };
  const history = [];
  const clients = new Set();
  let child = null;
  let started = false;

  const send = (msg) => {
    history.push(msg);
    for (const res of clients) res.write(`data: ${JSON.stringify(msg)}\n\n`);
  };
  const emit = (raw) => send({ kind: "line", raw, ev: parseLine(raw) });

  function start(chaos) {
    started = true;
    send({ kind: "started", at: Date.now(), chaos: !info.replay && chaos });
    if (info.replay) {
      let t = 0;
      const lines = readFileSync(values.replay, "utf8").split("\n");
      for (const raw of lines) {
        t = parseLine(raw)?.t ?? t;
        setTimeout(() => emit(raw), (t * 1000) / speed);
      }
      setTimeout(() => send({ kind: "exit", at: Date.now(), code: 0 }), (t * 1000) / speed + 100);
      return;
    }
    child = spawn(join(root, "demo/demo.sh"), ["run", pr], {
      cwd: root,
      env: { ...process.env, KILL_AFTER: chaos ? info.killAfter : "" },
    });
    for (const stream of [child.stdout, child.stderr]) createInterface({ input: stream }).on("line", emit);
    child.on("exit", (code) => {
      child = null;
      send({ kind: "exit", at: Date.now(), code });
    });
  }

  // Only this machine's browser may drive the demo: check Host and Origin so a
  // web page elsewhere cannot start a run through the browser.
  const allowed = new Set([`localhost:${port}`, `127.0.0.1:${port}`]);
  const server = createServer((req, res) => {
    const origin = req.headers.origin;
    if (!allowed.has(req.headers.host ?? "") || (origin && !allowed.has(origin.replace(/^http:\/\//, "")))) {
      res.writeHead(403).end();
    } else if (req.method === "GET" && req.url === "/") {
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" }).end(PAGE);
    } else if (req.method === "GET" && req.url === "/events") {
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
      res.write(`data: ${JSON.stringify({ kind: "hello", ...info })}\n\n`);
      for (const msg of history) res.write(`data: ${JSON.stringify(msg)}\n\n`);
      clients.add(res);
      req.on("close", () => clients.delete(res));
    } else if (req.method === "POST" && req.url?.startsWith("/start")) {
      if (started) return void res.writeHead(409).end();
      start(new URL(req.url, "http://localhost").searchParams.get("chaos") === "1");
      res.writeHead(204).end();
    } else {
      res.writeHead(404).end();
    }
  });
  server.listen(port, "127.0.0.1", () => console.log(`demo console: http://localhost:${port}`));

  process.on("SIGINT", () => {
    if (!child) process.exit(130);
    child.kill("SIGINT");
    child.on("exit", () => process.exit(130));
  });
}

const PAGE = String.raw`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>AI PR 评审演示</title>
<style>
:root{--bg:#f6f7f9;--panel:#fff;--line:#dfe3e8;--text:#14181f;--muted:#5d6673;--accent:#2357d9;--ok:#13804b;--warn:#b26200;--bad:#c62828;--soft:#eef2fb}
@media (prefers-color-scheme:dark){:root{--bg:#0f1218;--panel:#171b23;--line:#2a313d;--text:#e8ecf2;--muted:#98a2b3;--accent:#6d9bff;--ok:#3ecf8e;--warn:#f0a542;--bad:#ff6b6b;--soft:#1d2533}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:16px/1.5 -apple-system,"PingFang SC","Noto Sans SC",system-ui,sans-serif}
main{max-width:1400px;margin:0 auto;padding:20px 16px 40px;display:grid;gap:16px}
header{display:flex;flex-wrap:wrap;align-items:center;gap:16px}h1{font-size:22px;margin:0;flex:1;min-width:220px}h1 a{color:var(--accent);text-decoration:none}
#model{display:block;font-size:14px;font-weight:400;color:var(--muted)}
#timer{font:600 56px/1 ui-monospace,"SF Mono",Menlo,monospace;font-variant-numeric:tabular-nums}
button,.btn{font:inherit;font-weight:600;padding:10px 20px;border-radius:8px;border:0;background:var(--accent);color:#fff;cursor:pointer;text-decoration:none;display:inline-block}
button:disabled{opacity:.4;cursor:default}label{color:var(--muted);user-select:none}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:16px}.panel h2{margin:0 0 12px;font-size:15px;color:var(--muted);font-weight:600}
#flow{display:grid;grid-template-columns:repeat(6,1fr);gap:8px}
.step{padding:12px;border-radius:10px;background:var(--soft);border:2px solid transparent;color:var(--muted)}
.step b{display:block;font-size:18px}.step span{font:13px ui-monospace,Menlo,monospace}
.step.done{color:var(--text)}.step.done b::before{content:"✓ ";color:var(--ok)}
.step.now{border-color:var(--accent);color:var(--text);animation:pulse 1.6s ease-in-out infinite}
@keyframes pulse{50%{box-shadow:0 0 0 6px color-mix(in srgb,var(--accent) 20%,transparent)}}
.grid{display:grid;grid-template-columns:3fr 2fr;gap:16px;align-items:start}.side{display:grid;gap:16px}
#stream{height:62vh;overflow-y:auto;font:14px/1.6 ui-monospace,"SF Mono",Menlo,monospace}
.row{display:flex;gap:10px;padding:3px 0;align-items:baseline}.row time{color:var(--muted);flex:none}
.tag{flex:none;min-width:84px;text-align:center;border-radius:5px;padding:0 6px;font-size:12px;font-weight:700;color:#fff}
.t-read,.t-ls,.t-find{background:#4b6cb7}.t-grep{background:#7b4bb7}.t-git{background:#c2571a}.t-kubectl{background:#1f8a8a}.t-submit_review{background:var(--ok)}
.row.sys{color:var(--muted)}.row.err{color:var(--bad)}.row.chaos{color:var(--bad);font-weight:700}
#fault{border-color:var(--bad);display:none}#fault.ok{border-color:var(--ok)}#fault h2{color:var(--bad)}#fault.ok h2{color:var(--ok)}
#fault ol{margin:0;padding-left:20px}
.kv{display:grid;grid-template-columns:auto 1fr;gap:4px 12px;font-size:15px}.kv dt{color:var(--muted)}.kv dd{margin:0;word-break:break-all}
.badge{font-weight:700}.yes{color:var(--ok)}.no{color:var(--warn)}
#verdict{font-size:28px;font-weight:700;margin-bottom:8px}#verdict .note{font-size:14px;font-weight:400;color:var(--muted)}.v-approve{color:var(--ok)}.v-request_changes{color:var(--warn)}.v-comment{color:var(--accent)}
.comment{border-left:3px solid var(--warn);padding:4px 10px;margin:8px 0;font-size:14px}.comment code{color:var(--muted)}
#phases{display:flex;height:22px;border-radius:5px;overflow:hidden;margin:10px 0 4px}#phases div{min-width:2px}
#plegend{font-size:13px;color:var(--muted);display:flex;flex-wrap:wrap;gap:4px 12px}
#warn{display:none;background:var(--warn);color:#fff;padding:10px 16px;border-radius:8px}
details{color:var(--muted)}pre{white-space:pre-wrap;font-size:12px}
@media (max-width:900px){.grid{grid-template-columns:1fr}#flow{grid-template-columns:repeat(3,1fr)}#timer{font-size:40px}#stream{height:50vh}}
</style></head><body><main>
<header><h1>AI PR 评审 · 现场演示 <a id="prlink" target="_blank"></a><span id="model"></span></h1>
<label id="chaosbox"><input type="checkbox" id="chaos" checked> 第 <span id="killafter"></span> 秒删除 runner Pod</label>
<button id="start" disabled>开始</button><div id="timer">0:00</div></header>
<div id="warn"></div>
<div id="flow"></div>
<div class="grid"><section class="panel"><h2>agent 活动</h2><div id="stream"></div></section>
<div class="side">
<section class="panel" id="fault"><h2>故障注入</h2><ol id="faultlog"></ol></section>
<section class="panel"><h2>预览环境</h2><dl class="kv" id="preview"><dt>状态</dt><dd>尚未部署</dd></dl></section>
<section class="panel" id="result"><h2>评审结果</h2><div id="verdict" class="muted">等待投票</div><div id="comments"></div>
<div id="phases"></div><div id="plegend"></div><dl class="kv" id="cost"></dl><p id="links"></p></section>
</div></div>
<details><summary>原始输出</summary><pre id="raw"></pre></details>
</main><script>
const $ = (id) => document.getElementById(id);
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
const fmt = (s) => Math.floor(s / 60) + ":" + String(Math.floor(s % 60)).padStart(2, "0");
const STAGES = [["webhook", "Webhook"], ["queue", "排队"], ["review", "评审代码"], ["preview", "部署预览"], ["evidence", "kubectl 取证"], ["vote", "投票"]];
const PHASES = { queue: ["排队", "#8a94a6"], startup: ["建 Pod", "#4b6cb7"], fetch: ["拉代码", "#7b4bb7"], review: ["评审", "#1f8a8a"], publish: ["投票", "#13804b"] };
const VERDICTS = { approve: "批准", request_changes: "需要修改", comment: "仅评论" };
const S = { stage: -1, times: [], startedAt: 0, speed: 1, frozen: null, previewSeen: false, chaos: false, attempt: 1, pod: {} };
for (const [, label] of STAGES) { const d = el("div", "step"); d.append(el("b", "", label), el("span", "", "–")); $("flow").append(d); }

function advance(name, t) {
  const i = STAGES.findIndex(([n]) => n === name);
  if (i <= S.stage) return;
  for (let k = S.stage + 1; k <= i; k++) S.times[k] ??= t;
  S.stage = i; renderFlow();
}
function renderFlow() {
  [...$("flow").children].forEach((d, i) => {
    d.className = "step" + (i < S.stage || (S.frozen != null && i <= S.stage) ? " done" : i === S.stage ? " now" : "");
    d.lastChild.textContent = S.times[i] != null ? fmt(S.times[i]) : "–";
  });
}
function row(t, cls, tag, text) {
  const r = el("div", "row " + (cls || ""));
  r.append(el("time", "", t != null ? fmt(t) : "     "));
  if (tag) r.append(el("span", "tag t-" + tag, tag === "submit_review" ? "submit" : tag));
  r.append(el("span", "", text));
  const box = $("stream"), stick = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  box.append(r); if (stick) box.scrollTop = box.scrollHeight;
}
function fault(text, ok) { $("fault").style.display = "block"; if (ok) $("fault").className = "panel ok"; $("faultlog").append(el("li", "", text)); }
function finish(t) { S.frozen = t; $("timer").textContent = fmt(t); renderFlow(); }
function kv(box, pairs) { box.replaceChildren(); for (const [k, v, cls] of pairs) box.append(el("dt", "", k), el("dd", cls || "", v)); }
function renderPreview() {
  const p = S.pod, pairs = [];
  if (p.namespace) pairs.push(["namespace", p.namespace]);
  if (p.pod) pairs.push(["Pod", p.pod], ["就绪", p.ready ? "Ready" : "未就绪", "badge " + (p.ready ? "yes" : "no")], ["重启次数", String(p.restarts), "badge " + (p.restarts ? "no" : "yes")]);
  pairs.push(["状态", p.status || "尚未部署"]);
  kv($("preview"), pairs);
}

function apply(ev, t) {
  switch (ev.type) {
    case "ready": $("prlink").textContent = "#" + ev.pr; $("prlink").href = ev.url; row(t, "sys", "", "PR 标为待评审，GitHub 发出 webhook"); break;
    case "state":
      row(t, "sys", "", "run " + ev.run + " → " + ev.state + "（第 " + ev.attempt + " 次尝试）");
      if (ev.state === "queued") advance("queue", t);
      else if (ev.state === "running") advance(S.previewSeen ? "evidence" : "review", t);
      else if (ev.state === "previewing") advance("preview", t);
      else if (ev.state === "reviewed") advance("vote", t);
      else if (ev.state === "done" || ev.state === "awaiting_approval") { advance("vote", t); finish(t); }
      else if (ev.state === "failed" || ev.state === "superseded") { $("verdict").textContent = "run " + ev.state; finish(t); }
      if (S.chaos && ev.state === "running" && ev.attempt > S.attempt) fault("第 " + ev.attempt + " 次尝试：controller 重建 runner Pod（" + fmt(t) + "）");
      S.attempt = Math.max(S.attempt, ev.attempt);
      break;
    case "job": row(t, "sys", "", "为这个 PR 创建独立的 runner Job"); break;
    case "chaos": S.chaos = true; row(t, "chaos", "", "⚡ 模拟节点故障：删除第 " + ev.attempt + " 次尝试的 runner Pod"); fault("删除 runner Pod，模拟节点故障（" + fmt(t) + "）"); break;
    case "terminated": row(t, "err", "", "runner 收到 SIGTERM 退出"); break;
    case "snapshot": fault("读取上次尝试的 session 快照（" + ev.entries + " 条记录）"); break;
    case "resumed": row(t, "sys", "", "从第 " + ev.turn + " 轮快照续跑"); fault("从第 " + ev.turn + " 轮快照续跑，不从头再来", true); break;
    case "reviewing": $("model").textContent = ev.model; row(t, "sys", "", ev.text); break;
    case "deploying": advance("preview", t); S.pod.status = "部署中，等待就绪或超时"; renderPreview(); row(t, "sys", "", "部署预览环境，等待 Ready 或超时"); break;
    case "previewStatus": S.previewSeen = true; advance("evidence", t); S.pod.namespace = ev.namespace; S.pod.status = ev.status; renderPreview(); row(t, "sys", "", "预览：" + ev.status); break;
    case "pod": Object.assign(S.pod, ev); renderPreview(); break;
    case "tool": if (ev.tool === "kubectl") advance("evidence", t); row(t, "", ev.tool, ev.text); break;
    case "toolFailed": row(t, "err", "", "↳ " + ev.tool + " 失败：" + ev.text); break;
    case "posted": advance("vote", t); row(t, "sys", "", "提交评审：" + ev.verdict + "，" + ev.comments + " 条逐行评论"); break;
    case "warn": $("warn").style.display = "block"; $("warn").textContent = ev.text; break;
    case "verdict":
      $("verdict").textContent = VERDICTS[ev.verdict] || ev.verdict; $("verdict").className = "v-" + ev.verdict;
      $("verdict").append(el("div", "note", ev.verdict + (ev.attempts > 1 ? " · 共 " + ev.attempts + " 次尝试" : "")));
      $("result").scrollIntoView({ block: "nearest", behavior: "smooth" });
      break;
    case "comment": {
      // demo.sh cuts each comment's first line at 110 characters.
      const c = el("div", "comment"); c.append(el("code", "", ev.path + ":" + ev.line + " [" + ev.severity + "]"), el("div", "", ev.text + (ev.text.length >= 110 ? "…" : "")));
      $("comments").append(c); break;
    }
    case "phases": {
      const total = Object.keys(PHASES).reduce((sum, k) => sum + (ev.phases[k] ?? 0), 0) || 1;
      for (const [k, [label, color]] of Object.entries(PHASES)) {
        if (!(k in ev.phases)) continue;
        const bar = el("div"); bar.style.cssText = "flex:" + ev.phases[k] / total + ";background:" + color; $("phases").append(bar);
        const l = el("span", "", "■ " + label + " " + ev.phases[k] + "s"); l.style.color = color; $("plegend").append(l);
      }
      $("plegend").append(el("span", "", "合计 " + fmt(ev.phases.total ?? total)));
      break;
    }
    case "cost": kv($("cost"), [["花费", "$" + ev.cost.toFixed(4)], ["模型轮数", String(ev.turns)]]); break;
    case "review": { const a = el("a", "btn", "在 GitHub 查看评审 →"); a.href = ev.url; a.target = "_blank"; $("links").append(a); break; }
    case "log": case "agentLog": row(t, "sys", "", ev.text); break;
  }
}

let lastT = 0, connected = false;
const es = new EventSource("/events");
es.onmessage = (e) => {
  const m = JSON.parse(e.data);
  if (m.kind === "hello") {
    // A reconnect replays the whole history; start from a clean page.
    if (connected) return location.reload();
    connected = true;
    S.speed = m.speed; $("killafter").textContent = m.killAfter;
    if (m.replay) $("chaosbox").style.display = "none";
    if (m.pr) { $("prlink").textContent = "#" + m.pr; }
    $("start").disabled = false;
  } else if (m.kind === "started") {
    S.startedAt = m.at; $("start").disabled = true; $("chaos").disabled = true; advance("webhook", 0);
  } else if (m.kind === "line") {
    $("raw").textContent += m.raw + "\n";
    if (m.ev) { lastT = m.ev.t ?? lastT; apply(m.ev, m.ev.t); }
  } else if (m.kind === "exit") {
    if (S.frozen == null) finish(lastT);
    if (m.code) { $("warn").style.display = "block"; $("warn").textContent = "demo.sh 退出码 " + m.code + "，详情见原始输出"; }
  }
};
es.onerror = () => { $("start").disabled = true; };
$("start").onclick = () => fetch("/start?chaos=" + ($("chaos").checked ? 1 : 0), { method: "POST" });
setInterval(() => { if (S.startedAt && S.frozen == null) $("timer").textContent = fmt(((Date.now() - S.startedAt) / 1000) * S.speed); }, 250);
</script></body></html>`;

if (process.argv[1] === fileURLToPath(import.meta.url)) main();
