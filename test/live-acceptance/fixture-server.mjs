import { createServer } from "node:http";

const status = Number(process.env.STATUS || 200);
const models = String(process.env.MODELS || "model").split(",").map((value) => value.trim()).filter(Boolean);

createServer((request, response) => {
  response.setHeader("Content-Type", "application/json");
  if (request.url?.includes("/models")) {
    response.end(JSON.stringify({ data: models.map((id) => ({ id, supported_endpoints: ["/v1/chat/completions"] })) }));
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
    let stream = false;
    try {
      const parsed = JSON.parse(body);
      model = parsed.model || model;
      stream = parsed.stream === true;
    } catch {}
    if (stream) {
      // A request that streams is answered as an OpenAI-compatible stream.
      const chunk = (delta, finish) => `data: ${JSON.stringify({ id: "chatcmpl-live", model, choices: [{ index: 0, delta, finish_reason: finish }] })}\n\n`;
      response.setHeader("Content-Type", "text/event-stream");
      response.end(chunk({ role: "assistant", content: "ok" }, null) + chunk({}, "stop") + "data: [DONE]\n\n");
      return;
    }
    response.end(JSON.stringify({ id: "chatcmpl-live", model, choices: [{ index: 0, message: { role: "assistant", content: "ok" }, finish_reason: "stop" }] }));
  });
}).listen(8080, "0.0.0.0");
