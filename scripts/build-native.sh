#!/bin/sh
set -eu

repo_root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$repo_root"

source_revision="${SOURCE_REVISION:-$(git rev-parse HEAD)}"
output_dir="${OUTPUT_DIR:-$repo_root/dist}"
expected_go_version="${CONTROLLER_EXPECTED_GO_VERSION:-}"
base_name="motion-levels-controller-linux-amd64-${source_revision}"
binary="$output_dir/$base_name"
checksum="$binary.sha256"
metadata="$binary.metadata.json"

case "$source_revision" in
  *[!0-9a-f]*|'')
    echo "SOURCE_REVISION must be a lowercase full Git commit SHA" >&2
    exit 64
    ;;
esac
if [ "${#source_revision}" -ne 40 ]; then
  echo "SOURCE_REVISION must contain exactly 40 hexadecimal characters" >&2
  exit 64
fi
if [ "$(git rev-parse HEAD)" != "$source_revision" ]; then
  echo "SOURCE_REVISION must match the checked-out commit" >&2
  exit 65
fi
if [ -n "$(git status --porcelain --untracked-files=all -- go.mod cmd internal)" ]; then
  echo "Controller runtime inputs are dirty; refusing to label them as $source_revision" >&2
  exit 65
fi

actual_go_version="$(go env GOVERSION)"
if [ -n "$expected_go_version" ] && [ "$actual_go_version" != "$expected_go_version" ]; then
  echo "Controller build requires $expected_go_version; found $actual_go_version" >&2
  exit 69
fi

mkdir -p "$output_dir"
temporary="$(mktemp "$output_dir/.motion-levels-controller.XXXXXX")"
trap 'rm -f "$temporary"' EXIT HUP INT TERM

CGO_ENABLED=0 \
GOOS=linux \
GOARCH=amd64 \
GOFLAGS=-mod=readonly \
SOURCE_DATE_EPOCH=0 \
go build \
  -buildvcs=false \
  -trimpath \
  -ldflags="-buildid= -s -w -X github.com/motionlevels/motion-levels-controller/internal/adapter.BuildRevision=$source_revision" \
  -o "$temporary" \
  ./cmd/motion-levels-controller
chmod 0755 "$temporary"
mv -f "$temporary" "$binary"
trap - EXIT HUP INT TERM

python3 - "$binary" "$checksum" "$metadata" "$source_revision" "$actual_go_version" <<'PY'
from hashlib import sha256
import json
from pathlib import Path
import sys

binary, checksum, metadata = map(Path, sys.argv[1:4])
source_revision, go_version = sys.argv[4:6]
payload = binary.read_bytes()
digest = sha256(payload).hexdigest()

checksum.write_text(f"{digest}  {binary.name}\n", encoding="utf-8")
document = {
    "binary": {
        "bytes": len(payload),
        "file": binary.name,
        "sha256": digest,
    },
    "build": {
        "goVersion": go_version,
        "reproducibleTimestamp": "1970-01-01T00:00:00Z",
    },
    "schema": "motion-levels-controller-native-build-v2",
    "sourceRevision": source_revision,
    "target": {
        "architecture": "amd64",
        "operatingSystem": "linux",
    },
}
metadata.write_text(json.dumps(document, indent=2, sort_keys=True) + "\n", encoding="utf-8")
print(json.dumps(document, sort_keys=True))
PY

python3 "$repo_root/scripts/verify-native.py" "$metadata"
