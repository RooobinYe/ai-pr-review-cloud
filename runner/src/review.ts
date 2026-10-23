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
import { BUDGET_REMINDER, SUBMIT_REMINDER, systemPrompt, userPrompt } from "./prompts.ts";
import type { ReviewSubmission } from "./schema.ts";
import { gitTool, submitReviewTool } from "./tools.ts";
import { overNudgeThreshold, turnProblem, type TurnUsage, turnUsage } from "./usage.ts";

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
  /** Spending at which the model is told to submit (§7.3); none when unset. */
  budgetUSD?: number;
  onProgress?: (line: string) => void;
  /** Called with each turn's usage as the turn ends. */
  onTurn?: (usage: TurnUsage) => void;
}

export interface ReviewOutcome {
  submission: ReviewSubmission;
  stats: SessionStats;
  /** Whether the budget reminder was sent. */
  nudged: boolean;
}

export class ReviewNotSubmittedError extends Error {
  readonly stats: SessionStats;
  constructor(stats: SessionStats, message = `model did not call submit_review after ${MAX_SUBMIT_REMINDERS} reminders`) {
    super(message);
    this.stats = stats;
  }
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

  let turns = 0;
  let spent = 0;
  let nudged = false;
  // The provider error of the latest turn, if it failed; cleared by a good turn.
  let providerError: string | undefined;
  const unsubscribe = session.subscribe((event) => {
    if (event.type === "turn_end") {
      const problem = turnProblem(event.message);
      if (problem) opts.onProgress?.(problem.text);
      providerError = problem?.providerError ? problem.text : undefined;
      const usage = turnUsage(++turns, event.message);
      if (!usage) return;
      spent += usage.cost_usd;
      opts.onTurn?.(usage);
      if (!nudged && !submission && overNudgeThreshold(spent, opts.budgetUSD)) {
        nudged = true;
        opts.onProgress?.(`spent $${spent.toFixed(4)} of the $${opts.budgetUSD} budget; asking the model to submit`);
        session.steer(BUDGET_REMINDER).catch((err: unknown) => opts.onProgress?.(`budget reminder failed: ${String(err)}`));
      }
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
    for (let i = 0; !submission && !providerError && i < MAX_SUBMIT_REMINDERS; i++) {
      opts.onProgress?.("model stopped without submitting; reminding it");
      await session.prompt(SUBMIT_REMINDER);
    }
    const stats = session.getSessionStats();
    // A refused call (no credit, bad key, proxy budget) is not the model
    // ignoring the tool; say so, so the failure is not misread.
    if (!submission && providerError) throw new ReviewNotSubmittedError(stats, `model API error: ${providerError.replace(/^model turn ended with error: /, "")}`);
    if (!submission) throw new ReviewNotSubmittedError(stats);
    return { submission, stats, nudged };
  } finally {
    unsubscribe();
    session.dispose();
  }
}
