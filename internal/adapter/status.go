package adapter

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

//go:embed web/index.html
var webIndexHTML []byte

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

	udpWriteErrors      atomic.Uint64
	sourceAssigned      atomic.Bool
	udpStatusKnown      atomic.Bool
	udpWriteAvailable   atomic.Bool
	lastUDPSuccessAt    atomic.Int64
	lastFloorPacketAt   atomic.Int64
	pressureSequence    atomic.Uint64
	channelPackets      [floor.DefaultChannels]atomic.Uint64
	lastChannelPacketAt [floor.DefaultChannels]atomic.Int64
}

func newRuntimeStatus() *runtimeStatus {
	return &runtimeStatus{startedAt: time.Now()}
}

func (s *runtimeStatus) markEngineConnected() {
	s.engineConnected.Store(true)
	s.engineConnections.Add(1)
}

func (s *runtimeStatus) markChannelPacket(channel int, observedAt time.Time) {
	if channel >= 0 && channel < floor.DefaultChannels {
		s.channelPackets[channel].Add(1)
		s.lastChannelPacketAt[channel].Store(observedAt.UnixNano())
	}
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

func (s *runtimeStatus) markUDPAvailable() {
	s.udpStatusKnown.Store(true)
	s.udpWriteAvailable.Store(true)
	s.lastUDPSuccessAt.Store(time.Now().UnixNano())
}

func (s *runtimeStatus) markUDPUnavailable() {
	s.udpStatusKnown.Store(true)
	s.udpWriteAvailable.Store(false)
}

func (s *runtimeStatus) markFloorPacket(observedAt time.Time) {
	s.lastFloorPacketAt.Store(observedAt.UnixNano())
}

func (s *runtimeStatus) markPressure(sequence uint64) {
	s.pressureSequence.Store(sequence)
}

func (s *runtimeStatus) metricsLoop(ctxDone <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			s.actualFPSBits.Store(math.Float64bits(float64(s.framesSentWindow.Swap(0))))
		}
	}
}

type statusSnapshot struct {
	Uptime             time.Duration
	EngineConnected    bool
	DesiredSequence    uint64
	DesiredFrameAge    time.Duration
	FramesSent         uint64
	ActualFPS          float64
	FadeRatio          float32
	UDPWriteErrors     uint64
	SourceAssigned     bool
	UDPStatusKnown     bool
	UDPWriteAvailable  bool
	LastUDPSuccessAge  time.Duration
	FloorSeenRecently  bool
	LastFloorPacketAge time.Duration
	PressureSequence   uint64
}

func (s *runtimeStatus) snapshot(now time.Time, cfg Config) statusSnapshot {
	frameAge := time.Duration(-1)
	if value := s.lastFrameAt.Load(); value > 0 {
		frameAge = now.Sub(time.Unix(0, value))
	}
	udpAge := time.Duration(-1)
	if value := s.lastUDPSuccessAt.Load(); value > 0 {
		udpAge = now.Sub(time.Unix(0, value))
	}
	floorAge := time.Duration(-1)
	floorSeen := false
	if value := s.lastFloorPacketAt.Load(); value > 0 {
		floorAge = now.Sub(time.Unix(0, value))
		floorSeen = floorAge <= cfg.FloorSeenWindow
	}
	return statusSnapshot{
		Uptime:             now.Sub(s.startedAt),
		EngineConnected:    s.engineConnected.Load(),
		DesiredSequence:    s.desiredSequence.Load(),
		DesiredFrameAge:    frameAge,
		FramesSent:         s.framesSent.Load(),
		ActualFPS:          math.Float64frombits(s.actualFPSBits.Load()),
		FadeRatio:          math.Float32frombits(s.fadeBits.Load()),
		UDPWriteErrors:     s.udpWriteErrors.Load(),
		SourceAssigned:     s.sourceAssigned.Load(),
		UDPStatusKnown:     s.udpStatusKnown.Load(),
		UDPWriteAvailable:  s.udpWriteAvailable.Load(),
		LastUDPSuccessAge:  udpAge,
		FloorSeenRecently:  floorSeen,
		LastFloorPacketAge: floorAge,
		PressureSequence:   s.pressureSequence.Load(),
	}
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

type tileStatResponse struct {
	Presses  uint64  `json:"presses"`
	Duration float64 `json:"duration"`
}

type fullStateResponse struct {
	Status             string             `json:"status"`
	EngineConnected    bool               `json:"engine_connected"`
	FPS                float64            `json:"fps"`
	ConfiguredFPS      int                `json:"configured_fps"`
	UDPWrite           string             `json:"udp_write"`
	FloorSeen          bool               `json:"floor_seen"`
	ActivePressedTiles uint32             `json:"active_pressed_tiles"`
	PressureBase64     string             `json:"pressure_base64"`
	RGBBase64          string             `json:"rgb_base64"`
	WindowMinutes      int                `json:"window_minutes"`
	TileStats          []tileStatResponse `json:"tile_stats"`
	TileStats5m        []tileStatResponse `json:"tile_stats_5m"`
	TileStats15m       []tileStatResponse `json:"tile_stats_15m"`
}

func buildStateResponse(now time.Time, cfg Config, status *runtimeStatus, pressure *pressureStore, frames *frameStore, windowMinutes int) fullStateResponse {
	snapshot := status.snapshot(now, cfg)
	var pressureBase64 string
	var activePressedTiles uint32
	var tileStats, tileStats5m, tileStats15m []tileStatResponse
	if pressure != nil {
		pressureSnap := pressure.snapshot()
		pressureBase64 = base64.StdEncoding.EncodeToString(pressureSnap.Bits[:])

		floorStats := pressure.statsSnapshot(now, windowMinutes)
		activePressedTiles = floorStats.ActivePressedTiles
		tileStats = make([]tileStatResponse, floor.TileCount)
		for i := 0; i < floor.TileCount; i++ {
			tileStats[i] = tileStatResponse{
				Presses:  floorStats.Tiles[i].Presses,
				Duration: floorStats.Tiles[i].PressedDurationSec,
			}
		}

		floorStats5m := pressure.statsSnapshot(now, 5)
		tileStats5m = make([]tileStatResponse, floor.TileCount)
		for i := 0; i < floor.TileCount; i++ {
			tileStats5m[i] = tileStatResponse{
				Presses:  floorStats5m.Tiles[i].Presses,
				Duration: floorStats5m.Tiles[i].PressedDurationSec,
			}
		}

		floorStats15m := pressure.statsSnapshot(now, 15)
		tileStats15m = make([]tileStatResponse, floor.TileCount)
		for i := 0; i < floor.TileCount; i++ {
			tileStats15m[i] = tileStatResponse{
				Presses:  floorStats15m.Tiles[i].Presses,
				Duration: floorStats15m.Tiles[i].PressedDurationSec,
			}
		}
	}
	var rgbBase64 string
	if frames != nil {
		if frameSnap, hasFrame := frames.snapshot(); hasFrame {
			rgbBase64 = base64.StdEncoding.EncodeToString(frameSnap.RGB[:])
		}
	}
	return fullStateResponse{
		Status:             "ok",
		EngineConnected:    snapshot.EngineConnected,
		FPS:                snapshot.ActualFPS,
		ConfiguredFPS:      cfg.RefreshFPS,
		UDPWrite:           snapshot.udpWriteState(),
		FloorSeen:          snapshot.FloorSeenRecently,
		ActivePressedTiles: activePressedTiles,
		PressureBase64:     pressureBase64,
		RGBBase64:          rgbBase64,
		WindowMinutes:      windowMinutes,
		TileStats:          tileStats,
		TileStats5m:        tileStats5m,
		TileStats15m:       tileStats15m,
	}
}

func newHTTPHandler(cfg Config, status *runtimeStatus, pressure *pressureStore, hub *engineHub, frames *frameStore) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		snapshot := status.snapshot(time.Now(), cfg)
		response := struct {
			Status            string `json:"status"`
			UDPWrite          string `json:"udp_write"`
			FloorSeenRecently bool   `json:"floor_seen_recently"`
		}{
			Status:            "ok",
			UDPWrite:          snapshot.udpWriteState(),
			FloorSeenRecently: snapshot.FloorSeenRecently,
		}
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		p.builder.Grow(64 * 1024)
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
		p.metric("motion_levels_controller_pressure_sequence", "Canonical pressure-state sequence.", "gauge", snapshot.PressureSequence)
		for channel := 0; channel < floor.DefaultChannels; channel++ {
			chStr := strconv.Itoa(channel)
			p.metric("motion_levels_controller_channel_packets_total", "Total valid floor sensor packets observed for this hardware channel.", "counter", status.channelPackets[channel].Load(), "channel", chStr)
		}
		for channel := 0; channel < floor.DefaultChannels; channel++ {
			chStr := strconv.Itoa(channel)
			chAge := time.Duration(-1)
			if last := status.lastChannelPacketAt[channel].Load(); last > 0 {
				chAge = now.Sub(time.Unix(0, last))
			}
			p.metric("motion_levels_controller_channel_last_seen_seconds", "Age of the most recent sensor packet for this hardware channel.", "gauge", durationSeconds(chAge), "channel", chStr)
		}
		for channel := 0; channel < floor.DefaultChannels; channel++ {
			chStr := strconv.Itoa(channel)
			chHealthy := false
			if last := status.lastChannelPacketAt[channel].Load(); last > 0 {
				chHealthy = now.Sub(time.Unix(0, last)) <= cfg.FloorSeenWindow
			}
			p.metric("motion_levels_controller_channel_healthy", "Whether this hardware channel was observed within the floor-seen window.", "gauge", boolNumber(chHealthy), "channel", chStr)
		}
		if pressure != nil {
			floorStats := pressure.statsSnapshot(now, 0)
			p.metric("motion_levels_controller_active_pressed_tiles", "Number of currently pressed tiles on the physical floor.", "gauge", floorStats.ActivePressedTiles)
			for y := 0; y < floor.GridHeight; y++ {
				yStr := strconv.Itoa(y)
				for x := 0; x < floor.GridWidth; x++ {
					xStr := strconv.Itoa(x)
					idx := y*floor.GridWidth + x
					p.metric("motion_levels_controller_tile_presses_total", "Total times this tile was stepped on.", "counter", floorStats.Tiles[idx].Presses, "x", xStr, "y", yStr)
				}
			}
			for y := 0; y < floor.GridHeight; y++ {
				yStr := strconv.Itoa(y)
				for x := 0; x < floor.GridWidth; x++ {
					xStr := strconv.Itoa(x)
					idx := y*floor.GridWidth + x
					p.metric("motion_levels_controller_tile_pressed_seconds_total", "Total cumulative seconds this tile was actively stepped on.", "counter", floorStats.Tiles[idx].PressedDurationSec, "x", xStr, "y", yStr)
				}
			}
		}
		_, _ = w.Write([]byte(p.builder.String()))
	})

	mux.HandleFunc("/state", func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		window := 5
		if q := request.URL.Query().Get("window"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n >= 0 {
				window = n
			}
		}
		res := buildStateResponse(time.Now(), cfg, status, pressure, frames, window)
		_ = json.NewEncoder(w).Encode(res)
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, request *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		send := func() error {
			res := buildStateResponse(time.Now(), cfg, status, pressure, frames, 5)
			data, err := json.Marshal(res)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}

		if err := send(); err != nil {
			return
		}

		var eventCh <-chan struct{}
		if pressure != nil {
			ch, unsub := pressure.subscribe()
			defer unsub()
			eventCh = ch
		}

		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case <-eventCh:
				if err := send(); err != nil {
					return
				}
			case <-ticker.C:
				if err := send(); err != nil {
					return
				}
			}
		}
	})

	mux.HandleFunc("/press", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			X       int  `json:"x"`
			Y       int  `json:"y"`
			Pressed bool `json:"pressed"`
		}
		if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if pressure != nil {
			now := time.Now()
			snapshot, changed := pressure.apply([]pressureChange{{X: req.X, Y: req.Y, Pressed: req.Pressed}}, now)
			if changed {
				status.markPressure(snapshot.Sequence)
				if hub != nil {
					hub.publishPressure(snapshot)
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/stats/reset", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pressure != nil {
			pressure.resetStats()
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" && request.URL.Path != "/status" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(webIndexHTML)
	})
	return mux
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
