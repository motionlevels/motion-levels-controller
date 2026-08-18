package floor

import "testing"

func FuzzLogicalPhysicalRoundTrip(f *testing.F) {
	f.Add(0, 0, 0)
	f.Add(GridWidth-1, GridHeight-1, 180)
	f.Fuzz(func(t *testing.T, x, y, rotationValue int) {
		if !InLogicalBounds(x, y) {
			return
		}
		rotation := Rotation0
		if rotationValue&1 == 1 {
			rotation = Rotation180
		}
		physical := LogicalToPhysical(x, y, rotation)
		gotX, gotY := PhysicalToLogical(physical.Controller, physical.Channel, physical.Position, rotation)
		if gotX != x || gotY != y {
			t.Fatalf("round trip (%d,%d) -> %+v -> (%d,%d)", x, y, physical, gotX, gotY)
		}
	})
}
