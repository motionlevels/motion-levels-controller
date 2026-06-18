package main

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lobis/motion-levels/floor-controller/internal/floor"
	"github.com/lobis/motion-levels/packages/contracts/recordingpb"
)

func TestWebAndUDPPushSamePressureState(t *testing.T) {
	state := &controllerState{sensorState: make(map[sensorKey]bool)}
	x, y := 4, 9

	physical := floor.LogicalToPhysical(x, y)
	state.applyPress(pressEvent{
		Source:     "web",
		Controller: physical.Controller,
		Channel:    physical.Channel,
		Position:   physical.Position,
		X:          x,
		Y:          y,
		Pressed:    true,
	})
	if !state.snapshotPressed()[y][x] {
		t.Fatal("web press did not mark tile pressed")
	}

	packet := make([]byte, 3+floor.DefaultChannels*171)
	packet[0] = 0x88
	packet[1] = byte(physical.Controller)
	packet[3+physical.Channel*171+physical.Position] = 0x00
	state.applySensorPacket(packet)
	if state.snapshotPressed()[y][x] {
		t.Fatal("udp release did not clear web-pressed tile")
	}

	packet[3+physical.Channel*171+physical.Position] = 0xCC
	state.applySensorPacket(packet)
	if !state.snapshotPressed()[y][x] {
		t.Fatal("udp press did not mark tile pressed")
	}
}

func TestLatestFrameBufferCopiesIncomingFrame(t *testing.T) {
	buffer := &latestFrameBuffer{}
	frame := &recordingpb.FrameRecord{
		Sequence:  7,
		UnixNanos: 123,
		Width:     floor.GridWidth,
		Height:    floor.GridHeight,
		Tiles: []*recordingpb.TileState{
			{X: 1, Y: 2, R: 3, G: 4, B: 5},
		},
	}

	buffer.update(frame)
	frame.Sequence = 8
	frame.Tiles[0].R = 99

	snapshot, ok := buffer.snapshot()
	if !ok {
		t.Fatal("missing latest frame")
	}
	if snapshot.Sequence != 7 {
		t.Fatalf("snapshot sequence = %d, want 7", snapshot.Sequence)
	}
	if snapshot.Tiles[0].R != 3 {
		t.Fatalf("snapshot tile red = %d, want 3", snapshot.Tiles[0].R)
	}

	snapshot.Tiles[0].G = 99
	nextSnapshot, ok := buffer.snapshot()
	if !ok {
		t.Fatal("missing latest frame after snapshot mutation")
	}
	if nextSnapshot.Tiles[0].G != 4 {
		t.Fatalf("next snapshot tile green = %d, want 4", nextSnapshot.Tiles[0].G)
	}
}

func TestSnapshotStatusIncludesConfiguredRefreshFPS(t *testing.T) {
	metrics := &controllerMetrics{startedAt: time.Now()}
	hub := &websocketHub{clients: make(map[*websocket.Conn]bool)}
	status := snapshotStatus(metrics, config{RefreshFPS: 50}, hub, nil)
	if status.RefreshFPS != 50 {
		t.Fatalf("refresh fps = %d, want 50", status.RefreshFPS)
	}
}

func TestGameEngineStreamGateAllowsOnlyOneActiveStream(t *testing.T) {
	gate := &gameEngineStreamGate{}
	if !gate.tryAcquire() {
		t.Fatal("first stream should be accepted")
	}
	if gate.tryAcquire() {
		t.Fatal("second concurrent stream should be rejected")
	}
	gate.release()
	if !gate.tryAcquire() {
		t.Fatal("stream should be accepted after release")
	}
}

func TestConfigMessageUsesControllerOwnedSettings(t *testing.T) {
	message := config{
		FrameAddr:         "127.0.0.1:4201",
		RecvPort:          7800,
		BroadcastIP:       "127.0.0.1",
		BroadcastPort:     4626,
		RecordFrames:      "recordings/live.frames.pbstream",
		RecordCompression: "gzip",
		RefreshFPS:        30,
	}.configMessage()

	if message.Type != "config" {
		t.Fatalf("message type = %q, want config", message.Type)
	}
	if message.RefreshFPS != 30 {
		t.Fatalf("refresh fps = %d, want 30", message.RefreshFPS)
	}
	if message.GridWidth != floor.GridWidth || message.GridHeight != floor.GridHeight {
		t.Fatalf("grid = %dx%d, want %dx%d", message.GridWidth, message.GridHeight, floor.GridWidth, floor.GridHeight)
	}
	if message.BroadcastAddr != "127.0.0.1:4626" {
		t.Fatalf("broadcast address = %q, want 127.0.0.1:4626", message.BroadcastAddr)
	}
	if message.InputAddr != "" {
		t.Fatalf("input address = %q, want empty default from test config", message.InputAddr)
	}
	if !message.Recording {
		t.Fatal("recording should be enabled when RecordFrames is set")
	}
	if message.Compression != "gzip" {
		t.Fatalf("compression = %q, want gzip", message.Compression)
	}
}

func TestPressureProtoFromEventIncludesHardwareAndLogicalCoordinates(t *testing.T) {
	now := time.Unix(0, 123)
	record := pressureProtoFromEvent(9, now, pressEvent{
		Source:     "udp",
		Controller: 1,
		Channel:    2,
		Position:   3,
		X:          4,
		Y:          5,
		Pressed:    true,
	})

	if record.Sequence != 9 || record.UnixNanos != 123 || record.X != 4 || record.Y != 5 || !record.Pressed {
		t.Fatalf("unexpected pressure record: %+v", record)
	}
	if record.Source != "udp" || record.Controller != 1 || record.Channel != 2 || record.Position != 3 {
		t.Fatalf("unexpected pressure source metadata: %+v", record)
	}
}

func TestBuildViewerFrameEncodesRGBAndPressureBitset(t *testing.T) {
	data := buildViewerFrame(42, 4, 2, []floor.Tile{
		{X: 1, Y: 0, R: 10, G: 20, B: 30, Pressed: true},
		{X: 3, Y: 1, R: 40, G: 50, B: 60, Pressed: true},
	})

	if got := string(data[0:4]); got != "MLF1" {
		t.Fatalf("magic = %q, want MLF1", got)
	}
	if got := binary.LittleEndian.Uint32(data[4:8]); got != 42 {
		t.Fatalf("sequence = %d, want 42", got)
	}
	if got := binary.LittleEndian.Uint16(data[8:10]); got != 4 {
		t.Fatalf("width = %d, want 4", got)
	}
	if got := binary.LittleEndian.Uint16(data[10:12]); got != 2 {
		t.Fatalf("height = %d, want 2", got)
	}
	if data[12] != 1 {
		t.Fatalf("flags = %d, want pressure flag", data[12])
	}
	headerLen := int(binary.LittleEndian.Uint16(data[14:16]))
	if headerLen != 16 {
		t.Fatalf("header length = %d, want 16", headerLen)
	}

	firstRGB := headerLen + 1*3
	if data[firstRGB] != 10 || data[firstRGB+1] != 20 || data[firstRGB+2] != 30 {
		t.Fatalf("first tile rgb = %d,%d,%d, want 10,20,30", data[firstRGB], data[firstRGB+1], data[firstRGB+2])
	}
	secondRGB := headerLen + 7*3
	if data[secondRGB] != 40 || data[secondRGB+1] != 50 || data[secondRGB+2] != 60 {
		t.Fatalf("second tile rgb = %d,%d,%d, want 40,50,60", data[secondRGB], data[secondRGB+1], data[secondRGB+2])
	}

	pressureOffset := headerLen + 4*2*3
	if data[pressureOffset]&(1<<1) == 0 {
		t.Fatal("missing pressure bit for tile index 1")
	}
	if data[pressureOffset]&(1<<7) == 0 {
		t.Fatal("missing pressure bit for tile index 7")
	}
}

func TestConfigValidationRejectsInvalidHardwareConfig(t *testing.T) {
	cfg := config{
		FrameAddr:         "127.0.0.1:4201",
		RecvPort:          0,
		BroadcastIP:       "not-an-ip",
		BroadcastPort:     4626,
		RecordFrames:      "recordings",
		RecordCompression: "zstd",
		RefreshFPS:        0,
		EngineFadeDelay:   -time.Second,
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected invalid config error")
	}
}

func TestColorGridFromTilesSupportsConstantTimeLookup(t *testing.T) {
	grid := colorGridFromTiles([]floor.Tile{
		{X: 2, Y: 3, R: 11, G: 22, B: 33},
		{X: floor.GridWidth, Y: floor.GridHeight, R: 99, G: 99, B: 99},
	})

	if got := grid[3][2]; got.R != 11 || got.G != 22 || got.B != 33 {
		t.Fatalf("grid color = %+v, want 11,22,33", got)
	}
	if got := grid[0][0]; got.R != 0 || got.G != 0 || got.B != 0 {
		t.Fatalf("default grid color = %+v, want black", got)
	}
}

func TestStatusSnapshotIncludesRecorderHealth(t *testing.T) {
	metrics := newControllerMetrics()
	metrics.markPresented(12, time.Now(), nil)
	hub := &websocketHub{}

	status := snapshotStatus(metrics, config{}, hub, nil)
	if status.Type != "status" {
		t.Fatalf("status type = %q, want status", status.Type)
	}
	if status.PresentedFrames != 12 {
		t.Fatalf("presented frames = %d, want 12", status.PresentedFrames)
	}
}

func TestSyncStatusTracksClockOffsetAndLatency(t *testing.T) {
	metrics := newControllerMetrics()
	gameAt := time.Unix(0, 1_000_000_000)
	receivedAt := gameAt.Add(2 * time.Millisecond)
	presentedAt := gameAt.Add(8 * time.Millisecond)
	frame := &recordingpb.FrameRecord{
		Sequence:          44,
		UnixNanos:         gameAt.UnixNano(),
		SessionId:         "session-1",
		GameFrameSequence: 44,
		GameUnixNanos:     gameAt.UnixNano(),
	}
	metrics.markGameEngineConnected()
	metrics.markGameFrame(frame, receivedAt)
	metrics.markPresented(2, presentedAt, frame)

	status := metrics.syncStatus()
	if status.Status != "ok" {
		t.Fatalf("sync status = %q, want ok", status.Status)
	}
	if status.SessionID != "session-1" || status.LastGameFrameSequence != 44 {
		t.Fatalf("unexpected sync identity: %+v", status)
	}
	if status.EngineClockOffsetMS != 2 || status.PresentLatencyMS != 8 {
		t.Fatalf("unexpected sync timing: %+v", status)
	}
}

func TestEngineFadeAmountAfterDisconnect(t *testing.T) {
	metrics := newControllerMetrics()
	startedAt := time.Now()
	metrics.markGameEngineConnected()
	if fade := metrics.engineFadeAmount(startedAt, time.Second, time.Second); fade != 0 {
		t.Fatalf("fade while connected = %f, want 0", fade)
	}

	metrics.markGameEngineDisconnected()
	disconnectedAt := time.Unix(0, metrics.lastGameDisconnected.Load())
	if fade := metrics.engineFadeAmount(disconnectedAt.Add(500*time.Millisecond), time.Second, time.Second); fade != 0 {
		t.Fatalf("fade during hold = %f, want 0", fade)
	}
	if fade := metrics.engineFadeAmount(disconnectedAt.Add(1500*time.Millisecond), time.Second, time.Second); fade < 0.49 || fade > 0.51 {
		t.Fatalf("fade midway = %f, want about 0.5", fade)
	}
	if fade := metrics.engineFadeAmount(disconnectedAt.Add(3*time.Second), time.Second, time.Second); fade != 1 {
		t.Fatalf("fade complete = %f, want 1", fade)
	}
}

func TestFadeTilesScalesColorWithoutChangingPressure(t *testing.T) {
	tiles := fadeTiles([]floor.Tile{
		{X: 1, Y: 2, R: 100, G: 50, B: 10, Pressed: true},
	}, 0.5)

	tile := tiles[0]
	if tile.R != 50 || tile.G != 25 || tile.B != 5 {
		t.Fatalf("faded color = %d,%d,%d, want 50,25,5", tile.R, tile.G, tile.B)
	}
	if !tile.Pressed {
		t.Fatal("fade should not change pressure state")
	}
}
