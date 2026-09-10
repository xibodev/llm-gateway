import assert from "node:assert/strict";
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, extname, join, normalize, relative, resolve } from "node:path";

const root = resolve(import.meta.dirname, "..");
const excluded = new Set([".git", ".brains", ".opencode", ".superpowers", "node_modules"]);

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
const websiteText = websiteFiles.filter((file) => /\.(?:html|css|js|xml|txt|webmanifest)$/i.test(file)).map(source).join("\n");
check(websiteFiles.some((file) => repoPath(file) === "website/index.html"), "website/index.html missing");
check(websiteFiles.some((file) => repoPath(file) === "website/404.html"), "website/404.html missing");
check(websiteFiles.some((file) => repoPath(file) === "website/sitemap.xml"), "website/sitemap.xml missing");
check(websiteText.includes("https://github.com/xibodev/llm-gateway"), "website repository link missing");
check(!websiteText.includes("https://github.com/\""), "website has placeholder GitHub URL");

const server = source(join(root, "go", "internal", "api", "server.go"));
const publicRoutes = [...server.matchAll(/HandleFunc\("(?:GET|POST) (\/v1\/[^"{]+)/g)].map((match) => match[1]);
const apiDoc = source(join(root, "docs", "API.md"));
for (const route of new Set(publicRoutes)) {
  check(apiDoc.includes(route), `docs/API.md omits ${route}`);
  check(websiteText.includes(route), `website omits ${route}`);
}

const registry = JSON.parse(source(join(root, "go", "internal", "providers", "registry_manifest.json")));
const providersDoc = source(join(root, "docs", "PROVIDERS.md"));
for (const provider of registry) {
  check(providersDoc.includes(provider.label), `docs/PROVIDERS.md omits ${provider.label}`);
  check(websiteText.includes(provider.label), `website omits ${provider.label}`);
}

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

console.log(`documentation checks passed: ${markdownFiles.length} Markdown files, ${registry.length} registry entries, ${websiteFiles.length} website files`);
