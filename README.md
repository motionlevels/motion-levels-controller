# Motion Levels Floor Controller

A small hardware-facing adapter between the Motion Levels game engine and the
physical 16×32 LED floor.

It owns only the hardware boundary:

- one local engine connection;
- one latest packed RGB frame;
- physical UDP frame and sync transmission;
- physical pressure parsing and logical-coordinate mapping;
- a frame-freshness watchdog with hold, fade, and black output;
- a loopback hardware diagnostics page;
- bounded operational metrics and native/Nix packaging.

Game rules, product sessions, players, recordings, kiosk lifecycle, platform
credentials, venue orchestration, and updates belong elsewhere.

## Runtime model

The engine sends packed row-major RGB frames. The controller retains only the
latest valid frame and refreshes the floor at 50 fps. Frames never queue.

Pressure is a canonical 512-bit snapshot. A full snapshot is sent when an engine
connects and whenever pressure changes, so a dropped intermediate update heals
on the next snapshot.

A newer loopback engine connection replaces the previous one, invalidates its
frame generation, and begins from black until it sends a valid frame.

## Safety watchdog

Safety uses locally observed frame age, not socket connectivity. With defaults:

1. the frame lease expires after 500 ms;
2. the previous frame is held for 2 seconds;
3. it fades linearly to black over 3 seconds;
4. black continues to be physically refreshed.

The engine should resend its current desired frame even when its visual content
has not changed. The controller also sends black on startup and attempts a final
black transaction during orderly shutdown.

## Engine wire contract

The single unversioned full-duplex TCP contract listens on `127.0.0.1:4201`.
Controller and engine revisions are promoted in one atomic venue release.

Every message begins with:

```text
1 byte  message type
4 bytes big-endian payload length
N bytes payload, maximum 4096
```

### Desired frame, engine → controller (`type = 1`)

```text
8 bytes    positive monotonic sequence
1536 bytes RGB, 16 × 32 × 3, row-major R/G/B
```

Malformed frames and unexpected message types close the connection. Stale
sequences are ignored.

### Pressure state, controller → engine (`type = 2`)

```text
8 bytes  pressure sequence
8 bytes  observed Unix nanoseconds
64 bytes pressure bits, row-major; bit 0 is tile (0,0)
```

### Output state, controller → engine (`type = 3`)

```text
8 bytes  complete physical frame transactions sent
8 bytes  desired frame sequence, or zero for black/no frame
8 bytes  state Unix nanoseconds
8 bytes  desired frame age in milliseconds, signed two's complement
4 bytes  IEEE-754 float32 fade ratio
1 byte   flags: UDP available, floor seen, transaction succeeded
8 bytes  UDP write error count
8 bytes  pressure sequence
64 bytes pressure bits
```

Pressure and output use one-element latest-value queues, so a slow engine cannot
block the physical output loop.

## Physical floor behavior

The vendor packet encoder and wiring map remain isolated under `internal/floor`.
Supported room rotations are 0° and 180°, applied consistently to LED output and
pressure input.

One goroutine owns every physical write, preventing sync packets from
interleaving with a frame transaction.

When `-floor-source-ip` is configured, only that exact local IPv4 address is
used. If absent, the service remains online and retries without falling back to
another interface.

A successful UDP write only means the local kernel accepted the datagram.
Telemetry separately reports source assignment, UDP write state, and whether a
valid floor packet was recently observed.

Incomplete aggregate sensor packets are rejected as a unit so omitted channels
cannot leave stale canonical pressure behind.

## Diagnostics UI

The loopback HTTP listener defaults to `127.0.0.1:4200` and serves:

- `GET /` and `GET /status` — live floor diagnostics;
- `GET /state?window=5` — JSON state and per-tile in-memory statistics;
- `GET /events?window=5` — server-sent live updates;
- `GET /health` — process liveness;
- `GET /metrics` — bounded Prometheus telemetry.

The page visualizes desired RGB after the controller fade, canonical pressure,
step/dwell heatmaps, and the actual hardware channel map. Statistics are
in-memory, minute-bucketed, bounded to the most recent 60 minutes, and reset on
process restart.

The service is read-only by default. Development-only pressure simulation and
statistics reset require `-debug-controls`; mutation requests also require a
custom same-origin header to prevent simple cross-origin requests to localhost.
Do not expose debug controls on an untrusted network.

Per-tile heatmap data stays in `/state`; Prometheus exports aggregate counters to
avoid over a thousand fixed tile series on every scrape.

## Run

Development, with physical output safely bound to loopback:

```sh
go run ./cmd/motion-levels-controller -broadcast-ip 127.0.0.1
```

Development with touch simulation:

```sh
go run ./cmd/motion-levels-controller \
  -broadcast-ip 127.0.0.1 \
  -debug-controls
```

Production example:

```sh
go run ./cmd/motion-levels-controller \
  -floor-source-ip 192.168.50.10 \
  -floor-rotation 0
```

Public flags:

- `-http` — diagnostics/health/metrics, default `127.0.0.1:4200`;
- `-engine` — engine stream, default `127.0.0.1:4201`;
- `-recv-port` — floor input, default `7800`;
- `-floor-source-ip` — exact local IPv4 source, optional;
- `-floor-rotation` — `0` or `180`;
- `-broadcast-ip` — LED destination, default `255.255.255.255`;
- `-debug-controls` — enable local simulation/reset endpoints.

## Build and validation

```sh
make check
make native-build
make native-verify
```

`make check` runs formatting checks, the race-enabled tests, `go vet`, and a
static Linux/amd64 build. Native builds embed the exact full Git revision and
produce a binary, SHA-256 sidecar, and metadata document under `dist/`.

## NixOS

The flake exports the package and
`nixosModules.motion-levels-floor-controller`:

```nix
{
  services.motion-levels-floor-controller = {
    enable = true;
    floorSourceIP = "192.168.50.10";
    floorRotation = 0;
  };
}
```

The module uses systemd readiness/watchdog notifications, starts only after all
listeners and the initial black-output attempt are established, runs under a
dynamic user, and applies a restrictive service sandbox.

### Extended validation

The default check is intentionally fast. Before merging controller, wire, sensor,
or diagnostics changes, also run:

```sh
make test-stress  # shuffled race suite, repeated ten times
make fuzz-short   # bounded wire/sensor/mapping fuzz campaigns
make benchmark    # allocation/performance baselines
make web-e2e      # real Chromium diagnostics smoke test
```

`make web-e2e` requires the pinned dependency in `requirements-test.txt` and a
Playwright Chromium installation:

```sh
python3 -m pip install -r requirements-test.txt
python3 -m playwright install chromium
```

CI runs the race suite, short fuzz campaigns, the real-browser smoke test, the
native artifact verifier, and Nix package/module evaluation. The complete
normalized physical-frame fixture under `internal/floor/testdata` must only be
updated after reviewing the vendor packet delta and completing an on-floor smoke
test.

The flake should be shipped with a committed `flake.lock`. Generate or refresh it
from an environment with Nix using `nix flake lock`, review the pinned nixpkgs
revision, then rerun `nix flake check --show-trace` before merge.
