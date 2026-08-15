package floor

type PhysicalPoint struct {
	Controller int
	Channel    int
	Position   int
}

type Rotation int

const (
	Rotation0   Rotation = 0
	Rotation180 Rotation = 180
)

func IsSupportedRotation(degrees int) bool {
	return degrees == int(Rotation0) || degrees == int(Rotation180)
}

func PhysicalToLogical(controller, channel, position int, rotation Rotation) (x, y int) {
	blockRow := 7 - channel
	startY := blockRow * 4
	dy := 3 - (position / 16)

	dx := position % 16
	if dy%2 == 0 {
		dx = 15 - dx
	}

	// Mirror the logical X axis left/right before applying the room orientation:
	// the floor is wired the opposite way round from how the controller and
	// platform displays render it. LogicalToPhysical applies the inverse
	// transforms in reverse order — keep the two in sync.
	x, y = GridWidth-1-dx, startY+dy
	return rotateLogical(x, y, rotation)
}

func LogicalToPhysical(x, y int, rotation Rotation) PhysicalPoint {
	// A half turn is its own inverse, so the same transform converts the
	// room-oriented logical coordinates back into the wiring-oriented grid.
	x, y = rotateLogical(x, y, rotation)

	// Inverse of the left/right mirror in PhysicalToLogical.
	x = GridWidth - 1 - x

	blockRow := y / 4
	channel := 7 - blockRow
	dy := y % 4
	posBase := (3 - dy) * 16

	position := posBase + x
	if dy%2 == 0 {
		position = posBase + (15 - x)
	}

	return PhysicalPoint{Controller: 0, Channel: channel, Position: position}
}

func rotateLogical(x, y int, rotation Rotation) (int, int) {
	if rotation == Rotation180 {
		return GridWidth - 1 - x, GridHeight - 1 - y
	}
	return x, y
}

func InLogicalBounds(x, y int) bool {
	return x >= 0 && x < GridWidth && y >= 0 && y < GridHeight
}
