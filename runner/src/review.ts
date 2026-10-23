// The review pipeline shared by local and cluster mode (docs/architecture.md §4.2).
import {
  createAgentSession,
  createExtensionRuntime,
  type ModelRuntime,
  type ResourceLoader,
  type SessionStats,
  SessionManager,
  SettingsManager,
} from "@earendil-works/pi-coding-agent";
import { commentableLines } from "./diff.ts";
import type { PiModel, ThinkingLevel } from "./model.ts";
import type { PRContext } from "./pr.ts";
import { SUBMIT_REMINDER, systemPrompt, userPrompt } from "./prompts.ts";
import type { ReviewSubmission } from "./schema.ts";
import { gitTool, submitReviewTool } from "./tools.ts";

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
}

export interface ReviewOutcome {
  submission: ReviewSubmission;
  stats: SessionStats;
}

/** A review that ended without a submission; stats say what it cost. */
export class ReviewFailedError extends Error {
  readonly stats: SessionStats;
  constructor(message: string, stats: SessionStats) {
    super(message);
    this.stats = stats;
  }
}

export class ReviewNotSubmittedError extends ReviewFailedError {
  constructor(stats: SessionStats) {
    super(`model did not call submit_review after ${MAX_SUBMIT_REMINDERS} reminders`, stats);
  }
}

/** The model API refused a request and pi's retries did not help. */
export class ModelError extends ReviewFailedError {}

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
  if (name === "git" && Array.isArray(args.args)) return `git ${args.args.join(" ")}`;
  const target = args.pattern ?? args.path ?? "";
  return `${name} ${String(target)}`.trim();
}

export async function review(opts: ReviewOptions): Promise<ReviewOutcome> {
  let submission: ReviewSubmission | undefined;
  const submit = submitReviewTool(commentableLines(opts.pr.diff), (s) => {
    submission = s;
  });

  const { session } = await createAgentSession({
    cwd: opts.cwd,
    agentDir: opts.agentDir,
    model: opts.model,
    modelRuntime: opts.modelRuntime,
    thinkingLevel: opts.thinkingLevel,
    resourceLoader: isolatedResources(systemPrompt(opts.language)),
    sessionManager: SessionManager.inMemory(opts.cwd),
    settingsManager: SettingsManager.inMemory({ retry: { enabled: true, maxRetries: 3 } }),
    tools: ["read", "grep", "find", "ls", "git", "submit_review"],
    customTools: [gitTool(opts.cwd), submit],
  });

  // The latest model request's error, cleared when a retry succeeds.
  let failure: string | undefined;
  const unsubscribe = session.subscribe((event) => {
    if (event.type === "message_end" && event.message.role === "assistant") {
      failure = event.message.stopReason === "error" ? event.message.errorMessage || "unknown error" : undefined;
    } else if (event.type === "tool_execution_start") {
      opts.onProgress?.(describeToolCall(event.toolName, event.args ?? {}));
    } else if (event.type === "tool_execution_end" && event.isError) {
      const text: unknown = event.result?.content?.[0]?.text;
      const reason = typeof text === "string" ? `: ${text.split("\n")[0]!.slice(0, 200)}` : "";
      opts.onProgress?.(`  ${event.toolName} failed${reason}`);
    }
  });

  try {
    await session.prompt(userPrompt(opts.pr));
    // A reminder cannot help when the model API itself is failing.
    for (let i = 0; !submission && failure === undefined && i < MAX_SUBMIT_REMINDERS; i++) {
      opts.onProgress?.("model stopped without submitting; reminding it");
      await session.prompt(SUBMIT_REMINDER);
    }
    const stats = session.getSessionStats();
    if (!submission) throw failure === undefined ? new ReviewNotSubmittedError(stats) : modelError(failure, stats);
    return { submission, stats };
  } finally {
    unsubscribe();
    session.dispose();
  }
}
