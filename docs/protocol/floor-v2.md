# Floor Protocol v2

Protocol v2 uses one full-duplex, length-prefixed protobuf connection. The
controller listens on TCP port 4203 by default and accepts one engine producer
across both v1 and v2 transports.

## Handshake

1. The engine sends `EngineHello` with `protocol_version = 2`.
2. The adapter replies with `AdapterHello`, its revision, physical logical-grid
   dimensions, and target presentation rate.
3. Any other first message or protocol version closes the connection.

## Engine to adapter

`DesiredFrame` contains only a monotonic technical sequence, render timestamp,
the exact 16×32 dimensions, and packed row-major RGB bytes. Invalid dimensions,
payload lengths, or zero sequence values close the connection. Stale or
out-of-order sequences are ignored.

## Adapter to engine

- `PressureEvent` reports a physical transition with logical coordinates and
  bounded hardware coordinates.
- `PresentedFrame` reports the post-pressure, post-watchdog frame actually sent
  to the floor. It contains packed RGB, a pressure bitset, presentation and
  desired sequences, presentation time, and fade ratio.
- `AdapterStatus` reports bounded presentation health once per second.

Pressure uses a bounded queue. Presented frames and status use latest-value
queues, so a slow engine cannot block the hardware presentation loop.

## Ownership boundary

The protocol contains no game, level, player, run, session, controller ID,
platform URL, token, or storage concepts. The engine joins presented snapshots
to its canonical run/session state before publishing them externally.

Protocol v1 remains enabled during migration and retains its golden-wire tests.
