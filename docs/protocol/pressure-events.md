# Pressure Events

`floor-controller` keeps pressure state independently from rendered frames.

The game engine receives live pressure changes as event messages. The adapter
also samples current pressure into each protocol-v2 presented frame so the
engine-owned live view represents what was physically presented.

The protobuf contract is:

```proto
message PressureEvent {
  uint64 sequence = 1;
  int64 unix_nanos = 2;
  uint32 x = 3;
  uint32 y = 4;
  bool pressed = 5;
  string source = 6;
  uint32 controller = 7;
  uint32 channel = 8;
  uint32 position = 9;
}
```

The live stream is:

```text
floor-controller -> game-engine
```

This stream is event-driven and low latency. In protocol v1 it is separate from
the engine-to-controller `FrameRecord` frame stream.
