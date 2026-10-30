#!/usr/bin/env bash
# Builds Delta the same way it was built during development:
#   1. check formatting (gofmt)
#   2. static analysis (go vet)
#   3. run the tests with the race detector
#   4. build a static, stripped binary (no C dependencies, runs on any Linux)
#
# Usage:
#   ./build.sh                 build ./delta for this machine
#   ./build.sh --skip-tests    build without running the tests
#   ./build.sh --all           also cross-compile for common Linux platforms into dist/
#
# Environment variables:
#   VERSION=1.2.0 ./build.sh   embed a version number (shown by "delta -version")
set -euo pipefail

cd "$(dirname "$0")" # always build from the project folder

run_tests=true
build_all=false
for arg in "$@"; do
  case "$arg" in
    --skip-tests) run_tests=false ;;
    --all) build_all=true ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg (see --help)" >&2; exit 1 ;;
  esac
done

if ! command -v go >/dev/null; then
  echo "Go is not installed (https://go.dev/dl/)" >&2
  exit 1
fi

# -s -w strip debug information (smaller binary); -X sets the version variable in main.go.
ldflags="-s -w"
if [[ -n "${VERSION:-}" ]]; then
  ldflags="$ldflags -X main.version=$VERSION"
fi

echo "==> Checking formatting"
unformatted=$(gofmt -l .)
if [[ -n "$unformatted" ]]; then
  echo "These files are not formatted, run 'gofmt -w .':" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "==> go vet"
go vet ./...

if $run_tests; then
  echo "==> Running tests (with race detector)"
  go test -race -count=1 ./...
fi

# CGO_ENABLED=0 produces a fully static binary; -trimpath removes local paths from it.
build() { # build <GOOS> <GOARCH> [GOARM] <output file>
  local goos=$1 goarch=$2 goarm=$3 output=$4
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch GOARM=$goarm \
    go build -trimpath -ldflags="$ldflags" -o "$output" .
  echo "    $output ($(du -h "$output" | cut -f1))"
}

echo "==> Building"
build "$(go env GOOS)" "$(go env GOARCH)" "" delta

if $build_all; then
  echo "==> Cross-compiling into dist/"
  mkdir -p dist
  build linux amd64 "" dist/delta-linux-amd64
  build linux arm64 "" dist/delta-linux-arm64 # e.g. Raspberry Pi 4/5 with 64-bit OS
  build linux arm 7 dist/delta-linux-armv7    # e.g. Raspberry Pi with 32-bit OS
  build linux 386 "" dist/delta-linux-386
fi

echo "==> Done. Start with: ./delta -config delta.json"
