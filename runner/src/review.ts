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
import { kubectlTool, type PreviewAccess } from "./preview.ts";
import { Value } from "typebox/value";
import { commentableLines } from "./diff.ts";
import type { PiModel, ThinkingLevel } from "./model.ts";
import type { PRContext } from "./pr.ts";
import { budgetReminder, isPreviewPrompt, previewPrompt, resumePrompt, SUBMIT_REMINDER, systemPrompt, userPrompt } from "./prompts.ts";
import { ReviewSubmissionSchema, type ReviewSubmission } from "./schema.ts";
import { gitTool, invalidAnchors, normalizePath, submitReviewTool } from "./tools.ts";
import { overNudgeThreshold, turnProblem } from "./usage.ts";

/** Reminders sent when the model stops without calling submit_review. */
export const MAX_SUBMIT_REMINDERS = 2;

export interface ReviewOptions {
  preparePreview?: () => Promise<PreviewAccess>;
  pr: PRContext;
  /** Checked-out PR head. */
  cwd: string;
  /** Scratch directory for pi's own state; nothing is read from ~/.pi. */
  agentDir: string;
  model: PiModel;
  modelRuntime: ModelRuntime;
  thinkingLevel: ThinkingLevel;
  language: string;
  /** Spending at which the model is told to submit (§7.3); none when unset. */
  budgetUSD?: number;
  onProgress?: (line: string) => void;
  /** Session entries of an earlier attempt to continue from (docs/architecture.md §4.3). */
  resumeFrom?: FileEntry[];
  /**
   * Called after every turn with the session so far. Calls run one at a time
   * in turn order; a failing call is reported and does not stop the review.
   */
  onTurn?: (snapshot: TurnSnapshot) => void | Promise<void>;
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
  /** Whether the budget reminder was sent. */
  nudged: boolean;
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

/** The model API refused a request and pi's retries did not help. */
export class ModelRequestError extends ReviewFailedError {}
export class ModelError extends ModelRequestError {}

/**
 * LiteLLM refused the run's virtual key because its budget is spent
 * (docs/architecture.md §7.3). The message starts with "budget_exceeded:",
 * which the controller reports as the run's failure reason.
 */
export class BudgetExceededError extends ModelError {}

/** Classifies the error pi recorded for a failed model request. */
export function modelError(errorMessage: string, stats: SessionStats): ModelError {
  if (!/budget_exceeded|Budget has been exceeded/i.test(errorMessage)) {
    return new ModelError(`model request failed: ${errorMessage.slice(0, 500)}`, stats);
  }
  const spent = /Current cost: ([\d.]+), Max budget: ([\d.]+)/.exec(errorMessage);
  const usd = (v: string) => `$${Number(Number(v).toFixed(4))}`;
  const detail = spent ? `spent ${usd(spent[1]!)} of its ${usd(spent[2]!)} budget` : "spent its budget";
  return new BudgetExceededError(`budget_exceeded: the model proxy refused further requests; the run ${detail}`, stats);
}

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
  if ((name === "git" || name === "kubectl") && Array.isArray(args.args)) return `${name} ${args.args.join(" ")}`;
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
      if (Value.Check(ReviewSubmissionSchema, args) && invalidAnchors(args.comments, commentable).length === 0) {
        return { ...args, comments: args.comments.map((comment) => ({ ...comment, path: normalizePath(comment.path) })) };
      }
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

/** Whether a crashed review had already reached runtime verification. */
function reachedPreviewPhase(entries: FileEntry[]): boolean {
  return (entries as unknown as Entry[]).some((e) => {
    const m = e.type === "message" ? (e.message as Entry) : undefined;
    if (m?.role !== "user") return false;
    const text = typeof m.content === "string" ? m.content : Array.isArray(m.content) ? (m.content as Entry[]).map((c) => (c.type === "text" ? c.text : "")).join("") : "";
    return isPreviewPrompt(text);
  });
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
  let previewAccess: PreviewAccess | undefined;
  const preview = !!opts.preparePreview;
  // With a preview, submit_review is only offered in phase 2 (docs/architecture.md §4.2).
  let canSubmit = !preview;
  const baseTools = ["read", "grep", "find", "ls", "git"];
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
    resourceLoader: isolatedResources(systemPrompt(opts.language, preview)),
    sessionManager,
    settingsManager: SettingsManager.inMemory({ retry: { enabled: true, maxRetries: 3 } }),
    tools: [...baseTools, "submit_review", ...(preview ? ["kubectl"] : [])],
    customTools: [gitTool(opts.cwd), submit, ...(preview ? [kubectlTool(() => previewAccess)] : [])],
  });

  if (preview) session.setActiveToolsByName(baseTools);

  // Snapshots are taken synchronously at turn_end, while the session matches
  // the turn, and uploaded in order behind each other.
  let spent = session.getSessionStats().cost;
  // With a preview, a reminder sent while exploring cannot ask for a
  // submission, so phase 2 gets its own reminder.
  let nudged = false;
  let submitNudged = false;
  const nudge = (deliver: (text: string) => void) => {
    nudged = true;
    submitNudged = canSubmit;
    opts.onProgress?.(`spent ${spent.toFixed(4)} of the ${opts.budgetUSD} budget; asking the model to ${canSubmit ? "submit" : "end exploration"}`);
    deliver(budgetReminder(canSubmit));
  };
  let uploads = Promise.resolve();
  const unsubscribe = session.subscribe((event) => {
    if (event.type === "turn_end") {
      const snapshot = snapshotOf(sessionManager, event.message);
      const problem = turnProblem(event.message);
      if (problem) opts.onProgress?.(problem.text);
      spent += snapshot.cost;
      if (!submission && !(canSubmit ? submitNudged : nudged) && overNudgeThreshold(spent, opts.budgetUSD)) {
        nudge((text) => session.steer(text).catch((err: unknown) => opts.onProgress?.(`budget reminder failed: ${String(err)}`)));
      }
      const onTurn = opts.onTurn;
      uploads = uploads
        .then(() => onTurn?.(snapshot))
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
        throw modelError(failed, session.getSessionStats());
      }
    };
    if (!submission) {
      if (resumed) opts.onProgress?.(`resuming after turn ${turns(sessionManager)} of an earlier attempt`);
      // A review that crashed during runtime verification goes straight back to it.
      const resumeInPreview = preview && !!resumed && reachedPreviewPhase(resumed);
      if (!resumeInPreview) await prompt(resumed ? resumePrompt(preview) : userPrompt(opts.pr, { preview }));
      if (opts.preparePreview) {
        await uploads;
        previewAccess = await opts.preparePreview();
        session.setActiveToolsByName([...baseTools, "kubectl", "submit_review"]);
        canSubmit = true;
        let text = previewPrompt(previewAccess.namespace, previewAccess.status, resumeInPreview);
        if (overNudgeThreshold(spent, opts.budgetUSD)) nudge((reminder) => (text += `\n\n${reminder}`));
        await prompt(text);
      }
    }
    for (let i = 0; !submission && i < MAX_SUBMIT_REMINDERS; i++) {
      opts.onProgress?.("model stopped without submitting; reminding it");
      await prompt(SUBMIT_REMINDER);
    }
    await uploads;
    const stats = session.getSessionStats();
    if (!submission) throw new ReviewNotSubmittedError(stats);
    return { submission, stats, nudged };
  } finally {
    unsubscribe();
    session.dispose();
  }
}
