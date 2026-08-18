# AGENTS.md

## Scope

This repository owns only the hardware-facing Motion Levels floor controller:
physical UDP, wiring/rotation mapping, pressure state, the local engine wire
contract, watchdog behavior, health, and its native runtime artifact.

Game rules, game assets, recordings, sessions, players, venue orchestration,
display/kiosk lifecycle, platform APIs, deployment promotion, and automatic
updates belong elsewhere.

## Safety

- Automated and local checks must set LED output to loopback. Never send UDP to
  the production broadcast address during validation.
- Preserve startup black, frame-age hold/fade, replacement-engine invalidation,
  exact-source behavior, and best-effort shutdown black.
- Preserve the vendor packet encoder and logical/physical map unless a change is
  backed by captured physical-floor fixtures and an explicit hardware test.
- The engine contract is intentionally unversioned. A contract change requires
  a coordinated engine change and one atomic venue release; do not add parallel
  compatibility listeners or runtime negotiation.
- Do not add game, product, venue identity, storage, outbound platform clients,
  browser UI, or host-service control to this process.

## Validation

Run `make check` before committing. Physical-floor changes additionally require
a loopback packet check and an on-floor smoke test before merge.
