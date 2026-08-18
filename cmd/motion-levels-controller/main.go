package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/motionlevels/motion-levels-controller/internal/adapter"
	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func main() {
	cfg := adapter.DefaultConfig()
	var receivePort int
	var rotation int
	flag.StringVar(&cfg.HTTPAddr, "http", cfg.HTTPAddr, "loopback HTTP address for health and metrics")
	flag.StringVar(&cfg.EngineAddr, "engine", cfg.EngineAddr, "loopback TCP address for the single engine stream")
	flag.IntVar(&receivePort, "recv-port", 7800, "UDP port for floor handshake and sensor packets")
	flag.StringVar(&cfg.FloorSourceIP, "floor-source-ip", os.Getenv("MOTION_LEVELS_FLOOR_SOURCE_IP"), "exact local IPv4 source for floor UDP output; empty uses the default route")
	flag.IntVar(&rotation, "floor-rotation", 0, "logical-to-physical floor rotation in degrees (0 or 180)")
	flag.StringVar(&cfg.BroadcastIP, "broadcast-ip", cfg.BroadcastIP, "floor LED broadcast IPv4 address")
	flag.BoolVar(&cfg.EnableDebugControls, "debug-controls", false, "enable loopback-only pressure simulation and statistics reset endpoints")
	flag.Parse()

	cfg.ReceiveAddr = fmt.Sprintf(":%d", receivePort)
	cfg.FloorRotation = floor.Rotation(rotation)
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	log.Printf("config: http=%s engine=%s floor-input=%s source=%s rotation=%d broadcast=%s:%d refresh=%dfps debug-controls=%v", cfg.HTTPAddr, cfg.EngineAddr, cfg.ReceiveAddr, cfg.FloorSourceIP, cfg.FloorRotation, cfg.BroadcastIP, cfg.BroadcastPort, cfg.RefreshFPS, cfg.EnableDebugControls)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := adapter.Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
