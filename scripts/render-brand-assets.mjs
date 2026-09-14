import assert from "node:assert/strict";
import { copyFileSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";

const root = resolve(import.meta.dirname, "..");
const args = process.argv.slice(2);
if (args.length > 1 || (args.length === 1 && args[0] !== "--check")) {
  throw new Error("Usage: node scripts/render-brand-assets.mjs [--check]");
}
const checkOnly = args[0] === "--check";

const svgPath = join(root, "brand", "og", "og-default.svg");
const pngPath = join(root, "brand", "og", "og-default.png");
const temporaryDirectory = mkdtempSync(join(tmpdir(), "llmgw-brand-"));
const renderedPath = join(temporaryDirectory, "og-default.png");
let png;
try {
  const npm = process.env.npm_execpath ?? (process.platform === "win32" ? join(dirname(process.execPath), "node_modules", "npm", "bin", "npm-cli.js") : "npm");
  const command = npm.endsWith(".js") || npm.endsWith(".cjs") ? process.execPath : npm;
  const prefix = command === process.execPath ? [npm] : [];
  // Exact temporary packages keep the public repository free of a permanent renderer dependency.
  const renderer = spawnSync(command, [...prefix,
    "exec",
    "--yes",
    "--package=@resvg/resvg-js@2.6.2",
    "--package=@resvg/resvg-js-cli@2.6.2-beta.1",
    "--",
    "resvg-js",
    "--no-system-font",
    "--fit-width",
    "1200",
    svgPath,
    renderedPath,
  ], { encoding: "utf8" });
  if (renderer.error) throw renderer.error;
  assert.equal(renderer.status, 0, renderer.stderr || renderer.stdout || "resvg failed");
  png = readFileSync(renderedPath);
} finally {
  rmSync(temporaryDirectory, { recursive: true, force: true });
}
const pngSignature = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
assert(png.subarray(0, 8).equals(pngSignature), "renderer did not produce a PNG");
assert.equal(png.readUInt32BE(16), 1200, "rendered PNG width must be 1200");
assert.equal(png.readUInt32BE(20), 630, "rendered PNG height must be 630");

const projections = new Map([
  [join(root, "brand", "icons", "favicon.svg"), join(root, "website", "favicon.svg")],
  [join(root, "brand", "logos", "mark.svg"), join(root, "website", "logo-mark.svg")],
  [join(root, "brand", "icons", "icon-192.svg"), join(root, "website", "icon-192.svg")],
  [join(root, "brand", "icons", "icon-512.svg"), join(root, "website", "icon-512.svg")],
  [svgPath, join(root, "website", "og-default.svg")],
]);

if (checkOnly) {
  assert.deepEqual(readFileSync(pngPath), png, "brand/og/og-default.png is stale; regenerate brand assets");
  for (const [master, projection] of projections) {
    assert.deepEqual(readFileSync(projection), readFileSync(master), `${projection} is not an exact projection of ${master}`);
  }
  assert.deepEqual(readFileSync(join(root, "website", "og-default.png")), png, "website/og-default.png is stale");
  console.log("Brand assets are current with @resvg/resvg-js 2.6.2.");
  process.exit(0);
}

writeFileSync(pngPath, png);
for (const [master, projection] of projections) copyFileSync(master, projection);
copyFileSync(pngPath, join(root, "website", "og-default.png"));
console.log("Rendered brand/og/og-default.png and refreshed six website projections with @resvg/resvg-js 2.6.2.");
