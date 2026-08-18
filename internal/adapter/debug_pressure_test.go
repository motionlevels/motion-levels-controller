package adapter

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestDebugPressLeaseExpiresWithoutClearingPhysicalPressure(t *testing.T) {
	store := &pressureStore{observedAt: time.Unix(100, 0)}
	start := time.Unix(100, 0)
	store.apply([]pressureChange{{X: 3, Y: 4, Pressed: true}}, start)
	store.applyDebug([]pressureChange{{X: 3, Y: 4, Pressed: true}}, start, time.Second)
	if _, changed := store.expireDebug(start.Add(2 * time.Second)); changed {
		t.Fatal("expiring diagnostic layer changed canonical physical pressure")
	}
	if !store.snapshot().IsPressed(3, 4) {
		t.Fatal("diagnostic expiry cleared a physical press")
	}
}

func TestDebugPressLeaseReleasesAbandonedSimulatedTouch(t *testing.T) {
	store := &pressureStore{observedAt: time.Unix(100, 0)}
	start := time.Unix(100, 0)
	pressed, changed := store.applyDebug([]pressureChange{{X: 1, Y: 2, Pressed: true}}, start, time.Second)
	if !changed || !pressed.IsPressed(1, 2) {
		t.Fatal("diagnostic press was not applied")
	}
	if _, changed := store.expireDebug(start.Add(500 * time.Millisecond)); changed {
		t.Fatal("diagnostic press expired before its lease")
	}
	released, changed := store.expireDebug(start.Add(time.Second))
	if !changed || released.IsPressed(1, 2) {
		t.Fatal("abandoned diagnostic press did not expire")
	}
}

func TestRepeatedDebugPressRenewsLease(t *testing.T) {
	store := &pressureStore{observedAt: time.Unix(100, 0)}
	start := time.Unix(100, 0)
	store.applyDebug([]pressureChange{{X: 1, Y: 2, Pressed: true}}, start, time.Second)
	store.applyDebug([]pressureChange{{X: 1, Y: 2, Pressed: true}}, start.Add(750*time.Millisecond), time.Second)
	if _, changed := store.expireDebug(start.Add(1200 * time.Millisecond)); changed {
		t.Fatal("renewed diagnostic press expired at its original deadline")
	}
	if !store.snapshot().IsPressed(1, 2) {
		t.Fatal("renewed diagnostic press was cleared")
	}
}

func TestDebugPressureLeaseLoopPublishesExpiry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DebugPressLease = 50 * time.Millisecond
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Now()}
	status := newRuntimeStatus()
	hub := newEngineHub(cfg, frames, pressure, status)
	server, client := net.Pipe()
	defer client.Close()
	session := hub.attach(server)
	defer session.close()
	// Drain the initial pressure snapshot.
	<-session.pressure

	pressure.applyDebug([]pressureChange{{X: 0, Y: 0, Pressed: true}}, time.Now(), cfg.DebugPressLease)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (&debugPressureLeaseLoop{cfg: cfg, pressure: pressure, hub: hub, status: status}).run(ctx)
	}()

	select {
	case snapshot := <-session.pressure:
		if snapshot.IsPressed(0, 0) {
			t.Fatal("lease loop published a still-pressed snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("lease loop did not publish diagnostic expiry")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
