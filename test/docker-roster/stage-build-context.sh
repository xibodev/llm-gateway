#!/usr/bin/env bash
set -euo pipefail

# Stages a minimal build context for the Docker test.
# Usage: ./stage-build-context.sh <output-dir>

OUTPUT_DIR="${1:?usage: stage-build-context.sh <output-dir>}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

mkdir -p "$OUTPUT_DIR/go" "$OUTPUT_DIR/internal/web/console"

# Core Go files
cp "$REPO_ROOT/go/go.mod" "$REPO_ROOT/go/go.sum" "$OUTPUT_DIR/go/"
cp -r "$REPO_ROOT/go/cmd" "$OUTPUT_DIR/go/"
cp -r "$REPO_ROOT/go/internal" "$OUTPUT_DIR/go/internal/"

# Remove node_modules and test dirs from internal (large, not needed for build)
find "$OUTPUT_DIR/go/internal/web/console" -name "node_modules" -type d -exec rm -rf {} + 2>/dev/null || true
find "$OUTPUT_DIR/go/internal/web/console" -name "tests" -type d -exec rm -rf {} + 2>/dev/null || true

# Copy console dist if it exists (needed for embedding)
if [ -d "$REPO_ROOT/go/internal/web/console/dist" ]; then
  cp -r "$REPO_ROOT/go/internal/web/console/dist" "$OUTPUT_DIR/go/internal/web/console/"
fi

echo "Staged build context at $OUTPUT_DIR ($(du -sh "$OUTPUT_DIR" | cut -f1))"
