#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$project_root"
set -a
source "${DEV_ENV_FILE:-.env.local}"
set +a

case "${1:-run}" in
  run)
    exec go run ./cmd/openapi-mcp-server
    ;;
  smoke)
    exec go run ./cmd/mcp-smoke -endpoint "http://${HTTP_ADDR:-127.0.0.1:8080}${MCP_PATH:-/mcp}"
    ;;
  schema)
    exec go run ./cmd/schema-check
    ;;
  *)
    echo 'usage: scripts/dev.sh run|smoke|schema' >&2
    exit 2
    ;;
esac
