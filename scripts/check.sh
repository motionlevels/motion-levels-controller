#!/bin/sh
set -eu

files="$(gofmt -l cmd internal)"
if [ -n "$files" ]; then
  printf 'gofmt required:\n%s\n' "$files" >&2
  exit 1
fi

go test -race ./...
go vet ./...
binary="$(mktemp)"
trap 'rm -f "$binary"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$binary" ./cmd/motion-levels-controller
