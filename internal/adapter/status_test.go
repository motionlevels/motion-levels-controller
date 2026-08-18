package adapter

import (
	"math"
	"testing"
	"time"
)

func TestFrameRateNormalizesElapsedTime(t *testing.T) {
	if got := frameRate(50, time.Second); got != 50 {
		t.Fatalf("rate=%v, want 50", got)
	}
	if got := frameRate(75, 1500*time.Millisecond); math.Abs(got-50) > 1e-9 {
		t.Fatalf("rate=%v, want 50", got)
	}
	if got := frameRate(50, 0); got != 0 {
		t.Fatalf("zero-duration rate=%v, want 0", got)
	}
}
