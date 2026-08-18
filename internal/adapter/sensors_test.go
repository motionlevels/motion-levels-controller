package adapter

import (
	"bufio"
	"context"
	"net"
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

	changes, _, err := decodeSensorPacket(packet, floor.Rotation0)
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

	changes, _, err := decodeSensorPacket(packet, floor.Rotation180)
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

func TestSensorReaderEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	udpListener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	udpAddr := udpListener.LocalAddr().String()
	_ = udpListener.Close()

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := tcpListener.Addr().String()
	_ = tcpListener.Close()

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

	// Connect engine TCP client
	var conn net.Conn
	for i := 0; i < 50; i++ {
		c, err := net.Dial("tcp", tcpAddr)
		if err == nil {
			conn = c
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("failed to connect to engine server")
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	// Initial message upon attach is pressure snapshot (sequence 0)
	msgType, payload, err := readWireMessage(reader)
	if err != nil {
		t.Fatalf("read initial wire message: %v", err)
	}
	if msgType != messagePressureState {
		t.Fatalf("initial message type = %d, want %d", msgType, messagePressureState)
	}
	initSnap, err := decodePressureSnapshot(payload)
	if err != nil {
		t.Fatalf("decode initial pressure: %v", err)
	}
	if initSnap.Sequence != 0 {
		t.Fatalf("initial sequence = %d, want 0", initSnap.Sequence)
	}

	// Prepare and send UDP sensor packet pressing (5, 12)
	destUDP, err := net.ResolveUDPAddr("udp4", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	senderConn, err := net.DialUDP("udp4", nil, destUDP)
	if err != nil {
		t.Fatal(err)
	}
	defer senderConn.Close()

	packet := make([]byte, 3+floor.DefaultChannels*sensorChannelStride)
	packet[0] = 0x88
	x, y := 5, 12
	phys := floor.LogicalToPhysical(x, y, floor.Rotation0)
	packet[3+phys.Channel*sensorChannelStride+phys.Position] = 0xCC

	if _, err := senderConn.Write(packet); err != nil {
		t.Fatalf("write UDP packet: %v", err)
	}

	// Read updated pressure message from engine TCP stream
	msgType, payload, err = readWireMessage(reader)
	if err != nil {
		t.Fatalf("read updated pressure message: %v", err)
	}
	if msgType != messagePressureState {
		t.Fatalf("message type = %d, want %d", msgType, messagePressureState)
	}
	snap, err := decodePressureSnapshot(payload)
	if err != nil {
		t.Fatalf("decode updated pressure: %v", err)
	}
	if snap.Sequence != 1 {
		t.Fatalf("updated sequence = %d, want 1", snap.Sequence)
	}
	tileIdx := y*floor.GridWidth + x
	if snap.Bits[tileIdx/8]&(1<<uint(tileIdx%8)) == 0 {
		t.Fatalf("expected tile (%d,%d) bit to be set in pressure bitset", x, y)
	}
	if count := status.channelPackets[phys.Channel].Load(); count != 1 {
		t.Fatalf("channel %d packets = %d, want 1", phys.Channel, count)
	}
}
