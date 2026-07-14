# Pressure Events

`floor-controller` keeps pressure state independently from rendered frames.

Recordings store pressure sampled into each controller-presented frame. The
game engine should receive live pressure changes as event messages instead of
reading sampled pressure from recordings.

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

This stream is event-driven and low latency. It is separate from the
controller-presented `FrameRecord` recording stream.
