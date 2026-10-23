// Model selection from the environment. In cluster mode LLM_BASE_URL points at
// the LiteLLM proxy and LLM_API_KEY is a per-run virtual key (M2).
import { join } from "node:path";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";

// pi-ai is not a direct dependency, so derive its Model type from the runtime.
export type PiModel = NonNullable<ReturnType<ModelRuntime["getModel"]>>;

export interface ModelConfig {
  provider: string;
  model: string;
  apiKey: string;
  baseUrl?: string;
  thinking: string;
}

export const THINKING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;
export type ThinkingLevel = (typeof THINKING_LEVELS)[number];

export function modelConfigFromEnv(env: NodeJS.ProcessEnv): ModelConfig {
  const apiKey = env.LLM_API_KEY;
  if (!apiKey) throw new Error("LLM_API_KEY is not set");
  return {
    provider: env.LLM_PROVIDER || "deepseek",
    model: env.LLM_MODEL || "deepseek-flash",
    apiKey,
    ...(env.LLM_BASE_URL ? { baseUrl: env.LLM_BASE_URL } : {}),
    thinking: env.LLM_THINKING || "high",
  };
}

export function parseThinking(level: string): ThinkingLevel {
  if (!(THINKING_LEVELS as readonly string[]).includes(level)) {
    throw new Error(`LLM_THINKING must be one of ${THINKING_LEVELS.join(", ")}, got ${level}`);
  }
  return level as ThinkingLevel;
}

/**
 * Builds a ModelRuntime that reads nothing from ~/.pi. Built-in providers keep
 * their catalog (pricing, compat flags) with an optional baseUrl override; any
 * other provider name is registered as a plain OpenAI-compatible endpoint.
 */
export async function resolveModel(cfg: ModelConfig, agentDir: string): Promise<{ modelRuntime: ModelRuntime; model: PiModel }> {
  const modelRuntime = await ModelRuntime.create({
    authPath: join(agentDir, "auth.json"),
    modelsPath: null,
    refreshOnCreate: false,
  });
  const builtin = modelRuntime.getProvider(cfg.provider) !== undefined;
  if (builtin) {
    if (cfg.baseUrl) modelRuntime.registerProvider(cfg.provider, { baseUrl: cfg.baseUrl });
  } else {
    if (!cfg.baseUrl) throw new Error(`provider ${cfg.provider} is not built in; set LLM_BASE_URL to an OpenAI-compatible endpoint`);
    modelRuntime.registerProvider(cfg.provider, {
      baseUrl: cfg.baseUrl,
      api: "openai-completions",
      models: [
        {
          id: cfg.model,
          name: cfg.model,
          input: ["text"],
          reasoning: false,
          contextWindow: 128_000,
          maxTokens: 16_384,
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        },
      ],
    });
  }
  await modelRuntime.setRuntimeApiKey(cfg.provider, cfg.apiKey);
  const model = modelRuntime.getModel(cfg.provider, cfg.model);
  if (!model) throw new Error(`model ${cfg.provider}/${cfg.model} not found`);
  return { modelRuntime, model };
}
