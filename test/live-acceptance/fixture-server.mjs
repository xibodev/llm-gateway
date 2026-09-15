import { createServer } from "node:http";

const status = Number(process.env.STATUS || 200);
const models = String(process.env.MODELS || "model").split(",").map((value) => value.trim()).filter(Boolean);

createServer((request, response) => {
  response.setHeader("Content-Type", "application/json");
  if (request.url?.includes("/models")) {
    response.end(JSON.stringify({ data: models.map((id) => ({ id })) }));
    return;
  }
  if (status !== 200) {
    response.writeHead(status);
    response.end(JSON.stringify({ error: { message: `controlled ${status}` } }));
    return;
  }
  let body = "";
  request.on("data", (chunk) => { body += chunk; });
  request.on("end", () => {
    let model = models[0];
    try { model = JSON.parse(body).model || model; } catch {}
    response.end(JSON.stringify({ id: "chatcmpl-live", model, choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }] }));
  });
}).listen(8080, "0.0.0.0");
