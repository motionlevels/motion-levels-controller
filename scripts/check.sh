#!/bin/sh
set -eu

files="$(gofmt -l cmd internal)"
if [ -n "$files" ]; then
  printf 'gofmt required:\n%s\n' "$files" >&2
  exit 1
fi

go test -race -count=1 ./...
go vet ./...

if command -v node >/dev/null 2>&1; then
  node scripts/check-web.mjs
elif [ "${REQUIRE_NODE:-0}" = "1" ]; then
  echo "Node.js is required to validate the embedded dashboard" >&2
  exit 1
else
  echo "warning: Node.js unavailable; skipped embedded dashboard syntax check" >&2
fi

binary="$(mktemp)"
trap 'rm -f "$binary"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$binary" ./cmd/motion-levels-controller

if command -v nix >/dev/null 2>&1; then
  nix flake check --no-write-lock-file
fi
