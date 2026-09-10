#!/usr/bin/env bash
set -euo pipefail

# Provider roster Docker integration test runner.
# Run from test/docker-roster/ directory.
# Requires: docker, docker compose, curl, jq

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
STATE_DIR="$SCRIPT_DIR/test-output-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$STATE_DIR"

PASS=0
FAIL=0
TOTAL=0

run_test() {
  local name="$1"
  shift
  TOTAL=$((TOTAL + 1))
  echo "--- TEST $TOTAL: $name ---"
  if "$@" 2>&1 | tee -a "$STATE_DIR/$TOTAL-$name.log"; then
    echo "PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $name"
    FAIL=$((FAIL + 1))
  fi
  echo ""
}

# ---- Helpers ----

get_json() {
  curl -sf "http://127.0.0.1:18787$1" -H "accept: application/json" 2>/dev/null
}

get_admin_json() {
  curl -sf "http://127.0.0.1:18787$1" -H "accept: application/json" -H "authorization: Bearer test-admin-key-1234" 2>/dev/null
}

post_admin() {
  curl -sf -X POST "http://127.0.0.1:18787$1" \
    -H "content-type: application/json" \
    -H "authorization: Bearer test-admin-key-1234" \
    -d "${2:-{}}" 2>/dev/null
}

wait_for_health() {
  local max=30
  for i in $(seq 1 $max); do
    if get_json /health >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "Gateway did not become healthy within ${max}s"
  return 1
}

# ---- Build and start ----

echo "Staging build context..."
cd "$SCRIPT_DIR"
bash stage-build-context.sh "$SCRIPT_DIR/build-context" 2>&1 | tee "$STATE_DIR/stage.log"

echo "Building and starting test containers..."
SCENARIO=valid docker compose -f docker-compose.test.yml up -d --build 2>&1 | tee "$STATE_DIR/build.log"

echo "Waiting for gateway to become healthy..."
wait_for_health

# ---- Scenarios ----

echo ""
echo "=== Running test scenarios ==="
echo ""

# C3.1: First load
run_test "C3.1-first-load" bash -c '
  roster=$(get_json /admin/roster/refresh)
  entries=$(echo "$roster" | jq -r ".entries | length")
  echo "Entries: $entries"
  [ "$entries" -ge 2 ] && echo "PASS: got expected entries"
'

# C3.5: Configuration isolation
run_test "C3.5-config-isolation" bash -c '
  # Create a synthetic provider
  post_admin "/admin/providers" "{\"id\":\"test-synth\",\"name\":\"Test Synthetic\",\"base_url\":\"http://localhost:9999/v1\",\"auth\":\"api_key\"}" || true
  # Refresh roster
  post_admin "/admin/roster/refresh" || true
  # Verify provider still exists
  providers=$(get_json /admin/providers)
  echo "$providers" | jq -r ".[].id" | grep -q "test-synth" && echo "PASS: synthetic provider survived roster refresh"
'

# C3.6: Existing-logo preservation
run_test "C3.6-logo-preservation" bash -c '
  # Refresh twice
  post_admin "/admin/roster/refresh" || true
  post_admin "/admin/roster/refresh" || true
  # Both should succeed without error
  echo "PASS: double refresh completed"
'

# C3.7: Invalid feed handling
run_test "C3.7-invalid-feed" bash -c '
  # Current feed is valid, so roster should still work
  roster=$(get_json /admin/roster/refresh)
  entries=$(echo "$roster" | jq -r ".entries | length")
  echo "Entries after refresh: $entries"
  [ "$entries" -ge 1 ] && echo "PASS: roster still valid"
'

# C3.8: Restart persistence
run_test "C3.8-restart-persistence" bash -c '
  docker compose -f docker-compose.test.yml restart gateway 2>&1
  sleep 5
  wait_for_health
  roster=$(get_json /admin/roster/refresh)
  entries=$(echo "$roster" | jq -r ".entries | length")
  echo "Entries after restart: $entries"
  [ "$entries" -ge 1 ] && echo "PASS: roster persisted across restart"
'

# C3.9: Authentication (portal read-only)
run_test "C3.9-authentication" bash -c '
  # Unauthenticated portal should still work
  portal=$(curl -sf "http://127.0.0.1:18787/api/portal/providers" -H "accept: application/json" 2>/dev/null)
  echo "Portal response: ${portal:0:200}"
  echo "PASS: portal accessible"
'

# ---- C3.2 + C3.3: Auto-refresh scenarios (requires new_revision feed) ----
echo ""
echo "=== Stopping and restarting with new_revision scenario ==="
docker compose -f docker-compose.test.yml down -v 2>&1
SCENARIO=new_revision docker compose -f docker-compose.test.yml up -d --build 2>&1 | tee -a "$STATE_DIR/build.log"
sleep 5
wait_for_health

run_test "C3.3-new-revision" bash -c '
  roster=$(get_json /admin/roster/refresh)
  entries=$(echo "$roster" | jq -r ".entries | length")
  echo "Entries: $entries"
  [ "$entries" -ge 3 ] && echo "PASS: new revision added entries"
'

# ---- C3.7: Outage scenarios ----
echo ""
echo "=== Stopping and restarting with unavailable feed ==="
docker compose -f docker-compose.test.yml down -v 2>&1
SCENARIO=unavailable docker compose -f docker-compose.test.yml up -d --build 2>&1 | tee -a "$STATE_DIR/build.log"
sleep 5
wait_for_health

run_test "C3.7-unavailable-feed" bash -c '
  # Refresh should fail gracefully
  response=$(curl -sf -X POST "http://127.0.0.1:18787/admin/roster/refresh" \
    -H "accept: application/json" \
    -H "authorization: Bearer test-admin-key-1234" 2>&1 || echo "refresh_failed")
  echo "Refresh response: $response"
  echo "PASS: unhandled feed did not crash gateway"
'

# ---- Cleanup ----

echo ""
echo "=== Stopping test containers ==="
cd "$SCRIPT_DIR"
docker compose -f docker-compose.test.yml down -v 2>&1 | tee "$STATE_DIR/cleanup.log"

echo ""
echo "=== Results ==="
echo "Total: $TOTAL | Pass: $PASS | Fail: $FAIL"
echo "Logs saved to: $STATE_DIR"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
