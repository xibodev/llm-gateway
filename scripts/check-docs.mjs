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

// Validate documentation, not legacy application copy or unrelated runtime files.
const files = [...filesUnder(join(root, "docs")), ...filesUnder(join(root, "website")),
  ...["README.md", "CONTRIBUTING.md", "SECURITY.md"].map(file => join(root, file))];
const markdownFiles = files.filter((file) => extname(file).toLowerCase() === ".md");
const productTextFiles = files.filter((file) => /\.(?:md|html)$/i.test(file));

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
const base = new URL("https://xibodev.github.io/llm-gateway/");
const htmlFiles = websiteFiles.filter(file => file.endsWith(".html"));
const htmlIds = new Map(htmlFiles.map(file => [file, [...source(file).matchAll(/\bid="([^"]+)"/g)].map(match => match[1])]));
let linkCount = 0;
function checkSiteURL(raw, from) {
  const url = new URL(raw.replaceAll("&amp;", "&"), new URL(repoPath(from).replace(/^website\//, ""), base));
  if (url.origin !== base.origin) return;
  check(url.pathname.startsWith(base.pathname), `${repoPath(from)} escapes project Pages base: ${raw}`);
  if (!url.pathname.startsWith(base.pathname)) return;
  const path = decodeURIComponent(url.pathname.slice(base.pathname.length)) || "index.html";
  const target = join(websiteDir, path.endsWith("/") ? `${path}index.html` : path);
  check(existsSync(target) && statSync(target).isFile(), `${repoPath(from)} links to missing ${raw}`);
  if (url.hash && existsSync(target)) {
    check(htmlIds.get(target)?.includes(decodeURIComponent(url.hash.slice(1))), `${repoPath(from)} links to missing fragment ${raw}`);
  }
  linkCount++;
}
for (const file of htmlFiles) {
  const text = source(file);
  const ids = htmlIds.get(file);
  check(ids.length === new Set(ids).size, `${repoPath(file)} contains duplicate IDs`);
  check((text.match(/<h1\b/g) || []).length === 1, `${repoPath(file)} must have exactly one h1`);
  check(text.includes('lang="en"'), `${repoPath(file)} has no document language`);
  for (const match of text.matchAll(/\b(?:href|src)="([^"]+)"/g)) checkSiteURL(match[1], file);
  for (const match of text.matchAll(/data-copy="#([^"]+)"/g)) check(ids.includes(match[1]), `${repoPath(file)} has missing copy target ${match[1]}`);
  for (const match of text.matchAll(/<dialog\b([^>]*)>/g)) {
    const labelledBy = match[1].match(/aria-labelledby="([^"]+)"/);
    check(/aria-label=/.test(match[1]) || (labelledBy && ids.includes(labelledBy[1])), `${repoPath(file)} has unlabelled dialog`);
  }
  check(!/https?:\/\/[^"\s]*(?:fonts\.googleapis|cdn\.)/.test(text), `${repoPath(file)} requires external UI assets`);
}
const search = JSON.parse(source(join(websiteDir, "search.json")));
check(search.length === htmlFiles.length - 1, "search must cover every page except 404");
for (const item of search) {
  check(item.title && item.description && item.keywords, "incomplete search entry");
  checkSiteURL(item.url, join(websiteDir, "search.json"));
}
const manifest = JSON.parse(source(join(websiteDir, "site.webmanifest")));
checkSiteURL(manifest.start_url, join(websiteDir, "site.webmanifest"));
for (const icon of manifest.icons) checkSiteURL(icon.src, join(websiteDir, "site.webmanifest"));
for (const match of source(join(websiteDir, "sitemap.xml")).matchAll(/<loc>([^<]+)<\/loc>/g)) checkSiteURL(match[1], join(websiteDir, "sitemap.xml"));
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

check(source(join(root, "docs", "CONFIGURATION.md")).includes("does **not** apply"), "configuration docs must disclose released policy loader limitation");

const maxWebsiteBytes = 350 * 1024;
const websiteBytes = websiteFiles.reduce((total, file) => total + statSync(file).size, 0);
check(websiteBytes <= maxWebsiteBytes, `website is ${websiteBytes} bytes; budget is ${maxWebsiteBytes}`);

if (failures.length) {
  console.error(failures.map((failure) => `- ${failure}`).join("\n"));
  process.exit(1);
}

console.log(`documentation checks passed: ${markdownFiles.length} Markdown files, ${registry.length} registry entries, ${htmlFiles.length} HTML pages, ${linkCount} local URLs, ${websiteBytes} bytes`);
