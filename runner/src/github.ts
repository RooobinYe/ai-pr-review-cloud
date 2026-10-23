// Local mode only: fetch PR metadata and code directly from GitHub. In cluster
// mode the fetch init container does this and the runner never sees a token.
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { buildDiff } from "./diff.ts";
import type { ChangedFile, PRContext } from "./pr.ts";
import type { PRRef } from "./prurl.ts";

const run = promisify(execFile);
const API = "https://api.github.com";
const MAX_FILE_PAGES = 30; // GitHub lists at most 3000 files per PR.

interface GitHubPull {
  title: string;
  body: string | null;
  user: { login: string } | null;
  base: { ref: string; sha: string };
  head: { ref: string; sha: string };
}

interface GitHubFile {
  filename: string;
  status: string;
  additions: number;
  deletions: number;
  previous_filename?: string;
  patch?: string;
}

async function api<T>(path: string, token: string | undefined): Promise<T> {
  const res = await fetch(`${API}${path}`, {
    headers: {
      Accept: "application/vnd.github+json",
      "X-GitHub-Api-Version": "2022-11-28",
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
  }).catch((err: Error) => {
    // undici reports only "fetch failed"; the cause names the DNS or TLS error.
    throw new Error(`GitHub API ${path}: ${err.message}${err.cause instanceof Error ? `: ${err.cause.message}` : ""}`);
  });
  if (!res.ok) {
    throw new Error(`GitHub API ${path}: ${res.status} ${res.statusText}: ${(await res.text()).slice(0, 200)}`);
  }
  return (await res.json()) as T;
}

export async function fetchPR(ref: PRRef, token?: string): Promise<PRContext> {
  const base = `/repos/${ref.owner}/${ref.repo}/pulls/${ref.number}`;
  const pull = await api<GitHubPull>(base, token);
  const raw: GitHubFile[] = [];
  for (let page = 1; page <= MAX_FILE_PAGES; page++) {
    const batch = await api<GitHubFile[]>(`${base}/files?per_page=100&page=${page}`, token);
    raw.push(...batch);
    if (batch.length < 100) break;
  }
  const files: (ChangedFile & { patch?: string })[] = raw.map((f) => ({
    path: f.filename,
    status: f.status,
    additions: f.additions,
    deletions: f.deletions,
    ...(f.previous_filename ? { previousPath: f.previous_filename } : {}),
    ...(f.patch !== undefined ? { patch: f.patch } : {}),
  }));
  return {
    repo: `${ref.owner}/${ref.repo}`,
    number: ref.number,
    title: pull.title,
    author: pull.user?.login ?? "unknown",
    body: pull.body ?? "",
    baseRef: pull.base.ref,
    headRef: pull.head.ref,
    baseSha: pull.base.sha,
    headSha: pull.head.sha,
    files: files.map(({ patch: _patch, ...f }) => f),
    diff: buildDiff(files),
  };
}

/**
 * Checks out the PR head into dir, with recent history and the base commit
 * available for git log / git diff. Blobs are fetched lazily (partial clone).
 */
export async function clonePR(ctx: PRContext, dir: string, token?: string): Promise<void> {
  // Pass the credential through git's environment config so it never shows up in argv.
  const env: NodeJS.ProcessEnv = { ...process.env, GIT_TERMINAL_PROMPT: "0" };
  if (token) {
    const basic = Buffer.from(`x-access-token:${token}`).toString("base64");
    Object.assign(env, {
      GIT_CONFIG_COUNT: "1",
      GIT_CONFIG_KEY_0: "http.https://github.com/.extraHeader",
      GIT_CONFIG_VALUE_0: `Authorization: Basic ${basic}`,
    });
  }
  const git = (...args: string[]) => run("git", args, { cwd: dir, env, maxBuffer: 16 << 20 });
  await git("init", "-q");
  await git("remote", "add", "origin", `https://github.com/${ctx.repo}.git`);
  await git("fetch", "-q", "--filter=blob:none", "--depth=20", "origin", ctx.headSha, ctx.baseSha);
  await git("checkout", "-q", "--detach", ctx.headSha);
}
