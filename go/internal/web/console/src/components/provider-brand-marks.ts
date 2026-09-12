import {
  siAlibabacloud, siAnthropic, siCloudflare, siDeepseek, siGithubcopilot,
  siGooglecloud, siGooglegemini, siHuggingface, siLmstudio, siMinimax,
  siMistralai, siMoonshotai, siNvidia, siOllama, siOpenrouter, siPerplexity,
  siReplicate, siVllm, type SimpleIcon,
} from "simple-icons";

export interface ProviderBrandMark {
  icon?: SimpleIcon;
  viewBox?: string;
  paths?: { d: string; fill?: string; fillRule?: "evenodd" }[];
  initials?: string;
  monochrome?: boolean;
  paleBackground?: boolean;
}

const deepgram: ProviderBrandMark = { paths: [{ d: "M11.203 24H1.517a.364.364 0 0 1-.258-.62l6.239-6.275a.366.366 0 0 1 .259-.108h3.52c2.723 0 5.025-2.127 5.107-4.845a5.004 5.004 0 0 0-4.999-5.148H7.613v4.646c0 .2-.164.364-.365.364H.968a.365.365 0 0 1-.363-.364V.364C.605.164.768 0 .969 0h10.416c6.684 0 12.111 5.485 12.01 12.187C23.293 18.77 17.794 24 11.202 24z", fill: "#13EF93" }] };
const fishaudio: ProviderBrandMark = { paths: [{ d: "M23.12 12.13v.62c0 .24.19.44.44.44.24 0 .44-.2.44-.44v-.62c0-.24-.2-.44-.44-.44-.25 0-.44.2-.44.44m-1.77.37v1.74c0 .24.2.44.44.44.25 0 .44-.2.44-.44V12.5c0-.24-.19-.44-.44-.44-.24 0-.44.2-.44.44m-1.76.43v1.99q0 .09.03.17.04.08.1.14.07.07.15.1t.17.03c.24 0 .44-.19.44-.44v-1.99c0-.25-.2-.44-.44-.44q-.09 0-.17.03t-.15.1q-.06.06-.1.14-.03.08-.03.17m-1.76.35v1.97c0 .24.19.44.44.44.24 0 .44-.2.44-.44v-1.97c0-.24-.2-.44-.44-.44-.25 0-.44.2-.44.44m-1.77.32v1.65c0 .25.2.45.44.45s.44-.2.44-.45V13.6c0-.25-.2-.45-.44-.45s-.44.2-.44.45m-1.76 1.26v.3q.01.17.14.3.13.11.3.12.18-.01.31-.12.13-.13.14-.3v-.3q-.01-.17-.14-.3-.13-.11-.31-.12-.17.01-.3.12-.13.13-.14.3M0 9.38v.19c0 .24.2.44.44.44.25 0 .44-.2.44-.44v-.19c0-.25-.19-.44-.44-.44-.24 0-.44.19-.44.44m1.82.15v.81c0 .24.19.44.44.44.24 0 .44-.2.44-.44v-.81q0-.18-.13-.31t-.31-.13-.31.13-.13.31m1.81-.24v3.39q.01.17.14.3.13.11.3.12.18-.01.31-.12.13-.13.14-.3V9.29q-.01-.17-.14-.3-.13-.11-.31-.12-.17.01-.3.12-.13.13-.14.3m1.83-.35v.22c0 .24.19.44.44.44.24 0 .44-.2.44-.44v-.22q0-.18-.13-.31T5.9 8.5t-.31.13-.13.31m0 2.94v2.34c0 .25.19.44.44.44.24 0 .44-.19.44-.44v-2.34c0-.24-.2-.44-.44-.44-.25 0-.44.2-.44.44m15.89-3.32V11c0 .25.2.44.44.44.25 0 .44-.19.44-.44V8.56c0-.24-.19-.44-.44-.44-.24 0-.44.2-.44.44m-1.76-.39v3.28q.01.17.14.3.13.11.3.12.18-.01.31-.12.13-.13.14-.3V8.17c0-.25-.2-.44-.44-.44q-.09 0-.17.03t-.15.1q-.06.06-.1.14-.03.08-.03.17M17.83 8v3.9c0 .24.19.44.44.44.24 0 .44-.2.44-.44V8c0-.25-.2-.44-.44-.44-.25 0-.44.19-.44.44m-1.77.11v3.96c0 .25.2.44.44.44s.44-.19.44-.44V8.11c0-.25-.2-.44-.44-.44s-.44.19-.44.44m-1.76.27v5.03q.01.17.14.3.13.11.3.12.18-.01.31-.12.13-.13.14-.3V8.38c0-.25-.2-.44-.45-.44-.24 0-.44.19-.44.44m-1.8.46v5.75q.01.17.14.3.13.11.3.12.18-.01.31-.12.13-.13.14-.3V8.84c0-.25-.2-.44-.45-.44-.24 0-.44.19-.44.44m-1.76.76v5.52c0 .24.2.44.44.44.25 0 .44-.2.44-.44V9.6c0-.24-.19-.44-.44-.44-.24 0-.44.2-.44.44m-1.77 1.01v4.77q.01.17.14.3.13.11.3.12.18-.01.31-.12.13-.13.14-.3v-4.77q-.01-.17-.14-.3-.13-.11-.31-.12-.17.01-.3.12-.13.13-.14.3M23.12 9.6v1.22c0 .24.19.44.44.44.24 0 .44-.2.44-.44V9.6c0-.24-.2-.44-.44-.44-.25 0-.44.2-.44.44M7.21 11.32V16c0 .24.19.44.44.44.24 0 .44-.2.44-.44v-4.68q0-.18-.13-.31t-.31-.13-.31.13-.13.31", fill: "#9B90E8" }] };
const github: ProviderBrandMark = { monochrome: true, paths: [{ d: "M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12" }] };
const scaleway: ProviderBrandMark = { paths: [{ d: "M16.605 11.11v5.72a1.77 1.77 0 01-1.54 1.69h-4a1.43 1.43 0 01-1.31-1.22 1.09 1.09 0 010-.18 1.37 1.37 0 011.37-1.36h1.74a1 1 0 001-1v-3.62a1.4 1.4 0 011.18-1.39h.17a1.37 1.37 0 011.39 1.36zm-6.46 1.74V9.26a1 1 0 011-1h1.85a1.37 1.37 0 001.37-1.37 1 1 0 000-.17 1.45 1.45 0 00-1.41-1.2h-3.96a1.81 1.81 0 00-1.58 1.66v5.7a1.37 1.37 0 001.37 1.37h.21a1.4 1.4 0 001.15-1.4zm12-4.29V20a4.53 4.53 0 01-4.15 4h-7.58a8.57 8.57 0 01-8.56-8.57V4.54A4.54 4.54 0 016.395 0h7.18a8.56 8.56 0 018.56 8.56zm-2.74 0a5.83 5.83 0 00-5.82-5.82h-7.19a1.79 1.79 0 00-1.8 1.8v10.89a5.83 5.83 0 005.82 5.8h7.44a1.79 1.79 0 001.54-1.48z", fill: "#4F0599" }] };
const ovh: ProviderBrandMark = { paths: [{ d: "M19.881 10.095l2.563-4.45C23.434 7.389 24 9.404 24 11.555c0 2.88-1.017 5.523-2.71 7.594h-6.62l2.04-3.541h-2.696l3.176-5.513h2.691zm-2.32-5.243L9.333 19.14l.003.009H2.709C1.014 17.077 0 14.435 0 11.555c0-2.152.57-4.17 1.561-5.918L5.855 13.1 10.6 4.852h6.961z", fill: "#123F6D" }] };
const google: ProviderBrandMark = { paths: [{ d: "M12.48 10.92v3.28h7.84c-.24 1.84-.853 3.187-1.787 4.133-1.147 1.147-2.933 2.4-6.053 2.4-4.827 0-8.6-3.893-8.6-8.72s3.773-8.72 8.6-8.72c2.6 0 4.507 1.027 5.907 2.347l2.307-2.307C18.747 1.44 16.133 0 12.48 0 5.867 0 .307 5.387.307 12s5.56 12 12.173 12c3.573 0 6.267-1.173 8.373-3.36 2.16-2.16 2.84-5.213 2.84-7.667 0-.76-.053-1.467-.173-2.053H12.48z", fill: "#4285F4" }] };

// First-party geometry and ownership notices: provider-brand-notices.md.
const openai: ProviderBrandMark = {
  monochrome: true,
  // Remove only the kit's oversized artboard margin, retaining clear space.
  viewBox: "155 155 406 406",
  paths: [{ d: "M508.749 317.399C516.777 287.314 508.991 253.884 485.389 230.282C461.788 206.681 428.36 198.895 398.273 206.923C376.231 184.928 343.39 174.956 311.148 183.596C278.906 192.234 255.45 217.292 247.36 247.361C217.291 255.451 192.233 278.91 183.595 311.149C174.957 343.391 184.927 376.232 206.924 398.274C198.896 428.359 206.683 461.789 230.284 485.391C253.885 508.992 287.313 516.779 317.401 508.75C339.442 530.745 372.286 540.717 404.525 532.079C436.767 523.441 460.223 498.384 468.313 468.315C498.383 460.224 523.44 436.766 532.078 404.526C540.716 372.285 530.747 339.443 508.749 317.402V317.399ZM470.899 244.776C486.892 260.77 493.488 282.601 490.687 303.412L415.577 260.046C412.411 258.218 408.509 258.218 405.345 260.046L317.401 310.82V277.526C317.401 275.191 318.652 273.005 320.676 271.837L387.644 233.174C414.178 218.353 448.346 222.223 470.901 244.776H470.899ZM357.837 311.144L398.275 334.491V381.185L357.837 404.532L317.398 381.185V334.491L357.837 311.144ZM264.776 269.693C265.207 239.305 285.644 211.649 316.453 203.393C338.3 197.54 360.505 202.744 377.127 215.573L302.014 258.937C298.848 260.764 296.898 264.144 296.898 267.798V369.346L268.065 352.699C266.043 351.531 264.776 349.353 264.776 347.017V269.691V269.693ZM203.391 316.454C209.244 294.608 224.854 277.978 244.276 269.999V356.73C244.276 360.384 246.226 363.763 249.392 365.591L337.337 416.365L308.503 433.013C306.481 434.181 303.961 434.188 301.939 433.02L234.971 394.357C208.868 378.789 195.138 347.261 203.391 316.454ZM244.775 470.9C228.781 454.906 222.186 433.075 224.986 412.264L300.096 455.63C303.263 457.457 307.164 457.457 310.328 455.63L398.273 404.856V438.149C398.273 440.485 397.022 442.671 394.997 443.839L328.029 482.502C301.495 497.322 267.327 493.452 244.772 470.9H244.775ZM450.897 445.982C450.466 476.371 430.029 504.027 399.22 512.283C377.373 518.136 355.168 512.932 338.547 500.102L413.659 456.738C416.826 454.911 418.775 451.532 418.775 447.877V346.329L447.609 362.977C449.631 364.145 450.897 366.323 450.897 368.659V445.985V445.982ZM512.282 399.221C506.429 421.068 490.819 437.697 471.397 445.676V358.946C471.397 355.292 469.448 351.912 466.281 350.085L378.336 299.311L407.17 282.663C409.192 281.495 411.712 281.487 413.734 282.655L480.702 321.318C506.805 336.887 520.536 368.415 512.282 399.221Z" }],
};

const cerebras: ProviderBrandMark = {
  viewBox: "0 0 533 533", paleBackground: true,
  paths: [
    { fill: "#F05A28", fillRule: "evenodd", d: "M269.588 497.912C237.2 497.912 206.526 491.428 178.519 479.795C136.604 462.251 100.786 432.884 75.4467 395.888C50.1073 358.893 35.4371 314.461 35.4371 266.405C35.4371 234.367 41.9148 204.047 53.9177 176.205C71.6362 134.633 101.358 99.3535 138.7 74.3721C176.042 49.3907 221.005 34.8977 269.588 34.8977V0C232.437 0 197 7.43721 164.611 20.9767C116.218 41.1907 75.0656 74.9442 46.1063 117.47C16.9564 159.995 0 211.293 0 266.405C0 303.209 7.62087 338.298 21.1479 370.144C41.5338 418.009 75.8277 458.628 118.695 487.423C161.753 516.219 213.575 533 269.398 533V497.912H269.588Z" },
    { fill: "#F05A28", fillRule: "evenodd", d: "M149.56 408.474C127.65 390.168 111.265 368.047 100.215 343.828C89.1642 319.609 83.4486 293.484 83.4486 267.167C83.4486 246.191 87.0685 225.214 94.1178 205.191C101.358 185.167 112.027 166.098 126.697 148.744C144.987 127.005 167.278 110.605 191.474 99.5442C215.671 88.4837 242.153 82.9535 268.445 82.9535C289.403 82.9535 310.551 86.5768 330.555 93.6326C350.751 100.879 369.803 111.558 387.14 126.051L409.813 99.1628C389.236 82 366.374 69.0326 342.368 60.6419C318.362 52.0605 293.404 47.8651 268.445 47.8651C237.009 47.8651 205.764 54.5395 176.804 67.6977C147.845 80.8558 121.362 100.498 99.6429 126.242C82.3054 146.837 69.5405 169.53 60.967 193.367C52.3935 217.205 48.202 242.186 48.202 267.167C48.202 298.442 54.8703 329.716 68.0163 358.512C81.1623 387.307 100.977 413.814 126.888 435.363L149.56 408.474Z" },
    { fill: "#F05A28", fillRule: "evenodd", d: "M203.858 385.781C180.805 373.577 162.706 355.842 150.322 335.056C137.938 314.27 131.46 290.242 131.46 266.023C131.46 244.665 136.414 223.116 147.273 203.093C159.467 180.019 177.376 162.093 198.333 149.888C219.291 137.493 243.296 131.009 267.683 131.009C289.022 131.009 310.741 135.967 330.937 146.647L347.321 115.563C321.982 102.214 294.547 95.7303 267.493 95.921C236.819 95.921 206.526 104.121 180.234 119.567C153.942 135.014 131.27 157.898 116.028 186.693C102.691 212.056 96.2135 239.326 96.2135 266.023C96.2135 296.535 104.406 326.665 120.029 352.791C135.652 379.107 158.514 401.419 187.474 416.674L203.858 385.781Z" },
    { fill: "#F05A28", fillRule: "evenodd", d: "M269.779 352.219C257.776 352.219 246.345 349.74 236.057 345.354C220.434 338.87 207.288 327.809 197.952 313.888C188.617 299.967 183.092 283.377 183.092 265.451C183.092 253.437 185.568 241.995 189.95 231.698C196.428 216.251 207.478 202.902 221.386 193.558C235.295 184.214 251.87 178.684 269.779 178.684V143.595C253.013 143.595 237.009 147.028 222.339 153.13C200.429 162.474 181.948 177.73 168.802 197.181C155.466 216.823 147.845 240.47 147.845 265.642C147.845 282.423 151.274 298.442 157.371 313.126C166.707 335.056 182.139 353.554 201.572 366.712C221.005 379.679 244.44 387.307 269.779 387.307V352.219Z" },
    { fill: "#000000", fillRule: "evenodd", d: "M298.357 237.8C294.356 233.605 289.974 230.172 285.592 227.693C281.21 225.214 276.638 223.879 271.875 223.879C265.397 223.879 259.872 225.023 254.728 227.312C249.774 229.6 245.392 232.651 241.772 236.656C238.152 240.47 235.485 245.047 233.58 250.005C231.675 254.963 230.913 260.302 230.913 265.642C230.913 270.981 231.865 276.321 233.58 281.279C235.485 286.237 238.152 290.814 241.772 294.628C245.392 298.442 249.584 301.684 254.728 303.972C259.681 306.26 265.397 307.405 271.875 307.405C277.209 307.405 282.353 306.26 286.926 304.163C291.498 301.874 295.499 298.633 298.738 294.437L321.982 319.419C318.553 322.851 314.552 325.902 309.979 328.381C305.407 330.861 300.834 332.958 296.262 334.484C291.689 336.009 287.116 337.154 282.925 337.726C278.734 338.488 274.923 338.679 271.875 338.679C261.396 338.679 251.489 336.963 242.344 333.53C233.008 330.098 225.006 325.14 218.338 318.656C211.479 312.363 206.145 304.544 202.144 295.581C198.143 286.619 196.238 276.512 196.238 265.642C196.238 254.581 198.143 244.665 202.144 235.702C206.145 226.74 211.479 219.112 218.338 212.628C225.197 206.335 233.199 201.377 242.344 197.754C251.679 194.321 261.587 192.605 271.875 192.605C280.829 192.605 289.784 194.321 298.738 197.754C307.693 201.186 315.695 206.526 322.363 213.772L298.357 237.8Z" },
  ],
};

const cohere: ProviderBrandMark = {
  viewBox: "0 0 32 32", paleBackground: true,
  paths: [
    { fill: "#355146", fillRule: "evenodd", d: "M10.3615 19.053C11.2016 19.053 12.8726 19.0057 15.1823 18.0307C17.8739 16.8945 23.229 14.8318 27.0919 12.7132C29.7936 11.2314 30.9779 9.27159 30.9779 6.63236C30.978 2.96943 28.0819 0 24.5094 0H9.54127C4.40983 0 0.25 4.26514 0.25 9.52649C0.25 14.7878 4.14483 19.053 10.3615 19.053Z" },
    { fill: "#D18EE2", fillRule: "evenodd", d: "M12.8928 25.6161C12.8928 23.0371 14.4071 20.7118 16.7303 19.7231L21.4441 17.7173C26.2121 15.6884 31.46 19.281 31.46 24.5741C31.46 28.6749 28.2172 31.999 24.2176 31.9979L19.114 31.9965C15.6778 31.9956 12.8928 29.1392 12.8928 25.6161Z" },
    { fill: "#FF7759", d: "M5.60615 20.3047H5.60606C2.64799 20.3047 0.25 22.7634 0.25 25.7963V26.5076C0.25 29.5406 2.64799 31.9993 5.60606 31.9993H5.60615C8.56422 31.9993 10.9622 29.5406 10.9622 26.5076V25.7963C10.9622 22.7634 8.56422 20.3047 5.60615 20.3047Z" },
  ],
};

function simple(icon: SimpleIcon): ProviderBrandMark {
  const channels = icon.hex.match(/.{2}/g)!.map((channel) => parseInt(channel, 16));
  const monochrome = Math.max(...channels) <= 64 && Math.max(...channels) - Math.min(...channels) <= 8;
  return { icon, monochrome };
}

const marks: Record<string, ProviderBrandMark> = {
  openai, openai_codex: openai, cerebras, cohere,
  // The final, standalone lightning subpath of Groq's official header logo.
  groq: { monochrome: true, viewBox: "0 0 369.6 562.32", paths: [{ d: "M165.98 342.21H0L272.4 1.5l-68.75 220.11H369.6L97.23 562.32z" }] },
  anthropic: simple(siAnthropic), github_copilot: simple(siGithubcopilot),
  gemini: simple(siGooglegemini), vertex_ai: simple(siGooglecloud),
  mistral: simple(siMistralai), ollama: simple(siOllama), openrouter: simple(siOpenrouter),
  nvidia: simple(siNvidia), cloudflare: simple(siCloudflare), deepseek: simple(siDeepseek),
  alibaba: simple(siAlibabacloud), replicate: simple(siReplicate), huggingface: simple(siHuggingface),
  perplexity: simple(siPerplexity), lmstudio: simple(siLmstudio), vllm: simple(siVllm),
  minimax: simple(siMinimax), moonshot: simple(siMoonshotai),
  deepgram, fishaudio, github, scaleway, ovh, google,
  // These are explicit textual identifiers, not architecture-kit brand assets.
  azure_openai: { initials: "AZ" }, bedrock: { initials: "AWS" },
  localai: { initials: "LA" }, litellm: { initials: "LL" }, edge_tts: { initials: "ET" },
  custom_openai: { initials: "API" }, custom_anthropic: { initials: "API" }, custom: { initials: "API" },
};

const aliases: Record<string, string> = {
  codex: "openai_codex", google: "gemini", ai_studio: "gemini", google_ai_studio: "gemini",
  "google-ai-studio": "gemini", google_flow: "google", "google-flow": "google",
  google_vertex: "vertex_ai", google_vertex_ai: "vertex_ai", copilot: "github_copilot",
  mistralai: "mistral", nvidia_nim: "nvidia", cloudflare_workers_ai: "cloudflare",
  "cloudflare-workers-ai": "cloudflare", alibaba_cloud: "alibaba", dashscope: "alibaba",
  qwen: "alibaba", "alibaba-cloud": "alibaba", "alibaba-cloud-model-studio": "alibaba",
  hugging_face: "huggingface", "hugging-face": "huggingface", lm_studio: "lmstudio",
  moonshotai: "moonshot", azure: "azure_openai", amazon_bedrock: "bedrock",
  deepgram: "deepgram", "fish-audio": "fishaudio", fish_audio: "fishaudio",
  "github-models": "github", github_models: "github", "scaleway-generative-apis": "scaleway",
  scaleway: "scaleway", ovh: "ovh", "ovh-ai-endpoints": "ovh", ovh_ai_endpoints: "ovh",
};

const endpointBrands: Record<string, string> = {
  "api.openai.com": "openai", "chatgpt.com": "openai_codex", "api.anthropic.com": "anthropic",
  "api.groq.com": "groq", "api.cerebras.ai": "cerebras", "api.cohere.com": "cohere", "api.cohere.ai": "cohere",
  "generativelanguage.googleapis.com": "gemini", "aiplatform.googleapis.com": "vertex_ai",
  "api.mistral.ai": "mistral", "openrouter.ai": "openrouter", "api.deepseek.com": "deepseek",
  "integrate.api.nvidia.com": "nvidia", "ai.api.nvidia.com": "nvidia", "api.cloudflare.com": "cloudflare",
  "dashscope.aliyuncs.com": "alibaba", "dashscope-intl.aliyuncs.com": "alibaba", "dashscope-us.aliyuncs.com": "alibaba",
  "api.replicate.com": "replicate", "router.huggingface.co": "huggingface", "api-inference.huggingface.co": "huggingface",
  "api.perplexity.ai": "perplexity", "api.minimax.io": "minimax", "api.minimax.chat": "minimax",
  "api.moonshot.ai": "moonshot", "api.moonshot.cn": "moonshot", "ollama.com": "ollama",
  "api.githubcopilot.com": "github_copilot", "api.individual.githubcopilot.com": "github_copilot",
  "api.business.githubcopilot.com": "github_copilot", "api.enterprise.githubcopilot.com": "github_copilot",
  "api.deepgram.com": "deepgram", "api.fish.audio": "fishaudio", "api.scaleway.ai": "scaleway",
  "endpoints.ai.cloud.ovh.net": "ovh",
};

/** Match endpoint hosts, never display names, paths, or arbitrary suffixes. */
export function providerBrandForEndpoint(value?: string): string | undefined {
  if (!value) return undefined;
  try {
    const url = new URL(value);
    if (!["https:", "http:"].includes(url.protocol) || url.username || url.password) return undefined;
    const host = url.hostname.toLowerCase();
    if (Object.hasOwn(endpointBrands, host)) return endpointBrands[host];
    if (/^[a-z0-9-]+\.openai\.azure\.com$/.test(host)) return "azure_openai";
    if (/^[a-z0-9-]+-aiplatform\.googleapis\.com$/.test(host)) return "vertex_ai";
    if (/^bedrock-runtime\.[a-z]{2}(?:-[a-z]+)+-\d\.amazonaws\.com(?:\.cn)?$/.test(host)) return "bedrock";
  } catch { /* Incomplete or local endpoints keep their explicit identifier. */ }
  return undefined;
}

export function providerBrandID(id: string, baseURL?: string): string | undefined {
  const raw = id.trim().toLowerCase();
  const stripped = raw.startsWith("roster:") ? raw.slice(7) : raw;
  const normalized = stripped.replace(/[-_]+/g, "_");
  const canonical = Object.hasOwn(aliases, stripped) ? aliases[stripped] :
    Object.hasOwn(aliases, normalized) ? aliases[normalized] :
    Object.hasOwn(marks, stripped) ? stripped :
    Object.hasOwn(marks, normalized) ? normalized : undefined;
  if (!canonical) return providerBrandForEndpoint(baseURL);
  if (canonical.startsWith("custom")) return providerBrandForEndpoint(baseURL) ?? (Object.hasOwn(marks, canonical) ? canonical : undefined);
  return Object.hasOwn(marks, canonical) ? canonical : providerBrandForEndpoint(baseURL);
}

export function hasProviderMark(id: string, baseURL?: string): boolean {
  return providerBrandID(id, baseURL) !== undefined;
}

export const hasProviderBrand = hasProviderMark;

export function resolveProviderBrand({ id, baseURL }: { id: string; baseURL?: string }): ProviderBrandMark | undefined {
  const brand = providerBrandID(id, baseURL);
  return brand ? marks[brand] : undefined;
}
