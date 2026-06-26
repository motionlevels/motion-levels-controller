package floor

import "testing"

func TestLogicalPhysicalRoundTrip(t *testing.T) {
	for y := 0; y < GridHeight; y++ {
		for x := 0; x < GridWidth; x++ {
			physical := LogicalToPhysical(x, y)
			gotX, gotY := PhysicalToLogical(physical.Controller, physical.Channel, physical.Position)
			if gotX != x || gotY != y {
				t.Fatalf("round trip (%d,%d) -> %+v -> (%d,%d)", x, y, physical, gotX, gotY)
			}
		}
	}
}

func TestKnownMappingCorners(t *testing.T) {
	cases := []struct {
		x, y     int
		channel  int
		position int
	}{
		{x: 0, y: 0, channel: 7, position: 48},
		{x: 15, y: 0, channel: 7, position: 63},
		{x: 0, y: 31, channel: 0, position: 15},
		{x: 15, y: 31, channel: 0, position: 0},
	}

	for _, tc := range cases {
		got := LogicalToPhysical(tc.x, tc.y)
		if got.Channel != tc.channel || got.Position != tc.position {
			t.Fatalf("(%d,%d) mapped to %+v, want channel=%d position=%d", tc.x, tc.y, got, tc.channel, tc.position)
		}
	}
}
