# Motion Levels Floor Controller

This repository contains the small hardware-facing adapter between the Motion
Levels game engine and the physical 16×32 LED floor.

It deliberately owns only the hardware boundary:

- one local engine connection;
- one latest packed RGB frame;
- physical UDP frame and sync transmission;
- physical pressure parsing and logical-coordinate mapping;
- a frame-freshness watchdog with hold, fade, and black output;
- minimal liveness and Prometheus telemetry.

Game rules, sessions, players, recordings, kiosk/display lifecycle, platform
credentials, updates, and venue orchestration belong in other repositories.

## Runtime model

The engine sends packed row-major RGB frames. The controller keeps only the
latest valid frame and refreshes the physical floor at 50 fps. Frames do not
queue: a newer frame replaces an older one.

Pressure is represented canonically as a 512-bit snapshot rather than as an
unrecoverable stream of individual transitions. The controller sends the full
snapshot when an engine connects and whenever pressure changes.

Only one engine session is active. A new loopback connection replaces the old
one immediately, invalidates its frame generation, and starts from black until
the new engine sends its own valid frame. A delayed old connection can never
publish into the new generation.

## Safety watchdog

Safety depends on locally observed frame age, not TCP connection state.

The engine should resend its current desired frame periodically even when its
visual contents have not changed. With the defaults:

1. a frame lease expires after 500 ms without a new valid frame;
2. the controller holds the last frame for another 2 seconds;
3. it fades linearly to black over 3 seconds;
4. black continues to be physically refreshed.

A frozen engine that leaves its socket open therefore cannot hold a bright stale
frame indefinitely. The controller also sends black on startup and makes a
best-effort final black transmission during orderly shutdown.

## Engine wire contract

There is one current, unversioned full-duplex TCP contract on
`127.0.0.1:4201`. Controller and engine revisions are promoted together in one
atomic venue release, so there is no protocol negotiation or compatibility
matrix inside the runtime.

Every message has this fixed header:

```text
1 byte  message type
4 bytes big-endian payload length
N bytes payload (maximum 4096)
```

All integer fields are unsigned big-endian unless stated otherwise.

### Engine → controller: desired frame (`type = 1`)

```text
8 bytes    positive monotonic sequence
1536 bytes RGB (16 × 32 × 3), row-major, R/G/B order
```

Any other engine message, invalid length, zero sequence, or malformed frame
closes the connection. Stale sequences are ignored.

### Controller → engine: pressure state (`type = 2`)

```text
8 bytes  pressure sequence
8 bytes  observed Unix nanoseconds
64 bytes pressure bits, row-major; bit 0 is tile (0,0)
```

### Controller → engine: output state (`type = 3`)

```text
8 bytes  complete physical frame transactions sent
8 bytes  desired frame sequence, or zero for black/no frame
8 bytes  state timestamp in Unix nanoseconds
8 bytes  desired frame age in milliseconds (signed two's-complement value)
4 bytes  IEEE-754 float32 fade ratio
1 byte   flags: bit 0 UDP write available, bit 1 floor seen recently,
         bit 2 this physical frame transaction succeeded
8 bytes  UDP write error count
8 bytes  pressure sequence
64 bytes pressure bits
```

Output and pressure use one-element latest-value queues. A slow engine cannot
block the physical refresh loop. Pressure messages are prioritized.

## Physical floor behavior

The vendor UDP packet encoder and logical/physical wiring map remain isolated in
`internal/floor`. The supported room orientations are 0° and 180°; LED output
and pressure input use the same transform.

One output goroutine owns all physical writes. Periodic sync packets therefore
cannot interleave with the start/configuration/RGB/end packets of a frame
transaction.

When `-floor-source-ip` is configured, the controller uses only that exact IPv4
source. If it is absent, the process remains online, reports output unavailable,
and retries. It never falls back to another interface. Once the address returns,
output is reacquired without restarting the process.

A successful UDP write means the local kernel accepted the datagram; it is not a
hardware acknowledgement. Telemetry therefore distinguishes:

- exact source assigned;
- UDP write available;
- physical floor packet seen recently.

## Run

For development, always bind output to loopback:

```sh
go run ./cmd/motion-levels-controller -broadcast-ip 127.0.0.1
```

Production example:

```sh
go run ./cmd/motion-levels-controller \
  -floor-source-ip 192.168.50.10 \
  -floor-rotation 0
```

Public flags are intentionally small:

- `-http` — health and metrics listener, default `127.0.0.1:4200`;
- `-engine` — engine stream, default `127.0.0.1:4201`;
- `-recv-port` — physical floor input, default `7800`;
- `-floor-source-ip` — exact local IPv4 source, optional;
- `-floor-rotation` — `0` or `180`;
- `-broadcast-ip` — physical LED destination, default `255.255.255.255`.

The physical destination port (4626), refresh rate (50 fps), and watchdog timing
are controller constants because they are properties of this installation, not
product-level runtime choices.

## Health and metrics

- `GET /health` is process liveness and remains HTTP 200 when hardware is absent.
- `GET /metrics` exposes bounded operational signals.
- Every other path returns 404.

Important metrics include locally received frame age, safety fade, complete
frame transactions sent, exact-source assignment, UDP write state, and whether
a valid physical floor packet was observed recently. No session, player,
controller, or network identity is used as a metric label.

## Build and validation

```sh
make check
make native-build
make native-verify
```

`make check` runs formatting checks, the race-enabled test suite, `go vet`, and a
static Linux/amd64 build. The native build embeds the exact full Git revision and
emits a binary, SHA-256 sidecar, and metadata document under `dist/`.

## Nix and NixOS module

This repository exposes a standard Nix Flake with package and NixOS module definitions:

- **Package**: `packages.${system}.default` / `packages.${system}.motion-levels-controller`
- **NixOS Module**: `nixosModules.default` / `nixosModules.motion-levels-floor-controller`

Example NixOS usage in downstream venue hosts:

```nix
{
  services.motion-levels-floor-controller = {
    enable = true;
    floorSourceIP = "192.168.50.10";
    floorRotation = 0; # or 180
  };
}
```
