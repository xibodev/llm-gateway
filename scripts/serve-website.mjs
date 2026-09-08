import { createServer } from "node:http";
import { readFileSync, statSync } from "node:fs";
import { extname, join, resolve, sep } from "node:path";

const root = resolve(import.meta.dirname, "../.website-dist");
const base = "/llm-gateway/";
const types = { ".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".json": "application/json", ".svg": "image/svg+xml", ".webmanifest": "application/manifest+json", ".xml": "application/xml", ".txt": "text/plain" };
const server = createServer((request, response) => {
  const pathname = new URL(request.url, "http://localhost").pathname;
  if (pathname === base.slice(0, -1)) {
    response.writeHead(302, { Location: base }).end();
    return;
  }
  let file;
  let status = 200;
  try {
    if (!pathname.startsWith(base)) throw new Error("Not found");
    file = resolve(root, decodeURIComponent(pathname.slice(base.length)) || "index.html");
    if (!file.startsWith(root + sep) || !statSync(file).isFile()) throw new Error("Not found");
  } catch {
    file = join(root, "404.html");
    status = 404;
  }
  response.writeHead(status, { "Content-Type": `${types[extname(file)] || "application/octet-stream"}; charset=utf-8`, "Cache-Control": "no-store" });
  response.end(readFileSync(file));
});
server.listen(Number(process.env.PORT || 8080), "127.0.0.1", () => {
  console.log(`Website preview: http://127.0.0.1:${server.address().port}${base}`);
});
