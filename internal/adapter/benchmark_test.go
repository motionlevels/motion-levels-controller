package adapter

import (
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func BenchmarkBuildStateResponse(b *testing.B) {
	cfg := DefaultConfig()
	status := newRuntimeStatus()
	pressure := &pressureStore{observedAt: time.Now()}
	frames := &frameStore{}
	frames.beginGeneration(1)
	_ = frames.store(1, 1, make([]byte, floor.RGBByteCount), time.Now())
	pressure.apply([]pressureChange{{X: 3, Y: 4, Pressed: true}}, time.Now())
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = buildStateResponse(now, cfg, status, pressure, frames, 5, true)
	}
}

func BenchmarkEncodeOutputSnapshot(b *testing.B) {
	var snapshot OutputSnapshot
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = encodeOutputSnapshot(snapshot)
	}
}
