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
  python3 -c 'import shutil, sys; shutil.rmtree(sys.argv[1], ignore_errors=True)' "$TEST_ROOT"
}
trap cleanup EXIT
TARGET_GOARCH="$(go env GOARCH)"
BINARY="$TEST_ROOT/option-scope-linux-$TARGET_GOARCH"
OUTPUT_BINARY="$BINARY" bash "$PROJECT_ROOT/scripts/build-go-release.sh" linux "$TARGET_GOARCH"
[ -x "$BINARY" ] || { printf 'FAIL: standalone executable was not produced\n' >&2; exit 1; }
if file "$BINARY" | grep -qi 'dynamically linked'; then
  printf 'FAIL: CGO-disabled standalone executable is dynamically linked\n' >&2
  exit 1
fi
python3 - "$BINARY" "$TARGET_GOARCH" <<'PY'
import struct, sys
path, arch = sys.argv[1:]
machine_by_arch = {"amd64": 62, "arm64": 183, "arm": 40}
with open(path, "rb") as binary: header = binary.read(20)
assert header[:4] == b"\x7fELF" and header[5] in (1, 2)
byte_order = "<" if header[5] == 1 else ">"
machine = struct.unpack(byte_order + "H", header[18:20])[0]
assert machine == machine_by_arch[arch], f"ELF machine {machine}, expected {arch}"
PY
port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
(
  cd "$TEST_ROOT"
  exec env HOST=127.0.0.1 PORT="$port" DATABASE_PATH="$TEST_ROOT/options.db" ACCESS_KEY=embed-test SCHEDULER_ENABLED=false RESOURCE_MODE=off "$BINARY"
 ) >"$TEST_ROOT/server.log" 2>&1 &
SERVER_PID=$!
ready=false
for _ in $(seq 1 60); do if curl -fsS "http://127.0.0.1:$port/health" >/dev/null 2>&1; then ready=true; break; fi; if ! kill -0 "$SERVER_PID" 2>/dev/null; then break; fi; sleep 0.25; done
[ "$ready" = true ] || { tail -n 80 "$TEST_ROOT/server.log" >&2; exit 1; }
curl -fsS "http://127.0.0.1:$port/?key=embed-test" | grep -q 'Option Scope'
curl -fsS "http://127.0.0.1:$port/static/common/js/request.js" | grep -q 'fetch'
printf 'PASS: standalone embedded Go binary (%s/%s)\n' linux "$TARGET_GOARCH"
