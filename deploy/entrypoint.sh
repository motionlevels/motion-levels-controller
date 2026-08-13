#!/bin/sh
set -eu

if [ -n "${MOTION_LEVELS_PLATFORM_TOKEN_FILE:-}" ]; then
  MOTION_LEVELS_PLATFORM_TOKEN="$(cat "$MOTION_LEVELS_PLATFORM_TOKEN_FILE")"
  export MOTION_LEVELS_PLATFORM_TOKEN
fi

exec /app/bin/motion-levels-controller \
  -http "${MOTION_LEVELS_CONTROLLER_HTTP:-127.0.0.1:4101}" \
  -frames "${MOTION_LEVELS_CONTROLLER_FRAMES:-127.0.0.1:4201}" \
  -input-events "${MOTION_LEVELS_CONTROLLER_INPUT_EVENTS:-127.0.0.1:4202}" \
  -duplex "${MOTION_LEVELS_CONTROLLER_DUPLEX:-127.0.0.1:4203}" \
  -recv-port "${MOTION_LEVELS_FLOOR_RECV_PORT:-7800}" \
  -floor-source-ip "${MOTION_LEVELS_FLOOR_SOURCE_IP:-}" \
  -broadcast-ip "${MOTION_LEVELS_LED_BROADCAST_IP:-255.255.255.255}" \
  -broadcast-port "${MOTION_LEVELS_LED_BROADCAST_PORT:-4626}" \
  -controller-id-file /var/lib/motion-levels/floor-controller/controller-id \
  -refresh-fps "${MOTION_LEVELS_REFRESH_FPS:-50}" \
  -engine-fade-delay 2s \
  -engine-fade-duration 3s \
  "$@"
