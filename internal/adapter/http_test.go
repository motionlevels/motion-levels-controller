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
	handler := newHTTPHandler(cfg, status)
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
	handler := newHTTPHandler(cfg, status)
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
	handler := newHTTPHandler(cfg, status)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{
		"motion_levels_controller_up 1",
		"motion_levels_controller_frames_sent_total 0",
		"motion_levels_controller_floor_seen_recently 0",
		"motion_levels_controller_desired_frame_age_seconds -1",
		"motion_levels_controller_last_udp_success_age_seconds -1",
		"motion_levels_controller_last_floor_packet_age_seconds -1",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "session_id") || strings.Contains(body, "controller_id") {
		t.Fatalf("metrics contain unbounded identity labels:\n%s", body)
	}
}
