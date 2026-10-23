// The review pipeline shared by local and cluster mode (docs/architecture.md §4.2).
import {
  createAgentSession,
  createExtensionRuntime,
  type FileEntry,
  type ModelRuntime,
  type ResourceLoader,
  type SessionStats,
  SessionManager,
  SettingsManager,
} from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { commentableLines } from "./diff.ts";
import type { PiModel, ThinkingLevel } from "./model.ts";
import type { PRContext } from "./pr.ts";
import { RESUME_PROMPT, SUBMIT_REMINDER, systemPrompt, userPrompt } from "./prompts.ts";
import { ReviewSubmissionSchema, type ReviewSubmission } from "./schema.ts";
import { gitTool, invalidAnchors, submitReviewTool } from "./tools.ts";

/** Reminders sent when the model stops without calling submit_review. */
export const MAX_SUBMIT_REMINDERS = 2;

export interface ReviewOptions {
  pr: PRContext;
  /** Checked-out PR head. */
  cwd: string;
  /** Scratch directory for pi's own state; nothing is read from ~/.pi. */
  agentDir: string;
  model: PiModel;
  modelRuntime: ModelRuntime;
  thinkingLevel: ThinkingLevel;
  language: string;
  onProgress?: (line: string) => void;
  /** Session entries of an earlier attempt to continue from (docs/architecture.md §4.3). */
  resumeFrom?: FileEntry[];
  /**
   * Called after every turn with the session so far. Calls run one at a time
   * in turn order; a failing call is reported and does not stop the review.
   */
  onTurn?: (snapshot: TurnSnapshot) => Promise<void>;
}

/** The state after one completed turn: what a later attempt resumes from. */
export interface TurnSnapshot {
  /** Assistant turns in the session so far, counting resumed ones. */
  turn: number;
  /** The session as JSONL: the header line, then one line per entry. */
  jsonl: string;
  /** `provider/model` that answered this turn. */
  model: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  cost: number;
}

export interface ReviewOutcome {
  submission: ReviewSubmission;
  stats: SessionStats;
}

/** A review that ended without a submission; carries the usage spent on it. */
export class ReviewFailedError extends Error {
  readonly stats: SessionStats;
  constructor(message: string, stats: SessionStats) {
    super(message);
    this.stats = stats;
  }
}

/** The model answered but never called submit_review. Retrying would not help. */
export class ReviewNotSubmittedError extends ReviewFailedError {
  constructor(stats: SessionStats) {
    super(`model did not call submit_review after ${MAX_SUBMIT_REMINDERS} reminders`, stats);
  }
}

/** The model request failed after pi's own retries, e.g. rate limits or an exhausted balance. */
export class ModelRequestError extends ReviewFailedError {}

/**
 * A resource loader that discovers nothing. The default one would load
 * extensions, skills and AGENTS.md from the reviewed repository, which is
 * untrusted input and must not run code or steer the system prompt.
 */
function isolatedResources(system: string): ResourceLoader {
  return {
    getExtensions: () => ({ extensions: [], errors: [], runtime: createExtensionRuntime() }),
    getSkills: () => ({ skills: [], diagnostics: [] }),
    getPrompts: () => ({ prompts: [], diagnostics: [] }),
    getThemes: () => ({ themes: [], diagnostics: [] }),
    getAgentsFiles: () => ({ agentsFiles: [] }),
    getSystemPrompt: () => system,
    getSystemPromptSource: () => undefined,
    getAppendSystemPrompt: () => [],
    getAppendSystemPromptSources: () => [],
    extendResources: () => {},
    reload: async () => {},
  };
}

function describeToolCall(name: string, args: Record<string, unknown>): string {
  if (name === "git" && Array.isArray(args.args)) return `git ${args.args.join(" ")}`;
  const target = args.pattern ?? args.path ?? "";
  return `${name} ${String(target)}`.trim();
}

type Entry = Record<string, unknown>;

/**
 * Finds a submit_review call that an earlier attempt made and the tool
 * accepted. The attempt crashed before reporting it, so it can be reported
 * now instead of reviewing again.
 */
export function acceptedSubmission(entries: FileEntry[], commentable: Map<string, Set<number>>): ReviewSubmission | undefined {
  const messages = (entries as unknown as Entry[]).filter((e) => e.type === "message").map((e) => e.message as Entry);
  const accepted = new Set(
    messages.filter((m) => m.role === "toolResult" && m.toolName === "submit_review" && m.isError === false).map((m) => m.toolCallId),
  );
  for (const m of messages.reverse()) {
    if (m.role !== "assistant" || !Array.isArray(m.content)) continue;
    for (const c of m.content as Entry[]) {
      if (c.type !== "toolCall" || c.name !== "submit_review" || !accepted.has(c.id)) continue;
      const args = c.arguments;
      if (Value.Check(ReviewSubmissionSchema, args) && invalidAnchors(args.comments, commentable).length === 0) return args;
    }
  }
  return undefined;
}

/** Serializes the session the way pi writes session files. */
function sessionJSONL(sm: SessionManager): string {
  const header = sm.getHeader();
  return [...(header ? [header] : []), ...sm.getEntries()].map((e) => JSON.stringify(e)).join("\n") + "\n";
}

/**
 * Returns the error of the last model response, if it failed. A failed
 * request is not the model declining to submit, so it must not use up the
 * reminders or count as a final failure.
 */
function lastRequestError(sm: SessionManager): string | undefined {
  const entries = sm.getEntries();
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]!;
    if (e.type !== "message" || e.message.role !== "assistant") continue;
    return e.message.stopReason === "error" ? e.message.errorMessage || "model request failed" : undefined;
  }
  return undefined;
}

/** Counts assistant turns; stable across attempts because resumed turns are kept. */
function turns(sm: SessionManager): number {
  return sm.getEntries().filter((e) => e.type === "message" && e.message.role === "assistant").length;
}

function snapshotOf(sm: SessionManager, message: unknown): TurnSnapshot {
  const m = message as { provider?: string; model?: string; usage?: { input: number; output: number; cacheRead: number; cacheWrite: number; cost?: { total: number } } };
  const u = m.usage;
  return {
    turn: turns(sm),
    jsonl: sessionJSONL(sm),
    model: `${m.provider ?? "unknown"}/${m.model ?? "unknown"}`,
    input: u?.input ?? 0,
    output: u?.output ?? 0,
    cacheRead: u?.cacheRead ?? 0,
    cacheWrite: u?.cacheWrite ?? 0,
    cost: u?.cost?.total ?? 0,
  };
}

export async function review(opts: ReviewOptions): Promise<ReviewOutcome> {
  const commentable = commentableLines(opts.pr.diff);
  const resumed = opts.resumeFrom?.length ? opts.resumeFrom : undefined;
  let submission: ReviewSubmission | undefined = resumed && acceptedSubmission(resumed, commentable);
  if (submission) opts.onProgress?.("an earlier attempt already submitted the review; reporting it");
  const submit = submitReviewTool(commentable, (s) => {
    submission = s;
  });
  const sessionManager = SessionManager.inMemory(opts.cwd, undefined, resumed);

  const { session } = await createAgentSession({
    cwd: opts.cwd,
    agentDir: opts.agentDir,
    model: opts.model,
    modelRuntime: opts.modelRuntime,
    thinkingLevel: opts.thinkingLevel,
    resourceLoader: isolatedResources(systemPrompt(opts.language)),
    sessionManager,
    settingsManager: SettingsManager.inMemory({ retry: { enabled: true, maxRetries: 3 } }),
    tools: ["read", "grep", "find", "ls", "git", "submit_review"],
    customTools: [gitTool(opts.cwd), submit],
  });

  // Snapshots are taken synchronously at turn_end, while the session matches
  // the turn, and uploaded in order behind each other.
  let uploads = Promise.resolve();
  const unsubscribe = session.subscribe((event) => {
    if (event.type === "turn_end" && opts.onTurn) {
      const snapshot = snapshotOf(sessionManager, event.message);
      const onTurn = opts.onTurn;
      uploads = uploads
        .then(() => onTurn(snapshot))
        .catch((err: unknown) => opts.onProgress?.(`saving turn ${snapshot.turn} failed: ${err instanceof Error ? err.message : String(err)}`));
    } else if (event.type === "tool_execution_start") {
      opts.onProgress?.(describeToolCall(event.toolName, event.args ?? {}));
    } else if (event.type === "tool_execution_end" && event.isError) {
      const text: unknown = event.result?.content?.[0]?.text;
      const reason = typeof text === "string" ? `: ${text.split("\n")[0]!.slice(0, 200)}` : "";
      opts.onProgress?.(`  ${event.toolName} failed${reason}`);
    }
  });

  try {
    const prompt = async (text: string) => {
      await session.prompt(text);
      const failed = !submission && lastRequestError(sessionManager);
      if (failed) {
        await uploads;
        throw new ModelRequestError(`model request failed: ${failed.slice(0, 500)}`, session.getSessionStats());
      }
    };
    if (!submission) {
      if (resumed) opts.onProgress?.(`resuming after turn ${turns(sessionManager)} of an earlier attempt`);
      await prompt(resumed ? RESUME_PROMPT : userPrompt(opts.pr));
    }
    for (let i = 0; !submission && i < MAX_SUBMIT_REMINDERS; i++) {
      opts.onProgress?.("model stopped without submitting; reminding it");
      await prompt(SUBMIT_REMINDER);
    }
    await uploads;
    const stats = session.getSessionStats();
    if (!submission) throw new ReviewNotSubmittedError(stats);
    return { submission, stats };
  } finally {
    unsubscribe();
    session.dispose();
  }
}
