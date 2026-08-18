package adapter

import (
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type runtimeStatus struct {
	startedAt time.Time

	engineConnected   atomic.Bool
	engineConnections atomic.Uint64
	lastFrameAt       atomic.Int64
	desiredSequence   atomic.Uint64

	framesSent       atomic.Uint64
	framesSentWindow atomic.Uint64
	actualFPSBits    atomic.Uint64
	fadeBits         atomic.Uint32

	udpWriteErrors       atomic.Uint64
	sourceAssigned       atomic.Bool
	udpStatusKnown       atomic.Bool
	udpWriteAvailable    atomic.Bool
	lastUDPSuccessAt     atomic.Int64
	lastFloorPacketAt    atomic.Int64
	pressureSequence     atomic.Uint64
	sensorPackets        atomic.Uint64
	invalidSensorPackets atomic.Uint64
	lastSensorPacketAt   atomic.Int64
}

func newRuntimeStatus() *runtimeStatus {
	return &runtimeStatus{startedAt: time.Now()}
}

func (s *runtimeStatus) markEngineConnected() {
	s.engineConnected.Store(true)
	s.engineConnections.Add(1)
}

func (s *runtimeStatus) setEngineConnected(value bool) {
	s.engineConnected.Store(value)
}

func (s *runtimeStatus) clearFrame() {
	s.desiredSequence.Store(0)
	s.lastFrameAt.Store(0)
}

func (s *runtimeStatus) markFrame(sequence uint64, receivedAt time.Time) {
	s.desiredSequence.Store(sequence)
	s.lastFrameAt.Store(receivedAt.UnixNano())
}

func (s *runtimeStatus) markFrameSent(sentAt time.Time, fade float32) uint64 {
	sequence := s.framesSent.Add(1)
	s.framesSentWindow.Add(1)
	s.fadeBits.Store(math.Float32bits(fade))
	s.lastUDPSuccessAt.Store(sentAt.UnixNano())
	return sequence
}

func (s *runtimeStatus) markFade(fade float32) {
	s.fadeBits.Store(math.Float32bits(fade))
}

func (s *runtimeStatus) markUDPError() {
	s.udpWriteErrors.Add(1)
	s.udpStatusKnown.Store(true)
	s.udpWriteAvailable.Store(false)
}

func (s *runtimeStatus) markUDPAvailable(observedAt time.Time) {
	s.udpStatusKnown.Store(true)
	s.udpWriteAvailable.Store(true)
	s.lastUDPSuccessAt.Store(observedAt.UnixNano())
}

func (s *runtimeStatus) markUDPUnavailable() {
	s.udpStatusKnown.Store(true)
	s.udpWriteAvailable.Store(false)
}

func (s *runtimeStatus) markFloorPacket(observedAt time.Time) {
	s.lastFloorPacketAt.Store(observedAt.UnixNano())
}

func (s *runtimeStatus) markSensorPacket(observedAt time.Time) {
	s.sensorPackets.Add(1)
	s.lastSensorPacketAt.Store(observedAt.UnixNano())
	s.markFloorPacket(observedAt)
}

func (s *runtimeStatus) markInvalidSensorPacket() {
	s.invalidSensorPackets.Add(1)
}

func (s *runtimeStatus) markPressure(sequence uint64) {
	s.pressureSequence.Store(sequence)
}

func (s *runtimeStatus) metricsLoop(ctxDone <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastSample := time.Now()
	for {
		select {
		case <-ctxDone:
			return
		case sampledAt := <-ticker.C:
			count := s.framesSentWindow.Swap(0)
			s.actualFPSBits.Store(math.Float64bits(frameRate(count, sampledAt.Sub(lastSample))))
			lastSample = sampledAt
		}
	}
}

func frameRate(count uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

type statusSnapshot struct {
	Uptime               time.Duration
	EngineConnected      bool
	DesiredSequence      uint64
	DesiredFrameAge      time.Duration
	FramesSent           uint64
	ActualFPS            float64
	FadeRatio            float32
	UDPWriteErrors       uint64
	SourceAssigned       bool
	UDPStatusKnown       bool
	UDPWriteAvailable    bool
	LastUDPSuccessAge    time.Duration
	FloorSeenRecently    bool
	LastFloorPacketAge   time.Duration
	PressureSequence     uint64
	SensorPackets        uint64
	InvalidSensorPackets uint64
	LastSensorPacketAge  time.Duration
}

func (s *runtimeStatus) snapshot(now time.Time, cfg Config) statusSnapshot {
	frameAge := ageSinceUnixNano(now, s.lastFrameAt.Load())
	udpAge := ageSinceUnixNano(now, s.lastUDPSuccessAt.Load())
	floorAge := ageSinceUnixNano(now, s.lastFloorPacketAt.Load())
	sensorAge := ageSinceUnixNano(now, s.lastSensorPacketAt.Load())
	return statusSnapshot{
		Uptime:               now.Sub(s.startedAt),
		EngineConnected:      s.engineConnected.Load(),
		DesiredSequence:      s.desiredSequence.Load(),
		DesiredFrameAge:      frameAge,
		FramesSent:           s.framesSent.Load(),
		ActualFPS:            math.Float64frombits(s.actualFPSBits.Load()),
		FadeRatio:            math.Float32frombits(s.fadeBits.Load()),
		UDPWriteErrors:       s.udpWriteErrors.Load(),
		SourceAssigned:       s.sourceAssigned.Load(),
		UDPStatusKnown:       s.udpStatusKnown.Load(),
		UDPWriteAvailable:    s.udpWriteAvailable.Load(),
		LastUDPSuccessAge:    udpAge,
		FloorSeenRecently:    floorAge >= 0 && floorAge <= cfg.FloorSeenWindow,
		LastFloorPacketAge:   floorAge,
		PressureSequence:     s.pressureSequence.Load(),
		SensorPackets:        s.sensorPackets.Load(),
		InvalidSensorPackets: s.invalidSensorPackets.Load(),
		LastSensorPacketAge:  sensorAge,
	}
}

func ageSinceUnixNano(now time.Time, value int64) time.Duration {
	if value <= 0 {
		return -1
	}
	age := now.Sub(time.Unix(0, value))
	if age < 0 {
		return 0
	}
	return age
}

func (s statusSnapshot) udpWriteState() string {
	if !s.UDPStatusKnown {
		return "unknown"
	}
	if s.UDPWriteAvailable {
		return "available"
	}
	return "unavailable"
}

type prometheusWriter struct {
	builder  strings.Builder
	lastHelp string
}

func (p *prometheusWriter) metric(name, help, kind string, value any, labels ...string) {
	if p.lastHelp != name {
		_, _ = fmt.Fprintf(&p.builder, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		p.lastHelp = name
	}
	p.builder.WriteString(name)
	if len(labels) > 0 {
		p.builder.WriteByte('{')
		for index := 0; index+1 < len(labels); index += 2 {
			if index > 0 {
				p.builder.WriteByte(',')
			}
			_, _ = fmt.Fprintf(&p.builder, "%s=%s", labels[index], strconv.Quote(labels[index+1]))
		}
		p.builder.WriteByte('}')
	}
	_, _ = fmt.Fprintf(&p.builder, " %v\n", value)
}

func metricsHandler(cfg Config, status *runtimeStatus, pressure *pressureStore) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if !allowMethods(w, request, http.MethodGet, http.MethodHead) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if request.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		now := time.Now()
		snapshot := status.snapshot(now, cfg)
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		var p prometheusWriter
		p.builder.Grow(8 * 1024)
		p.metric("motion_levels_controller_up", "Whether the controller process can serve metrics.", "gauge", 1)
		p.metric("motion_levels_controller_build_info", "Controller build information.", "gauge", 1, "revision", BuildRevision)
		p.metric("motion_levels_controller_uptime_seconds", "Controller process uptime.", "gauge", snapshot.Uptime.Seconds())
		p.metric("motion_levels_controller_go_memory_bytes", "Memory reserved by the Go runtime.", "gauge", memory.Sys)
		p.metric("motion_levels_controller_process_goroutines", "Current number of goroutines.", "gauge", runtime.NumGoroutine())
		p.metric("motion_levels_controller_engine_connected", "Whether an engine connection is active.", "gauge", boolNumber(snapshot.EngineConnected))
		p.metric("motion_levels_controller_engine_connections_total", "Total engine connections accepted.", "counter", status.engineConnections.Load())
		p.metric("motion_levels_controller_desired_frame_sequence", "Latest desired frame sequence.", "gauge", snapshot.DesiredSequence)
		p.metric("motion_levels_controller_desired_frame_age_seconds", "Age of the latest locally received desired frame.", "gauge", durationSeconds(snapshot.DesiredFrameAge))
		p.metric("motion_levels_controller_frames_sent_total", "Complete physical frame transactions accepted by the local UDP stack.", "counter", snapshot.FramesSent)
		p.metric("motion_levels_controller_actual_fps", "Measured successful physical frame transaction rate.", "gauge", snapshot.ActualFPS)
		p.metric("motion_levels_controller_configured_fps", "Configured physical refresh rate.", "gauge", cfg.RefreshFPS)
		p.metric("motion_levels_controller_fade_ratio", "Current safety fade from live frame (0) to black (1).", "gauge", snapshot.FadeRatio)
		p.metric("motion_levels_controller_udp_write_errors_total", "Physical UDP write errors.", "counter", snapshot.UDPWriteErrors)
		p.metric("motion_levels_controller_floor_source_assigned", "Whether the exact configured source address is assigned; always 1 when unset.", "gauge", boolNumber(snapshot.SourceAssigned))
		p.metric("motion_levels_controller_udp_write_status_known", "Whether an output attempt established UDP write status.", "gauge", boolNumber(snapshot.UDPStatusKnown))
		p.metric("motion_levels_controller_udp_write_available", "Whether the most recent physical UDP write succeeded.", "gauge", boolNumber(snapshot.UDPWriteAvailable))
		p.metric("motion_levels_controller_last_udp_success_age_seconds", "Age of the most recent successful physical UDP write.", "gauge", durationSeconds(snapshot.LastUDPSuccessAge))
		p.metric("motion_levels_controller_floor_seen_recently", "Whether a valid floor packet was observed within the configured window.", "gauge", boolNumber(snapshot.FloorSeenRecently))
		p.metric("motion_levels_controller_last_floor_packet_age_seconds", "Age of the most recent valid floor packet.", "gauge", durationSeconds(snapshot.LastFloorPacketAge))
		p.metric("motion_levels_controller_sensor_packets_total", "Complete physical sensor packets received.", "counter", snapshot.SensorPackets)
		p.metric("motion_levels_controller_invalid_sensor_packets_total", "Rejected malformed or incomplete sensor packets.", "counter", snapshot.InvalidSensorPackets)
		p.metric("motion_levels_controller_last_sensor_packet_age_seconds", "Age of the latest complete physical sensor packet.", "gauge", durationSeconds(snapshot.LastSensorPacketAge))
		p.metric("motion_levels_controller_pressure_sequence", "Canonical pressure-state sequence.", "gauge", snapshot.PressureSequence)
		if pressure != nil {
			floorStats := pressure.statsSnapshot(now, 0)
			p.metric("motion_levels_controller_active_pressed_tiles", "Number of currently pressed tiles on the physical floor.", "gauge", floorStats.ActivePressedTiles)
			p.metric("motion_levels_controller_tile_presses_total", "Total tile press transitions observed by this process.", "counter", floorStats.TotalPresses)
			p.metric("motion_levels_controller_tile_pressed_seconds_total", "Cumulative tile dwell seconds observed by this process.", "counter", floorStats.PressedDurationSec)
		}
		_, _ = w.Write([]byte(p.builder.String()))
	}
}

func boolNumber(value bool) int {
	if value {
		return 1
	}
	return 0
}

func durationSeconds(value time.Duration) float64 {
	if value < 0 {
		return -1
	}
	return value.Seconds()
}
