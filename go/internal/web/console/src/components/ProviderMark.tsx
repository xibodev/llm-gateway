import { Bot, Box, Cloud } from "lucide-preact";
import {
  siAnthropic,
  siGithubcopilot,
  siGooglegemini,
  siMistralai,
  siOllama,
  siOpenrouter,
  type SimpleIcon,
} from "simple-icons";

const icons: Record<string, SimpleIcon> = {
  github_copilot: siGithubcopilot,
  anthropic: siAnthropic,
  custom_anthropic: siAnthropic,
  gemini: siGooglegemini,
  mistral: siMistralai,
  ollama: siOllama,
  openrouter: siOpenrouter,
};

export function ProviderMark({ id, label }: { id: string; label?: string }) {
  const normalized = id.toLowerCase();
  const icon = icons[normalized];
  const fallback = normalized === "openai" || normalized === "openai_codex"
    ? <Bot size={18} strokeWidth={1.7} aria-hidden="true" />
    : normalized === "bedrock"
      ? <Cloud size={18} strokeWidth={1.7} aria-hidden="true" />
      : <Box size={17} strokeWidth={1.7} aria-hidden="true" />;
  return (
    <span class="provider-mark" aria-label={label ?? id} style={icon ? { color: `#${icon.hex}` } : undefined}>
      {icon
        ? <svg viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d={icon.path} /></svg>
        : fallback}
    </span>
  );
}
