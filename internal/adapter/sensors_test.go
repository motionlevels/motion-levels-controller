package adapter

import (
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestDecodeSensorPacketMapsOnlyInstalledSensors(t *testing.T) {
	packet := make([]byte, 3+floor.DefaultChannels*sensorChannelStride)
	packet[0] = 0x88
	packet[1] = 0
	for index := 3; index < len(packet); index++ {
		packet[index] = 0x7F // ignored vendor value
	}
	x, y := 4, 9
	physical := floor.LogicalToPhysical(x, y, floor.Rotation0)
	packet[3+physical.Channel*sensorChannelStride+physical.Position] = 0xCC
	packet[3+physical.Channel*sensorChannelStride+100] = 0xCC // outside installed 64 sensors

	changes, err := decodeSensorPacket(packet, floor.Rotation0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].X != x || changes[0].Y != y || !changes[0].Pressed {
		t.Fatalf("unexpected decoded changes: %+v", changes)
	}
}

func TestDecodeSensorPacketAppliesHalfTurn(t *testing.T) {
	packet := make([]byte, 3+floor.DefaultChannels*sensorChannelStride)
	packet[0] = 0x88
	for index := 3; index < len(packet); index++ {
		packet[index] = 0x7F
	}
	x, y := 2, 27
	physical := floor.LogicalToPhysical(x, y, floor.Rotation180)
	packet[3+physical.Channel*sensorChannelStride+physical.Position] = 0xCC

	changes, err := decodeSensorPacket(packet, floor.Rotation180)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].X != x || changes[0].Y != y {
		t.Fatalf("rotated change = %+v, want (%d,%d)", changes, x, y)
	}
}

func TestSensorSnapshotCanRecoverAfterMissedTransition(t *testing.T) {
	store := &pressureStore{observedAt: time.Now()}
	pressed, changed := store.apply([]pressureChange{{X: 1, Y: 1, Pressed: true}}, time.Now())
	if !changed {
		t.Fatal("press did not change state")
	}
	// A consumer may miss this snapshot. The next complete snapshot still
	// contains the current state and therefore heals the consumer.
	healed := store.snapshot()
	if healed.Sequence != pressed.Sequence || healed.Bits != pressed.Bits {
		t.Fatalf("canonical snapshot did not preserve state: pressed=%+v healed=%+v", pressed, healed)
	}
}
