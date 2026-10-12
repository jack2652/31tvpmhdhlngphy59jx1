#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
SERVER_PID=""

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
server_binary="$TEST_ROOT/option-scope-go"
CGO_ENABLED=0 go build -o "$server_binary" ./cmd/go-server

(
  cd "$TEST_ROOT"
  exec env \
    HOST=127.0.0.1 \
    PORT="$port" \
    DATABASE_PATH="$TEST_ROOT/options.db" \
    ACCESS_KEY=go-smoke-test \
    SCHEDULER_ENABLED=false \
    LOW_MEMORY=true \
    UPSTREAM_CONCURRENCY=1 \
    "$server_binary"
) >"$TEST_ROOT/server.log" 2>&1 &
SERVER_PID=$!

base_url="http://127.0.0.1:$port"
ready=false
for _ in $(seq 1 60); do
  if curl --silent --show-error --fail "$base_url/health" >/dev/null 2>&1; then
    ready=true
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    break
  fi
  sleep 0.25
done
if [ "$ready" != true ]; then
  printf 'Go server did not become healthy\n' >&2
  tail -n 80 "$TEST_ROOT/server.log" >&2
  exit 1
fi

python3 - "$base_url" <<'PY'
import json
import sys
import urllib.error
import urllib.request

base = sys.argv[1]

def get(path):
    with urllib.request.urlopen(base + path, timeout=3) as response:
        return response.status, response.read()

status, page = get("/?key=go-smoke-test")
assert status == 200 and b"Option Scope" in page, (status, page[:200])
status, asset = get("/static/common/js/request.js")
assert status == 200 and b"fetch" in asset, (status, asset[:200])

status, payload = get("/api/status/QQQ?key=go-smoke-test")
body = json.loads(payload)
assert status == 200 and body["symbol"] == "QQQ", (status, body)

try:
    urllib.request.urlopen(base + "/api/status/QQQ", timeout=3)
except urllib.error.HTTPError as error:
    assert error.code == 403, error.code
else:
    raise AssertionError("unauthenticated API request unexpectedly succeeded")
PY

printf 'PASS: isolated Go backend startup, page, API auth, and health checks\n'
