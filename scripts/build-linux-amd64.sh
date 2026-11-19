#!/usr/bin/env sh
set -eu

cd "$(dirname "$0")/.."

OUT_DIR="dist"
OUT_NAME="${1:-tailon-2.0.0-stability-linux-amd64}"
OUT="${OUT_DIR}/${OUT_NAME}"

mkdir -p "${OUT_DIR}"

echo "Generating embedded frontend assets..."
go generate ./...

echo "Building ${OUT} for linux/amd64 (CGO disabled)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="-s -w" \
    -o "${OUT}" \
    .

echo "Build complete: ${OUT}"
ls -lh "${OUT}"
