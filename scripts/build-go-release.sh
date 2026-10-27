#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TARGET_GOOS="${TARGET_GOOS:-${GOOS:-linux}}"
TARGET_GOARCH="${TARGET_GOARCH:-${GOARCH:-amd64}}"
if [ "$#" -gt 2 ]; then
  printf 'usage: bash %s [goos] [goarch]\n' "${BASH_SOURCE[0]}" >&2
  exit 2
fi
if [ "$#" -ge 1 ]; then TARGET_GOOS="$1"; fi
if [ "$#" -ge 2 ]; then TARGET_GOARCH="$2"; fi
OUTPUT_BINARY="${OUTPUT_BINARY:-$PROJECT_DIR/dist/option-scope-$TARGET_GOOS-$TARGET_GOARCH}"

fail() { printf '[error] %s\n' "$*" >&2; exit 1; }
command -v go >/dev/null 2>&1 || fail 'Go must be installed on the build machine'
[ "$TARGET_GOOS" = linux ] || fail 'deployment binaries currently target Linux only'
[ -f "$PROJECT_DIR/go.mod" ] || fail "go.mod not found under $PROJECT_DIR"
[ -d "$PROJECT_DIR/web/static" ] || fail 'web/static is required to embed browser assets in the Go server'
case "$TARGET_GOARCH" in amd64|arm64|arm) ;; *) fail "unsupported target architecture: $TARGET_GOARCH" ;; esac
if [ "$TARGET_GOARCH" = arm ]; then TARGET_GOARM="${TARGET_GOARM:-${GOARM:-7}}"; else TARGET_GOARM=""; fi
OUTPUT_PARENT="$(dirname "$OUTPUT_BINARY")"
OUTPUT_NAME="$(basename "$OUTPUT_BINARY")"
mkdir -p "$OUTPUT_PARENT"
OUTPUT_PARENT="$(cd "$OUTPUT_PARENT" && pwd)"
OUTPUT_BINARY="$OUTPUT_PARENT/$OUTPUT_NAME"

TEMP_BINARY="$OUTPUT_BINARY.tmp.$$"
cleanup() { rm -f "$TEMP_BINARY"; }
trap cleanup EXIT
build_env=(env CGO_ENABLED=0 GOOS="$TARGET_GOOS" GOARCH="$TARGET_GOARCH")
if [ "$TARGET_GOARCH" = arm ]; then build_env+=(GOARM="$TARGET_GOARM"); fi
printf '[build] target=%s/%s cgo=disabled flags=-trimpath -ldflags="-s -w"\n' "$TARGET_GOOS" "$TARGET_GOARCH"
if ! (cd "$PROJECT_DIR" && "${build_env[@]}" go build -trimpath -ldflags='-s -w' -o "$TEMP_BINARY" ./cmd/go-server); then fail "Go build failed for $TARGET_GOOS/$TARGET_GOARCH"; fi
chmod 0755 "$TEMP_BINARY"
mv "$TEMP_BINARY" "$OUTPUT_BINARY"
TEMP_BINARY=""
printf '[done] standalone binary: %s\n' "$OUTPUT_BINARY"
