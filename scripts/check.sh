#!/bin/sh
set -eu

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

protoc --go_out="$tmp_dir" --go_opt=paths=source_relative \
  contracts/floorpb/floor.proto \
  contracts/inputpb/input.proto \
  contracts/recordingpb/recording.proto
cmp -s "$tmp_dir/contracts/floorpb/floor.pb.go" contracts/floorpb/floor.pb.go
cmp -s "$tmp_dir/contracts/inputpb/input.pb.go" contracts/inputpb/input.pb.go
cmp -s "$tmp_dir/contracts/recordingpb/recording.pb.go" contracts/recordingpb/recording.pb.go

go test ./...
go vet ./...

files="$(gofmt -l cmd internal contracts)"
if [ -n "$files" ]; then
  printf 'gofmt required:\n%s\n' "$files"
  exit 1
fi

sh -n deploy/entrypoint.sh
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck deploy/entrypoint.sh
fi
