package adapter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunStartsWithoutPhysicalFloorAndStopsCleanly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.EngineAddr = "127.0.0.1:0"
	cfg.ReceiveAddr = "127.0.0.1:0"
	cfg.FloorSourceIP = "192.0.2.55"
	cfg.BroadcastIP = "127.0.0.1"
	cfg.RefreshFPS = 100
	cfg.SourceRetry = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case err := <-done:
		t.Fatalf("adapter exited while floor source was absent: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter did not stop cleanly")
	}
}
