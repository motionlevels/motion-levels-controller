package adapter

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

//go:embed web/index.html
var webIndexHTML []byte

const (
	debugActionHeader    = "X-Motion-Levels-Debug"
	maxDiagnosticStreams = 8
)

type tileStatResponse struct {
	Presses  uint64  `json:"presses"`
	Duration float64 `json:"duration"`
}

type fullStateResponse struct {
	Status               string             `json:"status"`
	EngineConnected      bool               `json:"engine_connected"`
	DesiredSequence      uint64             `json:"desired_sequence"`
	DesiredFrameAge      float64            `json:"desired_frame_age_seconds"`
	FadeRatio            float32            `json:"fade_ratio"`
	FPS                  float64            `json:"fps"`
	ConfiguredFPS        int                `json:"configured_fps"`
	UDPWrite             string             `json:"udp_write"`
	FloorSeen            bool               `json:"floor_seen"`
	FloorRotation        int                `json:"floor_rotation"`
	ActivePressedTiles   uint32             `json:"active_pressed_tiles"`
	PressureBase64       string             `json:"pressure_base64"`
	RGBBase64            string             `json:"rgb_base64"`
	WindowMinutes        int                `json:"window_minutes"`
	TileStats            []tileStatResponse `json:"tile_stats,omitempty"`
	DebugControlsEnabled bool               `json:"debug_controls_enabled"`
}

func buildStateResponse(now time.Time, cfg Config, status *runtimeStatus, pressure *pressureStore, frames *frameStore, windowMinutes int, includeStats bool) fullStateResponse {
	snapshot := status.snapshot(now, cfg)
	response := fullStateResponse{
		Status:               "ok",
		EngineConnected:      snapshot.EngineConnected,
		DesiredSequence:      snapshot.DesiredSequence,
		DesiredFrameAge:      durationSeconds(snapshot.DesiredFrameAge),
		FadeRatio:            snapshot.FadeRatio,
		FPS:                  snapshot.ActualFPS,
		ConfiguredFPS:        cfg.RefreshFPS,
		UDPWrite:             snapshot.udpWriteState(),
		FloorSeen:            snapshot.FloorSeenRecently,
		FloorRotation:        int(cfg.FloorRotation),
		WindowMinutes:        windowMinutes,
		DebugControlsEnabled: cfg.EnableDebugControls,
	}
	if pressure != nil {
		pressureSnapshot := pressure.snapshot()
		response.PressureBase64 = base64.StdEncoding.EncodeToString(pressureSnapshot.Bits[:])
		stats := pressure.statsSnapshot(now, windowMinutes)
		response.ActivePressedTiles = stats.ActivePressedTiles
		if includeStats {
			response.TileStats = make([]tileStatResponse, floor.TileCount)
			for index, tile := range stats.Tiles {
				response.TileStats[index] = tileStatResponse{
					Presses:  tile.Presses,
					Duration: tile.PressedDurationSec,
				}
			}
		}
	}
	if frames != nil {
		if frame, ok := frames.snapshot(); ok {
			response.RGBBase64 = base64.StdEncoding.EncodeToString(frame.RGB[:])
		}
	}
	return response
}

func allowMethods(w http.ResponseWriter, request *http.Request, methods ...string) bool {
	for _, method := range methods {
		if request.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func parseStatsWindow(request *http.Request) (int, error) {
	value := request.URL.Query().Get("window")
	if value == "" {
		return 5, nil
	}
	window, err := strconv.Atoi(value)
	if err != nil || window < 0 || window > historyMinutes {
		return 0, fmt.Errorf("window must be an integer from 0 to %d", historyMinutes)
	}
	return window, nil
}

func requireDebugControl(w http.ResponseWriter, request *http.Request, cfg Config) bool {
	if !cfg.EnableDebugControls {
		http.NotFound(w, request)
		return false
	}
	if request.Header.Get(debugActionHeader) != "1" {
		http.Error(w, "debug action header required", http.StatusForbidden)
		return false
	}
	return true
}

func setCommonSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
}

func newHTTPHandler(cfg Config, status *runtimeStatus, pressure *pressureStore, hub *engineHub, frames *frameStore) http.Handler {
	mux := http.NewServeMux()
	streamSlots := make(chan struct{}, maxDiagnosticStreams)

	mux.HandleFunc("/health", func(w http.ResponseWriter, request *http.Request) {
		if !allowMethods(w, request, http.MethodGet, http.MethodHead) {
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

	mux.HandleFunc("/metrics", metricsHandler(cfg, status, pressure))

	mux.HandleFunc("/state", func(w http.ResponseWriter, request *http.Request) {
		if !allowMethods(w, request, http.MethodGet, http.MethodHead) {
			return
		}
		window, err := parseStatsWindow(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		response := buildStateResponse(time.Now(), cfg, status, pressure, frames, window, true)
		_ = json.NewEncoder(w).Encode(response)
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, request *http.Request) {
		if !allowMethods(w, request, http.MethodGet) {
			return
		}
		window, err := parseStatsWindow(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case streamSlots <- struct{}{}:
			defer func() { <-streamSlots }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many diagnostic streams", http.StatusServiceUnavailable)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")

		send := func(includeStats bool) error {
			response := buildStateResponse(time.Now(), cfg, status, pressure, frames, window, includeStats)
			data, err := json.Marshal(response)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}
		if _, err := io.WriteString(w, "retry: 1000\n\n"); err != nil {
			return
		}
		if err := send(true); err != nil {
			return
		}

		var pressureEvents <-chan struct{}
		if pressure != nil {
			channel, unsubscribe := pressure.subscribe()
			defer unsubscribe()
			pressureEvents = channel
		}
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case <-pressureEvents:
				if err := send(true); err != nil {
					return
				}
			case <-ticker.C:
				if err := send(false); err != nil {
					return
				}
			}
		}
	})

	mux.HandleFunc("/press", func(w http.ResponseWriter, request *http.Request) {
		if !allowMethods(w, request, http.MethodPost) || !requireDebugControl(w, request, cfg) {
			return
		}
		mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		request.Body = http.MaxBytesReader(w, request.Body, 1024)
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		var body struct {
			X       int  `json:"x"`
			Y       int  `json:"y"`
			Pressed bool `json:"pressed"`
		}
		if err := decoder.Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
			return
		}
		if !floor.InLogicalBounds(body.X, body.Y) {
			http.Error(w, "tile coordinate is outside the floor", http.StatusBadRequest)
			return
		}
		if pressure != nil {
			snapshot, changed := pressure.applyDebug([]pressureChange{{X: body.X, Y: body.Y, Pressed: body.Pressed}}, time.Now(), cfg.DebugPressLease)
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
		if !allowMethods(w, request, http.MethodPost) || !requireDebugControl(w, request, cfg) {
			return
		}
		if pressure != nil {
			pressure.resetStats(time.Now())
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" && request.URL.Path != "/status" {
			http.NotFound(w, request)
			return
		}
		if !allowMethods(w, request, http.MethodGet, http.MethodHead) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if request.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(webIndexHTML)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		setCommonSecurityHeaders(w)
		mux.ServeHTTP(w, request)
	})
}
