# Controller–engine protocol

The authoritative wire description is in the README. This document records the
design constraints behind it.

- There is one current contract and no version negotiation.
- The 16×32 dimensions are fixed and are not repeated in every message.
- Frames use packed RGB and never become per-tile heap objects in the controller.
- Pressure is a complete bitset snapshot so reconnects and dropped intermediate
  messages heal automatically.
- A new engine connection replaces the old connection and starts a new frame
  generation.
- Frame safety uses the controller's local receipt clock.
- Outbound state uses bounded latest-value queues and cannot block hardware.
- Controller and engine are released atomically by venue orchestration.
