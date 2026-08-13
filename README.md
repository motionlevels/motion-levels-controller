# Motion Levels Controller

This repository owns the floor controller source and the immutable
`ghcr.io/motionlevels/motion-levels-controller:sha-<full-commit>` image. The
venue repository promotes an exact controller revision into an atomic venue
release; this repository never deploys itself or runs an automatic updater.

The controller supports the legacy pair of protocol-v1 protobuf streams and
the protocol-v2 duplex stream during migration. The browser preview remains
`MLF1`. Schemas live under `contracts/` so changes are explicit and reviewable.
The controller uses host networking for UDP floor broadcast, but the production
container is non-root, read-only, capability-free, and has no device mappings.

The floor controller is the local hardware-facing service.

Display and kiosk status, restart, and host service management belong to venue
host tooling. The confined controller container does not inspect or control
host systemd services.

It does not produce game animations. It receives logical board frames from
`game-engine` over a length-prefixed protobuf stream, then:

- keeps the latest frame in memory
- refreshes the physical LED floor via UDP at a controller-owned cadence
- serves the browser websocket preview
- merges tile pressure state into the live preview
- parses real floor sensor packets
- accepts browser/touchscreen press simulation

Only one `game-engine` frame stream is accepted at a time. If a second engine
connects by accident while one is already active, the controller closes the new
connection immediately so streams cannot race each other.

## Run

For local preview without touching the real floor:

```sh
go run ./cmd/motion-levels-controller -http 127.0.0.1:4101 -broadcast-ip 127.0.0.1
```

Open:

```text
http://127.0.0.1:4101/
```

The server listens on all interfaces when the HTTP address starts with `:`, so
other machines on the same network can open the LAN URL printed at startup.

For a real floor on the local network, use the default broadcast address:

```sh
go run ./cmd/motion-levels-controller
```

The defaults are:

- HTTP preview: `127.0.0.1:4101`
- frame stream TCP listener: `127.0.0.1:4201`
- pressure event stream: `127.0.0.1:4202`
- protocol-v2 duplex stream: `127.0.0.1:4203`
- UDP receive socket: `:7800`
- LED broadcast: `255.255.255.255:4626`
- controller ID file: `.motion-levels-controller-id`
- output refresh: `50fps`
- game-engine disconnect fade: hold `2s`, then fade to black over `3s`

The controller ID is generated once and reused on later starts:

```sh
go run ./cmd/motion-levels-controller -controller-id-file .motion-levels-controller-id
```

For a deployed controller, keep this file outside the git checkout so updates do
not change the identity:

```sh
go run ./cmd/motion-levels-controller \
  -controller-id-file /var/lib/motion-levels/floor-controller/controller-id
```

Tune the hardware and preview cadence with:

```sh
go run ./cmd/motion-levels-controller -refresh-fps 50
```

If the game-engine connection drops, the controller keeps refreshing the latest
frame briefly, then fades the hardware and live preview to black:

```sh
go run ./cmd/motion-levels-controller -engine-fade-delay 2s -engine-fade-duration 3s
```

## Status

The controller exposes live operational status as JSON:

```text
http://127.0.0.1:4101/status
```

Prometheus metrics for the same bounded operational signals are available at
`http://127.0.0.1:4101/metrics`. The endpoint includes presentation rate and
latency, engine connectivity/frame freshness, UDP errors, clock sync, preview
clients and Go process health. It never uses controller, session, or player
identifiers as metric labels.

Status includes presented frame count, measured FPS, latest game-frame age,
game-engine connection/fade state, websocket client count, UDP send errors, and
controller ID.
It also includes passive sync health derived from game-frame timestamps and the
controller receive/presentation clock:

```json
{
  "sync": {
    "status": "ok",
    "engineClockOffsetMs": 1.8,
    "presentLatencyMs": 7.4,
    "jitterMs": 2.1
  }
}
```

For same-PC controller and game-engine deployments this should remain small and
stable. If the components move to separate machines later, this status gives us
a non-intrusive warning before replay alignment becomes questionable.

The process handles `SIGINT` and `SIGTERM` for a clean shutdown.

## Live Viewer Protocol

The browser viewer uses websocket text messages for control and input events:

- `config`: controller-owned runtime settings
- `pressure`: immediate press/release changes from the floor or browser

Rendered frames are websocket binary messages. Each binary frame starts with a
fixed `MLF1` header, followed by packed RGB bytes and a pressure bitset. This
keeps the preview lightweight without adding protobuf tooling to the browser.

See `docs/protocol/live-viewer.md` and `docs/protocol/pressure-events.md` for
the protocol details.

## Floor Protocol v2

The preferred engine connection is one length-prefixed protobuf stream at
`127.0.0.1:4203`. Both peers first exchange a versioned hello. The engine then
sends packed RGB `DesiredFrame` messages; the adapter returns physical
`PressureEvent`, post-watchdog `PresentedFrame`, and bounded `AdapterStatus`
messages on the same connection.

`PresentedFrame` contains the RGB and pressure bits actually sent to the floor,
including safety fade. The engine can therefore publish the existing observed
live-floor view without giving the adapter platform identity or credentials.
Protocol v2 deliberately contains no game, player, run, session, or platform
fields. The v1 listeners remain available only for a compatibility window.
