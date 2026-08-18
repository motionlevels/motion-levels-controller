package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestHTTPHandlerWebDashboardAndEndpoints(t *testing.T) {
	cfg := DefaultConfig()
	status := newRuntimeStatus()
	pressure := &pressureStore{observedAt: time.Now()}
	frames := &frameStore{}
	hub := newEngineHub(cfg, frames, pressure, status)
	handler := newHTTPHandler(cfg, status, pressure, hub, frames)

	// GET / and /status serve embedded HTML
	for _, path := range []string{"/", "/status"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d, want 200", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "<title>Motion Levels Floor Controller</title>") {
			t.Fatalf("GET %s did not serve expected HTML", path)
		}
	}

	// Unknown paths 404
	for _, path := range []string{"/unknown", "/ws", "/tv"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d, want 404", path, response.Code)
		}
	}

	// GET /state returns full JSON state
	stateReq := httptest.NewRequest(http.MethodGet, "/state", nil)
	stateResp := httptest.NewRecorder()
	handler.ServeHTTP(stateResp, stateReq)
	if stateResp.Code != http.StatusOK {
		t.Fatalf("GET /state status=%d", stateResp.Code)
	}
	var state fullStateResponse
	if err := json.Unmarshal(stateResp.Body.Bytes(), &state); err != nil {
		t.Fatalf("unmarshal state response: %v", err)
	}
	if state.Status != "ok" || state.ConfiguredFPS != 50 {
		t.Fatalf("unexpected state response: %+v", state)
	}

	// POST /press simulates pressure
	pressReq := httptest.NewRequest(http.MethodPost, "/press", strings.NewReader(`{"x":2,"y":4,"pressed":true}`))
	pressResp := httptest.NewRecorder()
	handler.ServeHTTP(pressResp, pressReq)
	if pressResp.Code != http.StatusNoContent {
		t.Fatalf("POST /press status=%d, want 204", pressResp.Code)
	}
	if !pressure.snapshot().IsPressed(2, 4) {
		t.Fatal("simulated press was not recorded in pressureStore")
	}

	// GET /state?window=15 returns window-specific data
	winReq := httptest.NewRequest(http.MethodGet, "/state?window=15", nil)
	winResp := httptest.NewRecorder()
	handler.ServeHTTP(winResp, winReq)
	if winResp.Code != http.StatusOK {
		t.Fatalf("GET /state?window=15 status=%d", winResp.Code)
	}
	var winState fullStateResponse
	if err := json.Unmarshal(winResp.Body.Bytes(), &winState); err != nil {
		t.Fatalf("unmarshal state response: %v", err)
	}
	if winState.WindowMinutes != 15 || len(winState.TileStats15m) != floor.TileCount {
		t.Fatalf("unexpected window state response: %+v", winState)
	}

	// POST /stats/reset clears stats
	resetReq := httptest.NewRequest(http.MethodPost, "/stats/reset", nil)
	resetResp := httptest.NewRecorder()
	handler.ServeHTTP(resetResp, resetReq)
	if resetResp.Code != http.StatusNoContent {
		t.Fatalf("POST /stats/reset status=%d, want 204", resetResp.Code)
	}
	idx := 4*floor.GridWidth + 2
	if pressure.statsSnapshot(time.Now(), 0).Tiles[idx].Presses != 0 {
		t.Fatal("stats were not cleared after /stats/reset")
	}
}

func TestHealthSeparatesUDPWriteFromFloorPresence(t *testing.T) {
	cfg := DefaultConfig()
	status := newRuntimeStatus()
	status.udpStatusKnown.Store(true)
	status.udpWriteAvailable.Store(true)
	status.lastFloorPacketAt.Store(time.Now().Add(-time.Minute).UnixNano())
	pressure := &pressureStore{observedAt: time.Now()}
	handler := newHTTPHandler(cfg, status, pressure, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"udp_write":"available"`) || !strings.Contains(body, `"floor_seen_recently":false`) {
		t.Fatalf("unexpected health body: %s", body)
	}
}

func TestMetricsUseBoundedLabels(t *testing.T) {
	cfg := DefaultConfig()
	status := newRuntimeStatus()
	pressure := &pressureStore{observedAt: time.Now()}
	pressure.apply([]pressureChange{{X: 3, Y: 7, Pressed: true}}, time.Now())
	status.markEngineConnected()
	status.markChannelPacket(2, time.Now())

	handler := newHTTPHandler(cfg, status, pressure, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{
		"motion_levels_controller_up 1",
		"motion_levels_controller_engine_connections_total 1",
		"motion_levels_controller_active_pressed_tiles 1",
		"motion_levels_controller_frames_sent_total 0",
		"motion_levels_controller_floor_seen_recently 0",
		"motion_levels_controller_desired_frame_age_seconds -1",
		"motion_levels_controller_last_udp_success_age_seconds -1",
		"motion_levels_controller_last_floor_packet_age_seconds -1",
		`motion_levels_controller_channel_packets_total{channel="2"} 1`,
		`motion_levels_controller_channel_packets_total{channel="0"} 0`,
		`motion_levels_controller_channel_healthy{channel="2"} 1`,
		`motion_levels_controller_channel_healthy{channel="0"} 0`,
		`motion_levels_controller_channel_last_seen_seconds{channel="0"} -1`,
		`motion_levels_controller_tile_presses_total{x="3",y="7"} 1`,
		`motion_levels_controller_tile_presses_total{x="0",y="0"} 0`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "session_id") || strings.Contains(body, "controller_id") {
		t.Fatalf("metrics contain unbounded identity labels:\n%s", body)
	}
}
