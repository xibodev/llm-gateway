import { copyFileSync, mkdirSync, readdirSync, rmSync, statSync } from "node:fs";
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
const extensions = /\.(?:html|css|js|json|svg|webmanifest|xml|txt)$/;
const files = readdirSync(source).filter(name => extensions.test(name) && statSync(join(source, name)).isFile());
// Only this generated directory is replaced; source and private notes stay out.
rmSync(output, { recursive: true, force: true });
mkdirSync(output);
for (const name of files) copyFileSync(join(source, name), join(output, name));
console.log(`Built ${files.length} static files in ${output}; no production Node runtime.`);
