/**
 * Windmill scheduled job: drain llm-gateway's durable notification outbox.
 *
 * Store gateway/admin credentials as Windmill secrets/resources and pass them
 * as arguments. `delivery_webhook` should point at a second Windmill webhook
 * (or another internal notification adapter) that delivers email/Chatwoot.
 * The gateway never embeds mail/chat SDKs and remains a single lean binary.
 */
export async function main(
  gateway_base_url: string,
  gateway_admin_key: string,
  delivery_webhook: string,
  limit = 100,
) {
  const base = gateway_base_url.replace(/\/+$/, "");
  const headers = {
    authorization: `Bearer ${gateway_admin_key}`,
    "content-type": "application/json",
  };
  await fetch(`${base}/admin/api/alerts/evaluate`, {
    method: "POST",
    headers,
    body: "{}",
  }).then(assertOk);

  const workerId = crypto.randomUUID();
  const response = await fetch(`${base}/admin/api/outbox/claim`, {
    method: "POST",
    headers,
    body: JSON.stringify({ worker_id: workerId, limit, lease_seconds: 300 }),
  });
  await assertOk(response);
  const { events = [] } = await response.json();
  const results: Array<{ id: number; status: string; error?: string }> = [];

  for (const event of events) {
    try {
      const delivered = await fetch(delivery_webhook, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(event),
      });
      await assertOk(delivered);
      await fetch(`${base}/admin/api/outbox/${event.id}/delivered`, {
        method: "POST",
        headers,
        body: JSON.stringify({ worker_id: workerId }),
      }).then(assertOk);
      results.push({ id: event.id, status: "delivered" });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      await fetch(`${base}/admin/api/outbox/${event.id}/failed`, {
        method: "POST",
        headers,
        body: JSON.stringify({ worker_id: workerId, error: message }),
      }).then(assertOk);
      results.push({ id: event.id, status: "failed", error: message });
    }
  }
  return { processed: results.length, results };
}

async function assertOk(response: Response) {
  if (!response.ok) {
    throw new Error(`HTTP ${response.status}: ${(await response.text()).slice(0, 300)}`);
  }
  return response;
}
