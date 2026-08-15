package floor

import "testing"

func TestLogicalPhysicalRoundTrip(t *testing.T) {
	for _, rotation := range []Rotation{Rotation0, Rotation180} {
		for y := 0; y < GridHeight; y++ {
			for x := 0; x < GridWidth; x++ {
				physical := LogicalToPhysical(x, y, rotation)
				gotX, gotY := PhysicalToLogical(physical.Controller, physical.Channel, physical.Position, rotation)
				if gotX != x || gotY != y {
					t.Fatalf("rotation %d: round trip (%d,%d) -> %+v -> (%d,%d)", rotation, x, y, physical, gotX, gotY)
				}
			}
		}
	}
}

func TestKnownMappingCorners(t *testing.T) {
	cases := []struct {
		rotation Rotation
		x, y     int
		channel  int
		position int
	}{
		{rotation: Rotation0, x: 0, y: 0, channel: 7, position: 48},
		{rotation: Rotation0, x: 15, y: 0, channel: 7, position: 63},
		{rotation: Rotation0, x: 0, y: 31, channel: 0, position: 15},
		{rotation: Rotation0, x: 15, y: 31, channel: 0, position: 0},
		{rotation: Rotation180, x: 0, y: 0, channel: 0, position: 0},
		{rotation: Rotation180, x: 15, y: 0, channel: 0, position: 15},
		{rotation: Rotation180, x: 0, y: 31, channel: 7, position: 63},
		{rotation: Rotation180, x: 15, y: 31, channel: 7, position: 48},
	}

	for _, tc := range cases {
		got := LogicalToPhysical(tc.x, tc.y, tc.rotation)
		if got.Channel != tc.channel || got.Position != tc.position {
			t.Fatalf("rotation %d: (%d,%d) mapped to %+v, want channel=%d position=%d", tc.rotation, tc.x, tc.y, got, tc.channel, tc.position)
		}
	}
}

func TestSupportedRotations(t *testing.T) {
	for _, degrees := range []int{0, 180} {
		if !IsSupportedRotation(degrees) {
			t.Fatalf("rotation %d should be supported", degrees)
		}
	}
	for _, degrees := range []int{-180, 90, 360} {
		if IsSupportedRotation(degrees) {
			t.Fatalf("rotation %d should be rejected", degrees)
		}
	}
}
