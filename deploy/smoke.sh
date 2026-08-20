#!/usr/bin/env bash
# Live smoke test for a deployed llm-gateway. Read-only except for a tiny chat.
#
#   BASE_URL=https://llm.example.com KEY=llmgw_... ./deploy/smoke.sh [chat-model]
#
# Exit non-zero on the first failed check. Safe to run against production.
set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8787}"
KEY="${KEY:-}"
MODEL="${1:-}"
AUTH=()
[[ -n "$KEY" ]] && AUTH=(-H "Authorization: Bearer ${KEY}")

pass=0; fail=0
ok()   { echo "  [PASS] $1"; pass=$((pass+1)); }
bad()  { echo "  [FAIL] $1"; fail=$((fail+1)); }

echo "== llm-gateway smoke @ ${BASE_URL} =="

# 1) health
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/health")
[[ "$code" == "200" ]] && ok "health 200" || bad "health -> $code"

# 2) models catalog
models=$(curl -s "${AUTH[@]}" "${BASE_URL}/v1/models")
count=$(printf '%s' "$models" | grep -o '"id"' | wc -l | tr -d ' ')
[[ "${count:-0}" -gt 0 ]] && ok "models catalog (${count} entries)" || bad "models catalog empty/unreachable"

# 3) pick a model to exercise (arg > first endpoint > first provider/model)
if [[ -z "$MODEL" ]]; then
  MODEL=$(printf '%s' "$models" | grep -o '"id":"[^"]*","object":"model","owned_by":"endpoint"' | head -1 | sed 's/.*"id":"\([^"]*\)".*/\1/')
fi
if [[ -z "$MODEL" ]]; then
  MODEL=$(printf '%s' "$models" | grep -o '"id":"[^"/]*/[^"]*"' | head -1 | sed 's/"id":"\([^"]*\)"/\1/')
fi
[[ -n "$MODEL" ]] && echo "  using model: ${MODEL}" || bad "no model to test"

# 4) a tiny chat completion
if [[ -n "$MODEL" ]]; then
  body=$(printf '{"model":"%s","messages":[{"role":"user","content":"reply with the single word: ok"}],"max_tokens":300}' "$MODEL")
  resp=$(curl -s -w '\n%{http_code}' "${AUTH[@]}" -H "Content-Type: application/json" -d "$body" "${BASE_URL}/v1/chat/completions")
  chat_code=$(printf '%s' "$resp" | tail -1)
  if [[ "$chat_code" == "200" ]] && printf '%s' "$resp" | grep -q '"content"'; then
    ok "chat completion 200 with content"
  else
    bad "chat completion -> $chat_code"
  fi
fi

echo "== ${pass} passed, ${fail} failed =="
[[ "$fail" -eq 0 ]]
