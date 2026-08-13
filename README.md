# Motion Levels Floor Adapter

This repository owns the hardware-facing floor adapter source. The venue
repository pins an exact source revision, builds its static Linux/amd64 binary
with the helper in this repository, and promotes that verified binary into an
atomic venue release. This repository never deploys itself or runs an automatic
updater. The immutable container image remains available for isolated runtime
testing, but is not required by the native venue deployment.

The adapter uses the host network for the floor UDP protocol. Venue packaging
is responsible for its dedicated non-root user, systemd sandbox, writable state
boundary, and loopback-only management listeners.

## Pinned native build

`scripts/build-native.sh` builds only from the checked-out full Git revision,
refuses dirty controller runtime inputs, disables CGO and VCS-dependent build
metadata, embeds that revision, and emits a Linux/amd64 binary plus canonical
SHA-256 and JSON metadata sidecars. The same source, Go toolchain, and build
flags produce the same binary.

```sh
make native-build
make native-verify
```

The files are revision-qualified under `dist/`:

- `motion-levels-controller-linux-amd64-<revision>`;
- `motion-levels-controller-linux-amd64-<revision>.sha256`;
- `motion-levels-controller-linux-amd64-<revision>.metadata.json`.

Ansible should check out the pinned source revision, set
`CONTROLLER_EXPECTED_GO_VERSION` to the venue lock's exact Go version, run the
builder, verify the metadata, then install the binary by its recorded SHA-256.
The verifier also requires a static AMD64 ELF and confirms that the full source
revision is embedded in the executable.

## Responsibility boundary

The adapter:

- sends and receives the physical UDP floor protocol;
- maps logical tiles to physical controller/channel/position coordinates;
- parses and deduplicates physical pressure;
- keeps only the latest desired RGB frame;
- refreshes the physical LEDs at its own cadence, normally 50 fps;
- permits only one engine producer;
- holds and fades the last frame safely if the engine disappears;
- reports physical pressure, actual-presented frames, and bounded health;
- serves only `/health` and `/metrics`.

It deliberately has no game, player, run, session, venue identity, platform
credential, outbound HTTP client, browser UI, WebSocket, pressure simulation,
recording, replay, storage, or product state. Those responsibilities belong to
the game engine and platform.

The adapter remains a separate process and network boundary so engine, game,
audio, or UI failures cannot bypass the physical watchdog or expose the floor
hardware network.

## Protocols

Protocol v2 is preferred and uses one length-prefixed protobuf connection at
`127.0.0.1:4203`. Both peers exchange an explicit versioned hello. The engine
then sends packed-RGB `DesiredFrame` messages; the adapter returns physical
`PressureEvent`, post-watchdog `PresentedFrame`, and bounded
`AdapterStatus` messages.

`PresentedFrame` contains the RGB and pressure bits actually sent to the floor,
including safety fade. The engine uses it to publish the observed live-floor
view. Protocol v2 contains no product or platform identity fields and rejects
oversized envelopes before allocation.

The legacy v1 frame and pressure streams remain available temporarily at ports
4201 and 4202 for rollback compatibility. Only one v1 or v2 producer is accepted
at a time.

See [docs/protocol/floor-v2.md](docs/protocol/floor-v2.md) and
[docs/protocol/pressure-events.md](docs/protocol/pressure-events.md).

## Run

Use loopback LED output for development:

```sh
go run ./cmd/motion-levels-controller -broadcast-ip 127.0.0.1
```

Use the default broadcast address only on the real floor network:

```sh
go run ./cmd/motion-levels-controller
```

Defaults:

- health/metrics HTTP: `127.0.0.1:4101`;
- legacy frame listener: `127.0.0.1:4201`;
- legacy pressure listener: `127.0.0.1:4202`;
- protocol-v2 duplex listener: `127.0.0.1:4203`;
- floor UDP receive socket: `:7800`;
- LED broadcast: `255.255.255.255:4626`;
- physical refresh: `50fps`;
- engine disconnect: hold for `2s`, then fade to black over `3s`.

## Health and metrics

- `GET /health`: process liveness only.
- `GET /metrics`: bounded Prometheus signals for presentation cadence, engine
  connectivity and frame age, safety fade, UDP errors, clock/latency/jitter,
  memory, goroutines, uptime, and build revision.
- Every other HTTP path returns 404.

The engine exposes the aggregate floor-adapter status for venue and platform
consumers. Adapter metrics never use controller, session, player, or dynamic
network identifiers as labels.

The process handles `SIGINT` and `SIGTERM` for clean shutdown.
