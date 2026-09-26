# Windmill notification adapter

Run `outbox_worker.ts` every 1–5 minutes. It:

1. evaluates scheduled alerts (currently key expiry),
2. reads pending quota/key events from the durable SQLite outbox,
3. sends each event to a delivery webhook,
4. marks success or records a retryable failure.

Recommended Windmill secrets/resources:

- `LLMGW_BASE_URL` → `https://gateway.example.com`
- `LLMGW_ADMIN_KEY` → your gateway admin key
- `LLMGW_DELIVERY_WEBHOOK` → an internal Windmill webhook that formats and sends
  Chatwoot/email notifications.

The delivery flow owns channels and recipient preferences. The gateway owns only
policy evaluation and durable events; it does not gain SMTP/chat dependencies.
