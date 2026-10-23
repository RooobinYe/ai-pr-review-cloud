// Custom tools given to the review agent.
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { defineTool } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { ReviewSubmissionSchema, type ReviewComment, type ReviewSubmission } from "./schema.ts";

const run = promisify(execFile);

const GIT_SUBCOMMANDS = new Set(["log", "show", "diff", "blame", "ls-files", "rev-parse", "shortlog", "grep"]);
// Options that write files, run external programs or read outside the repository.
const GIT_FORBIDDEN = /^(--output|--ext-diff|--textconv|--exec|--upload-pack|--open-files-in-pager|-O|--no-index)/;
const GIT_OUTPUT_LIMIT = 50_000;

/** Returns why a git invocation is not allowed, or undefined if it is. */
export function checkGitArgs(args: string[]): string | undefined {
  const [sub, ...rest] = args;
  if (!sub || !GIT_SUBCOMMANDS.has(sub)) {
    return `subcommand must be one of: ${[...GIT_SUBCOMMANDS].join(", ")}`;
  }
  const bad = rest.find((a) => GIT_FORBIDDEN.test(a));
  return bad ? `option ${bad} is not allowed` : undefined;
}

/**
 * Read-only git without a shell. Local mode runs on a developer machine, so the
 * agent gets no bash: a prompt-injected PR could otherwise run arbitrary commands.
 */
export function gitTool(cwd: string) {
  return defineTool({
    name: "git",
    label: "git",
    description:
      "Run a read-only git command in the repository, e.g. [\"log\", \"--oneline\", \"-10\"] or [\"blame\", \"-L\", \"10,20\", \"src/a.ts\"]. Allowed subcommands: " +
      [...GIT_SUBCOMMANDS].join(", ") +
      ".",
    parameters: Type.Object({
      args: Type.Array(Type.String(), { minItems: 1, description: "Arguments after `git`, starting with the subcommand." }),
    }),
    async execute(_id, { args }) {
      const denied = checkGitArgs(args);
      if (denied) throw new Error(denied);
      try {
        const { stdout } = await run("git", ["--no-pager", ...args], {
          cwd,
          timeout: 60_000,
          maxBuffer: 32 << 20,
          env: { ...process.env, GIT_PAGER: "cat", GIT_TERMINAL_PROMPT: "0", GIT_CONFIG_NOSYSTEM: "1" },
        });
        const text =
          stdout.length > GIT_OUTPUT_LIMIT
            ? `${stdout.slice(0, GIT_OUTPUT_LIMIT)}\n[output truncated at ${GIT_OUTPUT_LIMIT} characters]`
            : stdout || "(no output)";
        return { content: [{ type: "text", text }], details: undefined };
      } catch (err) {
        const e = err as { stderr?: string; message: string };
        throw new Error((e.stderr || e.message).slice(0, 2000));
      }
    },
  });
}

export function normalizePath(path: string): string {
  return path.replace(/^\.\//, "").replace(/^b\//, "");
}

/** Lists comments whose anchor is not an added or context line of the diff. */
export function invalidAnchors(comments: ReviewComment[], commentable: Map<string, Set<number>>): string[] {
  return comments.flatMap((c, i) => {
    const lines = commentable.get(normalizePath(c.path));
    if (!lines) return [`comments[${i}]: ${c.path} is not a changed file in this PR`];
    if (!lines.has(c.line)) return [`comments[${i}]: ${c.path}:${c.line} is not an added or context line of the diff`];
    return [];
  });
}

/**
 * The tool through which the model hands in its decision. Invalid anchors are
 * rejected so the model can fix them; a valid submission ends the agent run.
 */
export function submitReviewTool(commentable: Map<string, Set<number>>, onSubmit: (s: ReviewSubmission) => void) {
  let done = false;
  return defineTool({
    name: "submit_review",
    label: "Submit review",
    description:
      "Submit your final review: the verdict, the Markdown body and inline comments. Call exactly once, after you have finished exploring.",
    parameters: ReviewSubmissionSchema,
    async execute(_id, params) {
      if (done) throw new Error("The review was already submitted.");
      const problems = invalidAnchors(params.comments, commentable);
      if (problems.length) {
        throw new Error(
          `Review not submitted. Fix these comment anchors and call submit_review again (anchor to a changed line, or move the point into the body):\n${problems.join("\n")}`,
        );
      }
      done = true;
      onSubmit({ ...params, comments: params.comments.map((c) => ({ ...c, path: normalizePath(c.path) })) });
      return { content: [{ type: "text", text: "Review submitted." }], details: undefined, terminate: true };
    },
  });
}
