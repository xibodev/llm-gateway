import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, extname, join, normalize, relative, resolve } from "node:path";

const root = resolve(import.meta.dirname, "..");
const excluded = new Set([".git", ".brains", ".opencode", ".quality-run", ".superpowers", ".website-dist", "node_modules"]);

function filesUnder(directory) {
  const output = [];
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    if (excluded.has(entry.name)) continue;
    const path = join(directory, entry.name);
    if (entry.isDirectory()) output.push(...filesUnder(path));
    else output.push(path);
  }
  return output;
}

const files = filesUnder(root);
const markdownFiles = files.filter((file) => extname(file).toLowerCase() === ".md");
const productTextFiles = files.filter((file) => /\.(?:md|html|tsx|yaml|yml)$/i.test(file));

function source(file) { return readFileSync(file, "utf8"); }
function repoPath(file) { return relative(root, file).replaceAll("\\", "/"); }

const failures = [];
function check(condition, message) { if (!condition) failures.push(message); }
function checkSame(master, projection) {
  check(existsSync(master), `${repoPath(master)} missing`);
  check(existsSync(projection), `${repoPath(projection)} missing`);
  if (existsSync(master) && existsSync(projection)) {
    check(readFileSync(master).equals(readFileSync(projection)), `${repoPath(projection)} differs from ${repoPath(master)}`);
  }
}

function pngDimensions(file) {
  const data = readFileSync(file);
  const signature = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
  if (data.length < 24 || !data.subarray(0, 8).equals(signature) || data.toString("ascii", 12, 16) !== "IHDR") return null;
  return { width: data.readUInt32BE(16), height: data.readUInt32BE(20) };
}

function parseJson(file) {
  try { return JSON.parse(source(file)); }
  catch { failures.push(`${repoPath(file)} is invalid JSON`); return null; }
}

function checkXml(file) {
  const text = source(file);
  check(!/&(?!amp;|lt;|gt;|quot;|apos;|#\d+;|#x[\da-f]+;)/i.test(text), `${repoPath(file)} contains an unescaped ampersand`);
  const stack = [];
  for (const match of text.matchAll(/<([^>]+)>/g)) {
    const tag = match[1].trim();
    if (!tag || tag.startsWith("?") || tag.startsWith("!")) continue;
    if (tag.startsWith("/")) {
      const name = tag.slice(1).trim();
      check(stack.pop() === name, `${repoPath(file)} has mismatched closing tag ${name}`);
      continue;
    }
    if (tag.endsWith("/")) continue;
    const name = tag.match(/^([A-Za-z_][\w:.-]*)/)?.[1];
    check(Boolean(name), `${repoPath(file)} has an invalid XML tag`);
    if (name) stack.push(name);
  }
  check(stack.length === 0, `${repoPath(file)} has unclosed XML tag ${stack.at(-1) ?? "unknown"}`);
  const ids = [...text.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]);
  check(new Set(ids).size === ids.length, `${repoPath(file)} has duplicate XML ids`);
  for (const match of text.matchAll(/\bhref="#([^"]+)"/g)) check(ids.includes(match[1]), `${repoPath(file)} references missing #${match[1]}`);
  const titles = [...text.matchAll(/<title\s+id="([^"]+)"/g)].map((match) => match[1]);
  const descriptions = [...text.matchAll(/<desc\s+id="([^"]+)"/g)].map((match) => match[1]);
  for (const match of text.matchAll(/\baria-labelledby="([^"]+)"/g)) {
    for (const reference of match[1].split(/\s+/)) check(titles.includes(reference) || descriptions.includes(reference), `${repoPath(file)} aria-labelledby references non-title/desc #${reference}`);
  }
}

for (const file of markdownFiles) {
  const text = source(file);
  for (const match of text.matchAll(/\[[^\]]*\]\(([^)]+)\)/g)) {
    const raw = match[1].trim().replace(/^<|>$/g, "");
    if (!raw || raw.startsWith("#") || /^[a-z][a-z0-9+.-]*:/i.test(raw)) continue;
    const decoded = decodeURIComponent(raw.split("#")[0]);
    const target = normalize(resolve(dirname(file), decoded));
    check(existsSync(target), `${repoPath(file)} links to missing ${raw}`);
  }
}

const allProductText = productTextFiles.map((file) => `${repoPath(file)}\n${source(file)}`).join("\n");
for (const forbidden of [
  "https://github.com/\"",
  "safe to read/commit",
  "token is shown once",
  "shown once, stored hashed",
  "Python build is available",
  "Every management mutation is audit logged",
]) {
  check(!allProductText.includes(forbidden), `stale public claim remains: ${forbidden}`);
}

const websiteDir = join(root, "website");
const websiteFiles = filesUnder(websiteDir);
const websiteHtmlFiles = websiteFiles.filter((file) => extname(file).toLowerCase() === ".html");
const websiteText = websiteFiles.filter((file) => /\.(?:html|css|js|xml|txt|webmanifest)$/i.test(file)).map(source).join("\n");
check(websiteFiles.some((file) => repoPath(file) === "website/index.html"), "website/index.html missing");
check(websiteFiles.some((file) => repoPath(file) === "website/404.html"), "website/404.html missing");
check(websiteFiles.some((file) => repoPath(file) === "website/sitemap.xml"), "website/sitemap.xml missing");
check(websiteText.includes("https://github.com/xibodev/llm-gateway"), "website repository link missing");
check(!websiteText.includes("https://github.com/\""), "website has placeholder GitHub URL");

const requiredBrandFiles = [
  "BRAND.md", "LICENSES.md", "README.md", "preview.html", "provenance.json", "tokens.css", "tokens.json",
  "logos/mark.svg", "logos/mark-inverse.svg", "logos/lockup.svg", "logos/lockup-inverse.svg",
  "logos/wordmark.svg", "logos/wordmark-inverse.svg", "logos/mono-black.svg", "logos/mono-white.svg",
  "icons/favicon.svg", "icons/app-icon.svg", "icons/icon-192.svg", "icons/icon-512.svg",
  "og/og-default.svg", "og/og-default.png",
];
for (const name of requiredBrandFiles) check(existsSync(join(root, "brand", name)), `brand/${name} missing`);

const requiredRuntimeBrandFiles = ["favicon.svg", "logo-mark.svg", "icon-192.svg", "icon-512.svg", "og-default.svg", "og-default.png", "site.webmanifest"];
for (const name of requiredRuntimeBrandFiles) check(existsSync(join(websiteDir, name)), `website/${name} missing`);

const projectionPairs = [
  ["brand/icons/favicon.svg", "website/favicon.svg"],
  ["brand/logos/mark.svg", "website/logo-mark.svg"],
  ["brand/icons/icon-192.svg", "website/icon-192.svg"],
  ["brand/icons/icon-512.svg", "website/icon-512.svg"],
  ["brand/og/og-default.svg", "website/og-default.svg"],
  ["brand/og/og-default.png", "website/og-default.png"],
];
for (const [master, projection] of projectionPairs) checkSame(join(root, master), join(root, projection));

for (const file of websiteHtmlFiles) {
  const text = source(file);
  const header = text.match(/<header\b[\s\S]*?<\/header>/i)?.[0];
  check(Boolean(header), `${repoPath(file)} shared header missing`);
  if (!header) continue;
  const brand = header.match(/<a\b[^>]*class="[^"]*\bbrand\b[^"]*"[^>]*>[\s\S]*?<\/a>/i)?.[0];
  check(Boolean(brand), `${repoPath(file)} header brand link missing`);
  if (!brand) continue;
  const visible = brand.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();
  const label = brand.match(/aria-label="([^"]+)"/i)?.[1] ?? "";
  check(visible === "llmgw", `${repoPath(file)} header brand must visibly read llmgw`);
  check(/^llmgw\b/i.test(label), `${repoPath(file)} header brand accessible label must begin with llmgw`);
}
const expectedSharedHeaderPages = ["404.html", "api.html", "clients.html", "concepts.html", "configuration.html", "docs.html", "governance.html", "index.html", "limitations.html", "operations.html", "providers.html", "quickstart.html", "security.html", "upgrading.html"];
check(websiteHtmlFiles.length === expectedSharedHeaderPages.length, `website HTML inventory changed; expected ${expectedSharedHeaderPages.length} pages, found ${websiteHtmlFiles.length}`);
for (const name of expectedSharedHeaderPages) check(existsSync(join(websiteDir, name)), `shared header page website/${name} missing`);

const homepage = source(join(websiteDir, "index.html"));
const socialImage = "https://xibodev.github.io/llm-gateway/og-default.png";
for (const metadata of [
  `<meta property="og:image" content="${socialImage}">`,
  "<meta property=\"og:image:type\" content=\"image/png\">",
  "<meta property=\"og:image:width\" content=\"1200\">",
  "<meta property=\"og:image:height\" content=\"630\">",
  "<meta property=\"og:image:alt\"",
  "<meta name=\"twitter:card\" content=\"summary_large_image\">",
  `<meta name="twitter:image" content="${socialImage}">`,
  "<meta name=\"twitter:image:alt\"",
]) check(homepage.includes(metadata), `website/index.html missing ${metadata}`);
check(homepage.includes('<link rel="canonical" href="https://xibodev.github.io/llm-gateway/">'), "website/index.html canonical URL changed");
check(!homepage.includes("<svg"), "website/index.html must use the canonical logo-mark.svg projection, not inline mark artwork");

const canonicalMark = source(join(root, "brand", "logos", "mark-inverse.svg"));
for (const fragment of [
  'viewBox="0 0 100 100"',
  '<line x1="16" y1="50" x2="42" y2="50" stroke="#FFFFFF" stroke-width="5" stroke-linecap="round"/>',
  '<circle cx="16" cy="50" r="3.5" fill="#FFFFFF"/>',
  '<polygon points="42,32 58,50 42,68" fill="#F59E0B"/>',
  'd="M54 44 L66 32 L84 32"',
  'd="M58 50 L84 50"',
  'd="M54 56 L66 68 L84 68"',
  '<circle cx="84" cy="32" r="3.5" fill="#FFFFFF"/>',
  '<circle cx="84" cy="50" r="3.5" fill="#FFFFFF"/>',
  '<circle cx="84" cy="68" r="3.5" fill="#FFFFFF"/>',
]) check(canonicalMark.includes(fragment), `canonical inverse mark geometry missing ${fragment}`);

const png = pngDimensions(join(root, "brand", "og", "og-default.png"));
check(png?.width === 1200 && png?.height === 630, `brand/og/og-default.png must be 1200x630, found ${png ? `${png.width}x${png.height}` : "invalid PNG"}`);
check(!source(join(root, "brand", "og", "og-default.svg")).includes("<text"), "brand/og/og-default.svg must use deterministic path lettering, not font-dependent text");

const manifest = parseJson(join(websiteDir, "site.webmanifest"));
if (manifest) {
  check(manifest.name === "llm-gateway", "manifest name must be llm-gateway");
  check(manifest.short_name === "llmgw", "manifest short_name must be llmgw");
  check(manifest.theme_color === "#090A0F", "manifest theme_color must be #090A0F");
  for (const size of ["192x192", "512x512"]) {
    const icon = manifest.icons?.find((entry) => entry.sizes === size);
    check(Boolean(icon), `manifest icon ${size} missing`);
    if (icon) check(existsSync(join(websiteDir, icon.src)), `manifest icon ${icon.src} is not published`);
  }
}
for (const file of files.filter((entry) => [".svg", ".xml"].includes(extname(entry).toLowerCase()))) checkXml(file);
for (const file of [join(root, "brand", "provenance.json"), join(root, "brand", "tokens.json"), join(websiteDir, "search.json")]) parseJson(file);
check(source(join(root, "brand", "icons", "icon-192.svg")).includes('width="192" height="192"'), "brand/icons/icon-192.svg dimensions must be 192x192");
check(source(join(root, "brand", "icons", "icon-512.svg")).includes('width="512" height="512"'), "brand/icons/icon-512.svg dimensions must be 512x512");
check(source(join(root, "brand", "og", "og-default.svg")).includes('width="1200" height="630" viewBox="0 0 1200 630"'), "brand/og/og-default.svg dimensions must be 1200x630");

const server = source(join(root, "go", "internal", "api", "server.go"));
const publicRoutes = [...server.matchAll(/HandleFunc\("(?:GET|POST) (\/v1\/[^"{]+)/g)].map((match) => match[1]);
const apiDoc = source(join(root, "docs", "API.md"));
for (const route of new Set(publicRoutes)) {
  check(apiDoc.includes(route), `docs/API.md omits ${route}`);
  check(websiteText.includes(route), `website omits ${route}`);
}

const registry = parseJson(join(root, "go", "internal", "providers", "registry_manifest.json"));
const providersDoc = source(join(root, "docs", "PROVIDERS.md"));
const providersPage = source(join(websiteDir, "providers.html"));
const providerMatrixBody = providersPage.match(/<h2\b[^>]*\bid="registry"[^>]*>[\s\S]*?<tbody\b[^>]*>([\s\S]*?)<\/tbody>/i)?.[1] ?? "";
const providerMatrixLabels = [...providerMatrixBody.matchAll(/<tr\b[^>]*>[\s\S]*?<td\b[^>]*>([\s\S]*?)<\/td>/gi)]
  .map((match) => match[1].replace(/<[^>]+>/g, " ").replaceAll("&amp;", "&").replace(/\s+/g, " ").trim());
check(Boolean(providerMatrixBody), "website/providers.html registry matrix missing");
check(providerMatrixLabels.length === registry?.length, `website/providers.html registry matrix has ${providerMatrixLabels.length} rows; expected ${registry?.length ?? "invalid"}`);
check(new Set(providerMatrixLabels).size === providerMatrixLabels.length, "website/providers.html registry matrix has duplicate integration labels");
for (const provider of registry ?? []) {
  check(providersDoc.includes(provider.label), `docs/PROVIDERS.md omits ${provider.label}`);
  check(providerMatrixLabels.includes(provider.label), `website/providers.html registry matrix omits ${provider.label}`);
}
const countClaims = [...websiteText.matchAll(/\b(?:All )?(\d+) registry entr(?:y|ies)\b/g)];
check(countClaims.length > 0, "website has no visible registry entry count");
for (const claim of countClaims) check(Number(claim[1]) === registry?.length, `visible provider count ${claim[1]} does not match ${registry?.length ?? "invalid"} registry entries`);

const configExample = source(join(root, "llmgw.config.example.yaml"));
check(configExample.includes("policies:"), "example policies block missing");
check(source(join(root, "docs", "CONFIGURATION.md")).includes("retry_max_attempts"), "configuration docs omit policies");

const maxWebsiteBytes = 350 * 1024;
const websiteBytes = websiteFiles.reduce((total, file) => total + statSync(file).size, 0);
check(websiteBytes <= maxWebsiteBytes, `website is ${websiteBytes} bytes; budget is ${maxWebsiteBytes}`);

if (failures.length) {
  console.error(failures.map((failure) => `- ${failure}`).join("\n"));
  process.exit(1);
}

console.log(`documentation checks passed: ${markdownFiles.length} Markdown files, ${registry?.length ?? "invalid"} registry entries, ${websiteFiles.length} website files`);
