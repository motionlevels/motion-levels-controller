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
	if !store.store(1, 1, rgb, time.Now()) {
		t.Fatal("active generation frame should be stored")
	}
	store.beginGeneration(2)
	if _, ok := store.snapshot(); ok {
		t.Fatal("new generation must invalidate the previous frame")
	}
	if store.store(1, 2, rgb, time.Now()) {
		t.Fatal("retired generation published a frame")
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
	if !frames.store(first.generation, 1, make([]byte, floor.RGBByteCount), time.Now()) {
		t.Fatal("first engine could not publish")
	}
	server2, client2 := net.Pipe()
	defer client2.Close()
	second := hub.attach(server2)
	defer second.close()
	if _, ok := frames.snapshot(); ok {
		t.Fatal("replacement engine retained an old frame")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("previous engine was not closed")
	}
}

func TestPressureStorePublishesCanonicalSnapshot(t *testing.T) {
	store := &pressureStore{observedAt: time.Unix(0, 1)}
	snapshot, changed := store.apply([]pressureChange{{X: 3, Y: 4, Pressed: true}}, time.Unix(0, 2))
	if !changed || snapshot.Sequence != 1 || !snapshot.IsPressed(3, 4) {
		t.Fatalf("unexpected press snapshot: changed=%v snapshot=%+v", changed, snapshot)
	}
	unchanged, changed := store.apply([]pressureChange{{X: 3, Y: 4, Pressed: true}}, time.Unix(0, 3))
	if changed || unchanged.Sequence != 1 {
		t.Fatalf("duplicate pressure changed sequence: %+v", unchanged)
	}
	released, changed := store.apply([]pressureChange{{X: 3, Y: 4, Pressed: false}}, time.Unix(0, 4))
	if !changed || released.Sequence != 2 || released.IsPressed(3, 4) {
		t.Fatalf("unexpected release snapshot: %+v", released)
	}
}

func TestPressureStoreTracksLifetimeStats(t *testing.T) {
	store := &pressureStore{observedAt: time.Unix(1000, 0)}
	index := 5*floor.GridWidth + 2
	store.apply([]pressureChange{{X: 2, Y: 5, Pressed: true}}, time.Unix(1000, 0))
	mid := store.statsSnapshot(time.Unix(1002, 500_000_000), 0)
	if mid.ActivePressedTiles != 1 || mid.Tiles[index].Presses != 1 || mid.Tiles[index].PressedDurationSec != 2.5 {
		t.Fatalf("unexpected active stats: %+v", mid.Tiles[index])
	}
	store.apply([]pressureChange{{X: 2, Y: 5, Pressed: false}}, time.Unix(1003, 0))
	final := store.statsSnapshot(time.Unix(1010, 0), 0)
	if final.ActivePressedTiles != 0 || final.Tiles[index].PressedDurationSec != 3 {
		t.Fatalf("unexpected final stats: %+v", final.Tiles[index])
	}
}

func TestRollingDwellDoesNotOverflowAfterFourSeconds(t *testing.T) {
	start := time.Unix(6000, 0)
	store := &pressureStore{observedAt: start}
	store.apply([]pressureChange{{X: 1, Y: 1, Pressed: true}}, start)
	store.apply([]pressureChange{{X: 1, Y: 1, Pressed: false}}, start.Add(30*time.Second))
	index := floor.GridWidth + 1
	got := store.statsSnapshot(start.Add(30*time.Second), 5).Tiles[index].PressedDurationSec
	if got != 30 {
		t.Fatalf("rolling dwell=%v, want 30 seconds", got)
	}
}

func TestRollingDwellIsSplitAcrossMinuteBuckets(t *testing.T) {
	start := time.Unix(60*100+50, 0)
	end := start.Add(20 * time.Second) // ten seconds in each of two minutes
	store := &pressureStore{observedAt: start}
	store.apply([]pressureChange{{X: 4, Y: 3, Pressed: true}}, start)
	store.apply([]pressureChange{{X: 4, Y: 3, Pressed: false}}, end)
	index := 3*floor.GridWidth + 4
	currentMinute := store.statsSnapshot(end, 1).Tiles[index].PressedDurationSec
	if currentMinute != 10 {
		t.Fatalf("current-minute dwell=%v, want 10", currentMinute)
	}
	twoMinutes := store.statsSnapshot(end, 2).Tiles[index].PressedDurationSec
	if twoMinutes != 20 {
		t.Fatalf("two-minute dwell=%v, want 20", twoMinutes)
	}
}

func TestRollingWindowClampsOngoingPressToWindow(t *testing.T) {
	start := time.Unix(60*100, 0)
	now := start.Add(10 * time.Minute)
	store := &pressureStore{observedAt: start}
	store.apply([]pressureChange{{X: 0, Y: 0, Pressed: true}}, start)
	got := store.statsSnapshot(now, 5).Tiles[0].PressedDurationSec
	if got != 5*60 {
		t.Fatalf("5-minute ongoing dwell=%v, want 300", got)
	}
}

func TestResetWhilePressedRestartsDwellAtResetTime(t *testing.T) {
	start := time.Unix(1000, 0)
	resetAt := start.Add(10 * time.Second)
	store := &pressureStore{observedAt: start}
	store.apply([]pressureChange{{X: 6, Y: 7, Pressed: true}}, start)
	store.resetStats(resetAt)
	index := 7*floor.GridWidth + 6
	stats := store.statsSnapshot(resetAt.Add(3*time.Second), 0)
	if stats.ActivePressedTiles != 1 || stats.Tiles[index].Presses != 0 || stats.Tiles[index].PressedDurationSec != 3 {
		t.Fatalf("unexpected post-reset stats: %+v", stats.Tiles[index])
	}
}

func TestPressureSubscriptionIsCoalescedAndUnsubscribes(t *testing.T) {
	store := &pressureStore{observedAt: time.Now()}
	updates, unsubscribe := store.subscribe()
	store.apply([]pressureChange{{X: 1, Y: 1, Pressed: true}}, time.Now())
	store.resetStats(time.Now())
	select {
	case <-updates:
	default:
		t.Fatal("subscriber did not receive update")
	}
	unsubscribe()
	unsubscribe()
	store.apply([]pressureChange{{X: 1, Y: 1, Pressed: false}}, time.Now())
	select {
	case <-updates:
		t.Fatal("unsubscribed channel received update")
	default:
	}
}
