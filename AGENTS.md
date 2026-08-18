# AGENTS.md

## Scope

This repository owns the hardware-facing Motion Levels floor controller:
physical UDP, wiring/rotation mapping, canonical pressure state, the local
engine wire contract, watchdog behavior, a loopback diagnostics UI, health,
metrics, and native/Nix runtime packaging.

Game rules, game assets, recordings, product sessions, players, venue
orchestration, kiosk/display lifecycle, platform APIs, deployment promotion,
and automatic updates belong elsewhere. The embedded page is an operational
hardware diagnostic, not a product UI.

## Safety

- Automated and local checks must set LED output to loopback. Never send UDP to
  the production broadcast address during validation.
- Preserve startup black, frame-age hold/fade, replacement-engine invalidation,
  exact-source behavior, and best-effort shutdown black.
- Preserve the vendor packet encoder and logical/physical map unless a change is
  backed by captured physical-floor fixtures and an explicit hardware test.
- The engine contract is intentionally unversioned. Contract changes require a
  coordinated engine change and one atomic venue release; do not add parallel
  compatibility listeners or runtime negotiation.
- Diagnostics are read-only by default. Pressure simulation and statistics reset
  must remain behind the explicit `-debug-controls` switch and loopback HTTP.
- Do not add venue identity, storage, outbound platform clients, or host-service
  control to the process.

## Validation

Run `make check` before committing. Physical-floor changes additionally require
a loopback packet check and an on-floor smoke test before merge. Nix changes
should pass `nix flake check` in an environment with Nix available.

## Test matrix

- `make check` is mandatory for every change.
- `make test-stress` is required for concurrency/lifecycle changes.
- `make fuzz-short` is required for wire, packet-parser, or coordinate changes.
- `make web-e2e` is required for embedded diagnostics changes.
- `nix flake check --show-trace` is required for flake/module changes.
- Keep `flake.lock` committed and review nixpkgs lock updates explicitly.
- Do not regenerate `internal/floor/testdata/frame_transaction_v1.bin` merely to
  make a test pass; treat it as a hardware wire-contract change.
