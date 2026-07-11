# AGENTS.md

## Scope

This repository owns the hardware-facing Motion Levels floor controller, its
wire-contract snapshot, and its immutable runtime image. Game rules, game
assets, venue orchestration, and platform APIs belong in their respective
repositories.

## Safety

- Automated and local tests must bind the LED output to loopback. Never send
  UDP to the production broadcast address during a read-only check.
- Do not invoke `/tv/refresh` on a live venue during validation.
- Preserve the controller identity path
  `/var/lib/motion-levels/floor-controller/controller-id`.
- Changes to protobuf field numbers, protobuf package names, `pbstream`, or the
  `MLF1` browser frame require an explicit protocol-version decision.
- Images are published for full commit SHAs and are promoted manually by the
  platform repository. Do not add an automatic deployment or updater.

## Validation

Run `make check` before committing. Use `make docker-build` for image changes.
