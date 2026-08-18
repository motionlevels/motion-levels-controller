package adapter

import (
	"math"
	"testing"
	"time"
)

func TestFrameFadeUsesFrameAgeRatherThanConnectionState(t *testing.T) {
	timeout := 500 * time.Millisecond
	hold := 2 * time.Second
	fade := 3 * time.Second
	cases := []struct {
		age  time.Duration
		want float64
	}{
		{age: 100 * time.Millisecond, want: 0},
		{age: timeout + hold, want: 0},
		{age: timeout + hold + fade/2, want: 0.5},
		{age: timeout + hold + fade*2, want: 1},
	}
	for _, testCase := range cases {
		if got := frameFadeRatio(testCase.age, timeout, hold, fade); math.Abs(got-testCase.want) > 0.001 {
			t.Fatalf("age %s: fade=%f, want %f", testCase.age, got, testCase.want)
		}
	}
}

func TestApplyFadePreservesFullFrameAndReachesBlack(t *testing.T) {
	rgb := []byte{100, 50, 10}
	applyFade(rgb, 0.5)
	if rgb[0] != 50 || rgb[1] != 25 || rgb[2] != 5 {
		t.Fatalf("half fade = %v", rgb)
	}
	applyFade(rgb, 0)
	if rgb[0] != 0 || rgb[1] != 0 || rgb[2] != 0 {
		t.Fatalf("black fade = %v", rgb)
	}
}
