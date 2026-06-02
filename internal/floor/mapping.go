package floor

type PhysicalPoint struct {
	Controller int
	Channel    int
	Position   int
}

func PhysicalToLogical(controller, channel, position int) (x, y int) {
	blockRow := 7 - channel
	startY := blockRow * 4
	dy := 3 - (position / 16)

	dx := position % 16
	if dy%2 == 0 {
		dx = 15 - dx
	}

	return dx, startY + dy
}

func LogicalToPhysical(x, y int) PhysicalPoint {
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

func InLogicalBounds(x, y int) bool {
	return x >= 0 && x < GridWidth && y >= 0 && y < GridHeight
}
