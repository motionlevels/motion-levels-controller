package adapter

import (
	"net"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestFrameStoreRejectsRetiredEngineGeneration(t *testing.T) {
	store := &frameStore{}
	store.beginGeneration(1)
	rgb := make([]byte, floor.RGBByteCount)
	rgb[0] = 9
	if !store.store(1, 1, rgb, time.Now()) {
		t.Fatal("active generation frame should be stored")
	}
	store.beginGeneration(2)
	if _, ok := store.snapshot(); ok {
		t.Fatal("new engine generation must invalidate the previous frame")
	}
	if store.store(1, 2, rgb, time.Now()) {
		t.Fatal("retired engine generation published a frame")
	}
}

func TestNewestEngineConnectionWinsAndClearsFrame(t *testing.T) {
	cfg := DefaultConfig()
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Now()}
	status := newRuntimeStatus()
	hub := newEngineHub(cfg, frames, pressure, status)

	server1, client1 := net.Pipe()
	defer client1.Close()
	first := hub.attach(server1)
	defer first.close()
	rgb := make([]byte, floor.RGBByteCount)
	if !frames.store(first.generation, 1, rgb, time.Now()) {
		t.Fatal("first engine could not publish")
	}

	server2, client2 := net.Pipe()
	defer client2.Close()
	second := hub.attach(server2)
	defer second.close()
	if first.generation == second.generation {
		t.Fatal("engine generations were not advanced")
	}
	if _, ok := frames.snapshot(); ok {
		t.Fatal("replacement engine resurrected the previous frame")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("previous engine connection was not closed")
	}
}

func TestPressureStorePublishesCanonicalSnapshot(t *testing.T) {
	store := &pressureStore{observedAt: time.Unix(0, 1)}
	snapshot, changed := store.apply([]pressureChange{{X: 3, Y: 4, Pressed: true}}, time.Unix(0, 2))
	if !changed || snapshot.Sequence != 1 {
		t.Fatalf("unexpected first pressure update: changed=%v snapshot=%+v", changed, snapshot)
	}
	index := 4*floor.GridWidth + 3
	if snapshot.Bits[index/8]&(1<<uint(index%8)) == 0 {
		t.Fatal("canonical pressure bit was not set")
	}
	unchanged, changed := store.apply([]pressureChange{{X: 3, Y: 4, Pressed: true}}, time.Unix(0, 3))
	if changed || unchanged.Sequence != 1 {
		t.Fatalf("duplicate pressure changed sequence: changed=%v snapshot=%+v", changed, unchanged)
	}
	released, changed := store.apply([]pressureChange{{X: 3, Y: 4, Pressed: false}}, time.Unix(0, 4))
	if !changed || released.Sequence != 2 || released.Bits[index/8]&(1<<uint(index%8)) != 0 {
		t.Fatalf("release did not heal canonical state: %+v", released)
	}
}

func TestPressureStoreTracksTilePressesAndDuration(t *testing.T) {
	store := &pressureStore{observedAt: time.Unix(1000, 0)}
	initStats := store.statsSnapshot(time.Unix(1000, 0))
	if initStats.ActivePressedTiles != 0 {
		t.Fatalf("initial active tiles = %d, want 0", initStats.ActivePressedTiles)
	}

	idx := 5*floor.GridWidth + 2
	if initStats.Tiles[idx].Presses != 0 || initStats.Tiles[idx].PressedDurationSec != 0 {
		t.Fatalf("initial tile stats non-zero: %+v", initStats.Tiles[idx])
	}

	// Press (2, 5) at t = 1000s
	store.apply([]pressureChange{{X: 2, Y: 5, Pressed: true}}, time.Unix(1000, 0))

	// Inspect while still pressed at t = 1002.5s
	midStats := store.statsSnapshot(time.Unix(1002, 500_000_000))
	if midStats.ActivePressedTiles != 1 {
		t.Fatalf("active tiles during press = %d, want 1", midStats.ActivePressedTiles)
	}
	if midStats.Tiles[idx].Presses != 1 {
		t.Fatalf("press count = %d, want 1", midStats.Tiles[idx].Presses)
	}
	if midStats.Tiles[idx].PressedDurationSec != 2.5 {
		t.Fatalf("pressed duration = %v, want 2.5", midStats.Tiles[idx].PressedDurationSec)
	}

	// Release (2, 5) at t = 1003s
	store.apply([]pressureChange{{X: 2, Y: 5, Pressed: false}}, time.Unix(1003, 0))

	// Inspect after release at t = 1010s
	releasedStats := store.statsSnapshot(time.Unix(1010, 0))
	if releasedStats.ActivePressedTiles != 0 {
		t.Fatalf("active tiles after release = %d, want 0", releasedStats.ActivePressedTiles)
	}
	if releasedStats.Tiles[idx].Presses != 1 {
		t.Fatalf("press count after release = %d, want 1", releasedStats.Tiles[idx].Presses)
	}
	if releasedStats.Tiles[idx].PressedDurationSec != 3.0 {
		t.Fatalf("pressed duration after release = %v, want 3.0", releasedStats.Tiles[idx].PressedDurationSec)
	}

	// Second press at t = 1010s, release at t = 1012s
	store.apply([]pressureChange{{X: 2, Y: 5, Pressed: true}}, time.Unix(1010, 0))
	store.apply([]pressureChange{{X: 2, Y: 5, Pressed: false}}, time.Unix(1012, 0))

	finalStats := store.statsSnapshot(time.Unix(1015, 0))
	if finalStats.Tiles[idx].Presses != 2 {
		t.Fatalf("final press count = %d, want 2", finalStats.Tiles[idx].Presses)
	}
	if finalStats.Tiles[idx].PressedDurationSec != 5.0 {
		t.Fatalf("final duration = %v, want 5.0", finalStats.Tiles[idx].PressedDurationSec)
	}
}
