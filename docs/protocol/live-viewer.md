# Live Viewer Protocol

The controller preview browser connects to `/ws`.

Websocket text messages are JSON:

- `config`: controller-owned runtime configuration
- `status`: controller health and metrics
- `pressure`: immediate tile press/release changes

Rendered frames are websocket binary messages with this layout:

```text
0..3    magic: "MLF1"
4..7    uint32 little-endian presented frame sequence
8..9    uint16 little-endian logical width
10..11  uint16 little-endian logical height
12      flags; bit 0 means pressure bitset is present
13      reserved
14..15  uint16 little-endian header length, currently 16
16..N   RGB bytes, 3 bytes per tile in row-major x/y order
N..end  pressure bitset, one bit per tile in the same order
```

For the current 16x32 floor, each binary frame is:

```text
16 byte header + 512 * 3 RGB bytes + 64 pressure bytes = 1616 bytes
```

The browser uses binary frames for frequent color refreshes and JSON pressure
events for immediate touch/press feedback.
