package adapter

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func sensorPacket() []byte {
	packet := make([]byte, sensorPacketSize)
	packet[0] = 0x88
	packet[1] = 0
	for index := 3; index < len(packet); index++ {
		packet[index] = 0x7F
	}
	return packet
}

func TestDecodeSensorPacketMapsOnlyInstalledSensors(t *testing.T) {
	packet := sensorPacket()
	x, y := 4, 9
	physical := floor.LogicalToPhysical(x, y, floor.Rotation0)
	packet[3+physical.Channel*sensorChannelStride+physical.Position] = 0xCC
	packet[3+physical.Channel*sensorChannelStride+100] = 0xCC
	changes, err := decodeSensorPacket(packet, floor.Rotation0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0] != (pressureChange{X: x, Y: y, Pressed: true}) {
		t.Fatalf("unexpected changes: %+v", changes)
	}
}

func TestDecodeSensorPacketAppliesHalfTurn(t *testing.T) {
	packet := sensorPacket()
	x, y := 2, 27
	physical := floor.LogicalToPhysical(x, y, floor.Rotation180)
	packet[3+physical.Channel*sensorChannelStride+physical.Position] = 0xCC
	changes, err := decodeSensorPacket(packet, floor.Rotation180)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].X != x || changes[0].Y != y {
		t.Fatalf("unexpected rotated changes: %+v", changes)
	}
}

func TestDecodeSensorPacketRejectsIncompleteAggregate(t *testing.T) {
	packet := sensorPacket()[:sensorPacketSize-sensorChannelStride]
	if _, err := decodeSensorPacket(packet, floor.Rotation0); err == nil {
		t.Fatal("incomplete packet was accepted")
	}
}

func TestSensorSnapshotCanRecoverAfterMissedTransition(t *testing.T) {
	store := &pressureStore{observedAt: time.Now()}
	pressed, changed := store.apply([]pressureChange{{X: 1, Y: 1, Pressed: true}}, time.Now())
	if !changed {
		t.Fatal("press did not change state")
	}
	healed := store.snapshot()
	if healed.Sequence != pressed.Sequence || healed.Bits != pressed.Bits {
		t.Fatalf("canonical snapshot did not preserve state")
	}
}

func TestSensorReaderEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	udpProbe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	udpAddr := udpProbe.LocalAddr().String()
	_ = udpProbe.Close()
	tcpProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := tcpProbe.Addr().String()
	_ = tcpProbe.Close()

	cfg := DefaultConfig()
	cfg.ReceiveAddr = udpAddr
	cfg.EngineAddr = tcpAddr
	status := newRuntimeStatus()
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Now()}
	hub := newEngineHub(cfg, frames, pressure, status)
	engine := &engineServer{cfg: cfg, hub: hub}
	sensors := &sensorReader{cfg: cfg, pressure: pressure, hub: hub, status: status}
	go func() { _ = engine.run(ctx) }()
	go func() { _ = sensors.run(ctx) }()

	var conn net.Conn
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		conn, err = net.Dial("tcp", tcpAddr)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("failed to connect to engine")
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if messageType, _, err := readWireMessage(reader); err != nil || messageType != messagePressureState {
		t.Fatalf("initial pressure message type=%d err=%v", messageType, err)
	}

	destination, _ := net.ResolveUDPAddr("udp4", udpAddr)
	sender, err := net.DialUDP("udp4", nil, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	packet := sensorPacket()
	x, y := 5, 12
	physical := floor.LogicalToPhysical(x, y, floor.Rotation0)
	packet[3+physical.Channel*sensorChannelStride+physical.Position] = 0xCC
	if _, err := sender.Write(packet); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := readWireMessage(reader)
	if err != nil || messageType != messagePressureState {
		t.Fatalf("updated pressure message type=%d err=%v", messageType, err)
	}
	snapshot, err := decodePressureSnapshot(payload)
	if err != nil || !snapshot.IsPressed(x, y) {
		t.Fatalf("unexpected pressure snapshot: %+v err=%v", snapshot, err)
	}
	if status.sensorPackets.Load() != 1 || status.invalidSensorPackets.Load() != 0 {
		t.Fatalf("sensor counters valid=%d invalid=%d", status.sensorPackets.Load(), status.invalidSensorPackets.Load())
	}
}
