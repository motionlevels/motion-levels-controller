package main

import (
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/contracts/recordingpb"
	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestHTTPHandlerExposesOnlyHealthAndMetrics(t *testing.T) {
	handler := newHTTPHandler(config{RefreshFPS: 50}, newControllerMetrics())
	for _, testCase := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/tv"},
		{method: http.MethodPost, path: "/tv/refresh"},
		{method: http.MethodGet, path: "/"},
		{method: http.MethodGet, path: "/status"},
		{method: http.MethodGet, path: "/ws"},
	} {
		t.Run(testCase.method+" "+testCase.path, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
			}
		})
	}
}

func TestPhysicalPressureStateFollowsSensorPackets(t *testing.T) {
	state := &controllerState{sensorState: make(map[sensorKey]bool)}
	x, y := 4, 9

	physical := floor.LogicalToPhysical(x, y)
	packet := make([]byte, 3+floor.DefaultChannels*171)
	packet[0] = 0x88
	packet[1] = byte(physical.Controller)
	packet[3+physical.Channel*171+physical.Position] = 0xCC
	state.applySensorPacket(packet)
	if !state.snapshotPressed()[y][x] {
		t.Fatal("udp press did not mark tile pressed")
	}

	packet[3+physical.Channel*171+physical.Position] = 0x00
	state.applySensorPacket(packet)
	if state.snapshotPressed()[y][x] {
		t.Fatal("udp release did not clear tile")
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
	status := snapshotStatus(metrics, config{RefreshFPS: 50})
	if status.RefreshFPS != 50 {
		t.Fatalf("refresh fps = %d, want 50", status.RefreshFPS)
	}
}

func TestMetricsEndpointExportsBoundedControllerHealth(t *testing.T) {
	metrics := newControllerMetrics()
	metrics.markGameEngineConnected()
	metrics.actualFPSBits.Store(math.Float64bits(49.8))
	handler := newHTTPHandler(config{RefreshFPS: 50}, metrics)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"motion_levels_controller_up 1",
		"motion_levels_controller_actual_fps 49.8",
		"motion_levels_controller_game_engine_connected 1",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "session_id") || strings.Contains(body, "controller_id") {
		t.Fatalf("metrics contain an unbounded identity label:\n%s", body)
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

func TestUDPSenderPinsConfiguredLoopbackSource(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	conn, err := openUDP(0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sender, err := newUDPSender(conn, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sender.WriteToUDP([]byte("floor-source"), receiver.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	count, source, err := receiver.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:count]); got != "floor-source" {
		t.Fatalf("payload = %q, want floor-source", got)
	}
	if !source.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("source IP = %s, want 127.0.0.1", source.IP)
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

func TestConfigValidationRejectsInvalidHardwareConfig(t *testing.T) {
	cfg := config{
		FrameAddr:       "127.0.0.1:4201",
		RecvPort:        0,
		BroadcastIP:     "not-an-ip",
		FloorSourceIP:   "not-an-ip",
		BroadcastPort:   4626,
		RefreshFPS:      0,
		EngineFadeDelay: -time.Second,
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

func TestStatusSnapshotIncludesPresentationHealth(t *testing.T) {
	metrics := newControllerMetrics()
	metrics.markPresented(12, time.Now(), nil)
	status := snapshotStatus(metrics, config{})
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
