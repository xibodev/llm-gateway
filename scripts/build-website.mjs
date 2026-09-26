import { copyFileSync, mkdirSync, rmSync } from "node:fs";
import { resolve, join } from "node:path";

const root = resolve(import.meta.dirname, "..");
const source = join(root, "website");
const output = join(root, ".website-dist");
const args = process.argv.slice(2);
if (args.length && (args.length !== 1 || args[0] !== "--clean")) {
  throw new Error("Usage: node scripts/build-website.mjs [--clean]");
}
if (args[0] === "--clean") {
  rmSync(output, { recursive: true, force: true });
  console.log("Removed generated .website-dist output.");
  process.exit(0);
}
await import("./check-docs.mjs");
const files = [
  "404.html",
  "api.html",
  "clients.html",
  "concepts.html",
  "configuration.html",
  "docs.html",
  "favicon.svg",
  "governance.html",
  "icon-192.svg",
  "icon-512.svg",
  "index.html",
  "limitations.html",
  "logo-mark.svg",
  "og-default.png",
  "og-default.svg",
  "operations.html",
  "providers.html",
  "quickstart.html",
  "robots.txt",
  "search.json",
  "security.html",
  "site.js",
  "site.webmanifest",
  "sitemap.xml",
  "styles.css",
  "upgrading.html",
];
// Only this generated directory is replaced; source and private notes stay out.
rmSync(output, { recursive: true, force: true });
mkdirSync(output);
for (const name of files) copyFileSync(join(source, name), join(output, name));
console.log(`Built ${files.length} static files in ${output}; no production Node runtime.`);
