#!/usr/bin/env python3
from __future__ import annotations

from hashlib import sha256
import json
from pathlib import Path
import struct
import sys


def fail(message: str) -> None:
    raise SystemExit(message)


if len(sys.argv) != 2:
    fail("usage: verify-native.py <native-build-metadata.json>")

metadata_path = Path(sys.argv[1]).resolve()
document = json.loads(metadata_path.read_text(encoding="utf-8"))
if document.get("schema") != "motion-levels-controller-native-build-v2":
    fail("unsupported controller native-build metadata schema")
revision = document.get("sourceRevision")
if not isinstance(revision, str) or len(revision) != 40 or any(character not in "0123456789abcdef" for character in revision):
    fail("invalid controller source revision")
if document.get("target") != {"architecture": "amd64", "operatingSystem": "linux"}:
    fail("controller native build target must be linux/amd64")
go_version = document.get("build", {}).get("goVersion")
if not isinstance(go_version, str) or not go_version.startswith("go1."):
    fail("controller native build has an invalid Go version")

binary_record = document.get("binary", {})
expected_name = f"motion-levels-controller-linux-amd64-{revision}"
if binary_record.get("file") != expected_name:
    fail("controller binary filename does not match its source revision")
binary_path = metadata_path.parent / expected_name
checksum_path = Path(f"{binary_path}.sha256")
payload = binary_path.read_bytes()
digest = sha256(payload).hexdigest()
if digest != binary_record.get("sha256") or len(payload) != binary_record.get("bytes"):
    fail("controller binary digest or size does not match metadata")
if checksum_path.read_text(encoding="utf-8") != f"{digest}  {expected_name}\n":
    fail("controller checksum sidecar is not canonical")
if revision.encode("ascii") not in payload:
    fail("controller binary does not contain its full source revision")

if len(payload) < 64 or payload[:4] != b"\x7fELF":
    fail("controller binary is not ELF")
if payload[4:6] != b"\x02\x01":
    fail("controller binary must be 64-bit little-endian ELF")
header = struct.unpack_from("<16sHHIQQQIHHHHHH", payload, 0)
machine = header[2]
program_header_offset = header[5]
program_header_size = header[9]
program_header_count = header[10]
if machine != 62:
    fail("controller binary is not AMD64 ELF")
for index in range(program_header_count):
    offset = program_header_offset + index * program_header_size
    if offset + 4 > len(payload):
        fail("controller ELF program headers are truncated")
    if struct.unpack_from("<I", payload, offset)[0] == 3:
        fail("controller binary is dynamically linked")

print(
    f"Verified {expected_name}: sha256={digest} revision={revision} "
    f"toolchain={go_version}"
)
