// Exact reviewed API roots, not hostname or name heuristics. Native APIs deliberately absent.
// Evidence is kept in the collector report, outside display copy.
export const API_EVIDENCE = Object.freeze(Object.fromEntries([
  ['https://api.groq.com/openai/v1', 'openai', 'https://console.groq.com/docs/openai'],
  ['https://api.mistral.ai/v1', 'openai', 'https://docs.mistral.ai/api/'],
  ['https://openrouter.ai/api/v1', 'openai', 'https://openrouter.ai/docs/quickstart'],
  ['https://router.huggingface.co/v1', 'openai', 'https://huggingface.co/docs/inference-providers/index'],
  ['https://integrate.api.nvidia.com/v1', 'openai', 'https://docs.api.nvidia.com/nim/reference/llm-apis'],
  ['https://api.cerebras.ai/v1', 'openai', 'https://inference-docs.cerebras.ai/resources/openai'],
  ['https://api.sambanova.ai/v1', 'openai', 'https://docs.sambanova.ai/cloud/docs/get-started/overview'],
  ['https://api.together.xyz/v1', 'openai', 'https://docs.together.ai/docs/openai-api-compatibility'],
  ['https://api.fireworks.ai/inference/v1', 'openai', 'https://docs.fireworks.ai/tools-sdks/openai-compatibility'],
  ['https://api.deepinfra.com/v1/openai', 'openai', 'https://deepinfra.com/docs/openai_api'],
  ['https://api.deepseek.com/v1', 'openai', 'https://api-docs.deepseek.com/'],
  ['https://api.x.ai/v1', 'openai', 'https://docs.x.ai/docs/api-reference'],
  ['https://ollama.com/v1', 'openai', 'https://docs.ollama.com/openai'],
  ['https://api.siliconflow.cn/v1', 'openai', 'https://docs.siliconflow.cn/en/api-reference/chat-completions/chat-completions'],
  ['https://api.siliconflow.com/v1', 'openai', 'https://docs.siliconflow.com/en/api-reference/chat-completions/chat-completions'],
  ['https://api.anthropic.com', 'anthropic', 'https://docs.anthropic.com/en/api/messages'],
  ['https://opencode.ai/zen/v1', 'openai', 'https://opencode.ai/zen'],
].map(([url, protocol, evidence]) => [url, { protocol, auth: url.includes('zen') ? 'none' : 'api_key', evidence }])));

// A prose homepage is never promoted to an API endpoint, even when its brand is known.
// These URLs are code-reviewed official raster assets, never source-supplied images.
// Icon links were checked against each brand's public website; CDN paths below are
// linked by together.ai and replicate.com themselves. Rights are not an OSS grant.
export const LOGO_SOURCES = Object.freeze([
  { hosts: ['ollama.com'], url: 'https://ollama.com/public/ollama.png' },
  // The lightning is also present in Groq's official header wordmark.
  { hosts: ['groq.com', 'api.groq.com', 'console.groq.com'], url: 'https://groq.com/apple-touch-icon.png' },
  { hosts: ['openrouter.ai'], url: 'https://openrouter.ai/favicon/glyph.png' },
  { hosts: ['localai.io'], url: 'https://localai.io/img/logo-mark.png' },
  { hosts: ['mistral.ai', 'api.mistral.ai', 'console.mistral.ai'], url: 'https://mistral.ai/favicon.ico' },
  { hosts: ['cohere.com', 'api.cohere.com', 'api.cohere.ai', 'dashboard.cohere.com'], url: 'https://cohere.com/apple-touch-icon.png' },
  { hosts: ['deepinfra.com', 'api.deepinfra.com'], url: 'https://deepinfra.com/fav-192.png' },
  { hosts: ['together.ai', 'www.together.ai', 'api.together.ai', 'api.together.xyz'],
    url: 'https://cdn.prod.website-files.com/69654e88dce9154b5f1206dd/69a59cff65ed0f267dd2fbe8_touchicon.png' },
  { hosts: ['nebius.com', 'studio.nebius.com', 'api.studio.nebius.com', 'tokenfactory.nebius.com', 'api.tokenfactory.nebius.com'],
    url: 'https://nebius.com/favicon/favicon-96x96.png' },
  { hosts: ['jina.ai', 'api.jina.ai'], url: 'https://jina.ai/icons/favicon-128x128.png' },
  { hosts: ['replicate.com', 'api.replicate.com'],
    url: 'https://static.replicateassets.com/fe/b383623d83cbf572f53447104233fdd3297736b8/favicon-CKiqAkf2.png' },
  { hosts: ['inference.net', 'api.inference.net'], url: 'https://inference.net/apple-touch-icon.png' },
  { hosts: ['fireworks.ai', 'api.fireworks.ai'], url: 'https://fireworks.ai/icon1.png' },
  { hosts: ['siliconflow.com', 'api.siliconflow.com', 'siliconflow.cn', 'api.siliconflow.cn', 'cloud.siliconflow.cn'],
    url: 'https://cloud.siliconflow.cn/favicon.ico' },
  { hosts: ['z.ai', 'api.z.ai'], url: 'https://z.ai/favicon.png' },
  { hosts: ['platform.stability.ai', 'api.stability.ai'], url: 'https://platform.stability.ai/favicon.png' },
  { hosts: ['api.ai21.com', 'docs.ai21.com', 'studio.ai21.com'], url: 'https://docs.ai21.com/favicon.png' },
  { hosts: ['opencode.ai'], url: 'https://opencode.ai/apple-touch-icon.png' },
].map(asset => ({ ...asset, license: 'Provider trademark; original asset rights retained; used for identification.' })));

// Reviewed official signup/site origins, not an approval inferred from a community link.
// This approves only same-origin fixed favicon paths, not arbitrary images or redirects.
export const APPROVED_SIGNUP_ORIGINS = Object.freeze([
  'https://console.groq.com', 'https://openrouter.ai', 'https://ollama.com',
  'https://huggingface.co', 'https://console.mistral.ai', 'https://aistudio.google.com',
  'https://build.nvidia.com', 'https://dashboard.cohere.com', 'https://cohere.com',
  'https://dash.cloudflare.com', 'https://modelscope.cn', 'https://cloud.siliconflow.cn',
  'https://siliconflow.com', 'https://open.bigmodel.cn', 'https://z.ai',
  'https://cloud.cerebras.ai', 'https://cerebras.ai', 'https://cloud.sambanova.ai',
  'https://together.ai', 'https://fireworks.ai', 'https://deepinfra.com',
  'https://platform.deepseek.com', 'https://console.x.ai', 'https://replicate.com',
  'https://app.hyperbolic.xyz', 'https://tokenfactory.nebius.com', 'https://studio.nebius.com',
  'https://novita.ai', 'https://console.scaleway.com', 'https://bailian.console.alibabacloud.com',
  'https://docs.ai21.com', 'https://studio.ai21.com', 'https://console.upstage.ai',
  'https://www.cerebrium.ai', 'https://www.nscale.com', 'https://console.nscale.com',
  'https://friendli.ai', 'https://portal.nousresearch.com', 'https://experiments.hetzner.com',
  'https://www.ovhcloud.com', 'https://endpoints.ai.cloud.ovh.net', 'https://app.kilo.ai',
  'https://opencode.ai', 'https://sarvam.ai', 'https://deepgram.com', 'https://fish.audio',
  'https://platform.stability.ai', 'https://dash.voyageai.com', 'https://jina.ai',
]);
