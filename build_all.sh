#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
OUT="dist"
mkdir -p "$OUT"

VARIANTS=(
  "deepseek|proxy.go"
  "openrouter|openrouter/proxy-openrouter.go"
  "ollama|ollama/proxy-ollama.go"
)

TARGETS=(
  "windows|amd64|.exe"
  "linux|amd64|"
  "darwin|amd64|"
  "darwin|arm64|"
)

for variant in "${VARIANTS[@]}"; do
  name="${variant%%|*}"
  main="${variant##*|}"
  for target in "${TARGETS[@]}"; do
    os="${target%%|*}"
    rest="${target#*|}"
    arch="${rest%%|*}"
    ext="${rest##*|}"
    bin="$OUT/${name}_${os}_${arch}${ext}"
    echo "==> $name ($os/$arch)"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" -o "$bin" "$main"
  done
done

echo "Done. Binaries in $OUT/"
