# Floor Controller

The floor controller is the local hardware-facing service.

It does not produce game animations. It receives logical board frames from
`game-engine` over a length-prefixed protobuf stream, then:

- keeps the latest frame in memory
- refreshes the physical LED floor via UDP at a controller-owned cadence
- serves the `/live` websocket preview
- merges tile pressure state into the preview and recordings
- parses real floor sensor packets
- accepts browser/touchscreen press simulation
- records every rendered frame as protobuf

Only one `game-engine` frame stream is accepted at a time. If a second engine
connects by accident while one is already active, the controller closes the new
connection immediately so streams cannot race each other.

## Run

For local preview without touching the real floor:

```sh
go run ./floor-controller/cmd/floor-controller -http :8081 -broadcast-ip 127.0.0.1
```

Open:

```text
http://127.0.0.1:8081/live
```

The server listens on all interfaces when the HTTP address starts with `:`, so
other machines on the same network can open the LAN URL printed at startup.

For a real floor on the local network, use the default broadcast address:

```sh
go run ./floor-controller/cmd/floor-controller
```

The defaults are:

- HTTP preview: `:8080`
- frame stream TCP listener: `:9090`
- UDP receive socket: `:7800`
- LED broadcast: `255.255.255.255:4626`
- controller ID file: `.motion-levels-controller-id`
- output refresh: `30fps`
- frame recording directory: `recordings`
- hot-path frame recording compression: `none`
- background recording compression: `zstd`
- frame recording rotation: `10m` or `256 MiB`, whichever comes first
- raw recording cleanup: `1h` after verified `.zst` exists
- pending raw recording cap: `4 GiB`
- object-storage upload: disabled unless `-record-upload-platform-url` is set
- game-engine disconnect fade: hold `2s`, then fade to black over `3s`

Frame recording writes one length-prefixed protobuf `FrameRecord` for every
controller-presented frame after pressure state has been merged. The recorded
sequence and timestamp come from the controller presentation clock, not the
source game frame.

The default recording target creates one or more timestamped segment files per
run under the stable controller UUID:

```text
recordings/01234567-89ab-4def-8123-456789abcdef/20260602T154530Z.frames.pbstream.open
recordings/01234567-89ab-4def-8123-456789abcdef/20260602T154530Z.frames.pbstream
recordings/01234567-89ab-4def-8123-456789abcdef/20260602T154530Z.frames.pbstream.zst
recordings/01234567-89ab-4def-8123-456789abcdef/20260602T154530Z-000002.frames.pbstream.zst
```

Passing `recordings/live.frames.pbstream` also resolves to a timestamped
session file for compatibility with early runs. Disable recording with:

```sh
go run ./floor-controller/cmd/floor-controller -record-frames ""
```

The controller ID is generated once and reused on later starts:

```sh
go run ./floor-controller/cmd/floor-controller -controller-id-file .motion-levels-controller-id
```

For a deployed controller, keep this file outside the git checkout so updates do
not change the identity:

```sh
go run ./floor-controller/cmd/floor-controller \
  -controller-id-file /var/lib/motion-levels/floor-controller/controller-id
```

If recordings should sync to Google Drive, point `-record-frames` at the local
synced Drive recordings directory. The controller will still create the
controller UUID subdirectory inside it.

The controller writes raw protobuf on the presentation path by default, then
compresses closed segments with `zstd` in a background worker:

```sh
go run ./floor-controller/cmd/floor-controller -record-compression none
go run ./floor-controller/cmd/floor-controller -record-post-compression zstd
```

Segments rotate before they would exceed the configured byte limit or duration:

```sh
go run ./floor-controller/cmd/floor-controller -record-segment-bytes 268435456 -record-segment-duration 10m
```

Raw `.pbstream` files are deleted one hour after a verified `.zst` exists:

```sh
go run ./floor-controller/cmd/floor-controller -record-delete-raw-after 1h
```

If compression is broken or falls behind, raw files are allowed to accumulate
only up to the configured cap. After that, recording stops instead of risking
the whole machine running out of storage:

```sh
go run ./floor-controller/cmd/floor-controller -record-max-pending-raw-bytes 4294967296
```

Closed recording segments can also upload directly to RustFS through the
platform. The controller asks the platform for a short-lived presigned upload
URL, uploads the finalized segment to RustFS, then reports the byte size,
SHA-256, frame count, sequence range, and real timestamps back to the platform:

```sh
MOTION_LEVELS_PLATFORM_TOKEN=... \
go run ./floor-controller/cmd/floor-controller \
  -record-upload-platform-url https://platform.motionlevels.obis.dev
```

Upload runs on a bounded background queue after compression, so failed or slow
network writes do not block floor refresh. Attach uploaded segments to a known
game session when the game engine provides one:

```sh
go run ./floor-controller/cmd/floor-controller \
  -record-upload-platform-url https://platform.motionlevels.obis.dev \
  -record-upload-session-id session-20260603T100000Z \
  -record-upload-queue-size 256 \
  -record-upload-timeout 5m
```

Tune the hardware, preview, and recording cadence with:

```sh
go run ./floor-controller/cmd/floor-controller -refresh-fps 30
```

If the game-engine connection drops, the controller keeps refreshing the latest
frame briefly, then fades the hardware and live preview to black:

```sh
go run ./floor-controller/cmd/floor-controller -engine-fade-delay 2s -engine-fade-duration 3s
```

Uncompressed recordings at the current 16x32 protobuf shape are roughly 6.5 KB
per frame, or about 0.7 GB per hour at 30fps. Whole-segment `zstd` compression
is much more effective for looping animations than per-frame gzip, but it runs
outside the presentation loop. A sudden process crash can leave the final
in-flight frame incomplete, but previously completed frames remain recoverable.
On startup, stale `.open` files are finalized and `.zst.tmp` files are removed
so closed raw segments can be compressed again. Recording writes run on a
background worker so disk IO does not block the controller presentation loop.
If the disk falls behind for an extended period, recording frames may be dropped
instead of slowing LED output.

## Status

The controller exposes live operational status as JSON:

```text
http://127.0.0.1:8081/status
```

Status includes presented frame count, measured FPS, latest game-frame age,
game-engine connection/fade state, websocket client count, UDP send errors, and
controller ID, recording compression, post-compression, current segment
size/index, pending raw bytes, compression queue health, and queue/drop health.

The process handles `SIGINT` and `SIGTERM` so the recorder can flush and close
cleanly.

## Live Viewer Protocol

The browser viewer uses websocket text messages for control and input events:

- `config`: controller-owned runtime settings
- `pressure`: immediate press/release changes from the floor or browser

Rendered frames are websocket binary messages. Each binary frame starts with a
fixed `MLF1` header, followed by packed RGB bytes and a pressure bitset. This
keeps `/live` lightweight without adding protobuf tooling to the browser.

See `docs/protocol/live-viewer.md` and `docs/protocol/pressure-events.md` for
the protocol details.
