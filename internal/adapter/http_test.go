package adapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPHandlerExposesOnlyHealthAndMetrics(t *testing.T) {
	cfg := DefaultConfig()
	status := newRuntimeStatus()
	pressure := &pressureStore{observedAt: time.Now()}
	handler := newHTTPHandler(cfg, status, pressure)
	for _, path := range []string{"/", "/status", "/ws", "/tv"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d, want 404", path, response.Code)
		}
	}
}

func TestHealthSeparatesUDPWriteFromFloorPresence(t *testing.T) {
	cfg := DefaultConfig()
	status := newRuntimeStatus()
	status.udpStatusKnown.Store(true)
	status.udpWriteAvailable.Store(true)
	status.lastFloorPacketAt.Store(time.Now().Add(-time.Minute).UnixNano())
	pressure := &pressureStore{observedAt: time.Now()}
	handler := newHTTPHandler(cfg, status, pressure)
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

	handler := newHTTPHandler(cfg, status, pressure)
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
