package adapter

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func testHTTPHandler(cfg Config) (http.Handler, *runtimeStatus, *pressureStore, *frameStore) {
	status := newRuntimeStatus()
	pressure := &pressureStore{observedAt: time.Now()}
	frames := &frameStore{}
	hub := newEngineHub(cfg, frames, pressure, status)
	return newHTTPHandler(cfg, status, pressure, hub, frames), status, pressure, frames
}

func TestHTTPDashboardAndReadEndpoints(t *testing.T) {
	cfg := DefaultConfig()
	handler, _, _, _ := testHTTPHandler(cfg)
	for _, path := range []string{"/", "/status"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "<title>Motion Levels Floor Controller</title>") {
			t.Fatalf("GET %s status=%d", path, response.Code)
		}
		if response.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("dashboard response is missing CSP")
		}
	}
	for _, path := range []string{"/unknown", "/ws", "/tv"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d, want 404", path, response.Code)
		}
	}
	for _, path := range []string{"/state", "/health", "/metrics", "/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status=%d, want 405", path, response.Code)
		}
	}
}

func TestStateResponseUsesSelectedWindowAndExposesFade(t *testing.T) {
	cfg := DefaultConfig()
	handler, status, pressure, frames := testHTTPHandler(cfg)
	now := time.Now()
	pressure.apply([]pressureChange{{X: 2, Y: 4, Pressed: true}}, now)
	frames.beginGeneration(1)
	rgb := make([]byte, floor.RGBByteCount)
	rgb[0] = 100
	if !frames.store(1, 7, rgb, now) {
		t.Fatal("could not store frame")
	}
	status.markFrame(7, now)
	status.markFade(0.5)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/state?window=15", nil))
	var state fullStateResponse
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.WindowMinutes != 15 || len(state.TileStats) != floor.TileCount || state.FadeRatio != 0.5 || state.DesiredSequence != 7 || state.RGBBase64 == "" {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestStateRejectsInvalidWindow(t *testing.T) {
	handler, _, _, _ := testHTTPHandler(DefaultConfig())
	for _, value := range []string{"-1", "61", "bad"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/state?window="+value, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("window=%s status=%d, want 400", value, response.Code)
		}
	}
}

func TestDebugControlsAreOffByDefault(t *testing.T) {
	handler, _, _, _ := testHTTPHandler(DefaultConfig())
	for _, path := range []string{"/press", "/stats/reset"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"x":1,"y":1,"pressed":true}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(debugActionHeader, "1")
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("POST %s status=%d, want 404", path, response.Code)
		}
	}
}

func TestDebugPressValidationAndReset(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EnableDebugControls = true
	handler, _, pressure, _ := testHTTPHandler(cfg)

	request := httptest.NewRequest(http.MethodPost, "/press", strings.NewReader(`{"x":2,"y":4,"pressed":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing action header status=%d, want 403", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/press", strings.NewReader(`{"x":99,"y":4,"pressed":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(debugActionHeader, "1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range press status=%d, want 400", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/press", strings.NewReader(`{"x":2,"y":4,"pressed":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(debugActionHeader, "1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !pressure.snapshot().IsPressed(2, 4) {
		t.Fatalf("valid press status=%d pressed=%v", response.Code, pressure.snapshot().IsPressed(2, 4))
	}

	reset := httptest.NewRequest(http.MethodPost, "/stats/reset", nil)
	reset.Header.Set(debugActionHeader, "1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, reset)
	if response.Code != http.StatusNoContent {
		t.Fatalf("reset status=%d", response.Code)
	}
	if stats := pressure.statsSnapshot(time.Now().Add(time.Second), 0); stats.Tiles[4*floor.GridWidth+2].Presses != 0 {
		t.Fatal("reset did not clear counters")
	}
}

func TestHealthSeparatesUDPWriteFromFloorPresence(t *testing.T) {
	cfg := DefaultConfig()
	handler, status, _, _ := testHTTPHandler(cfg)
	status.udpStatusKnown.Store(true)
	status.udpWriteAvailable.Store(true)
	status.lastFloorPacketAt.Store(time.Now().Add(-time.Minute).UnixNano())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	body := response.Body.String()
	if !strings.Contains(body, `"udp_write":"available"`) || !strings.Contains(body, `"floor_seen_recently":false`) {
		t.Fatalf("unexpected health: %s", body)
	}
}

func TestMetricsStayOperationalAndBounded(t *testing.T) {
	cfg := DefaultConfig()
	handler, status, pressure, _ := testHTTPHandler(cfg)
	pressure.apply([]pressureChange{{X: 3, Y: 7, Pressed: true}}, time.Now())
	status.markEngineConnected()
	status.markSensorPacket(time.Now())
	status.markInvalidSensorPacket()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		"motion_levels_controller_up 1",
		"motion_levels_controller_engine_connections_total 1",
		"motion_levels_controller_active_pressed_tiles 1",
		"motion_levels_controller_sensor_packets_total 1",
		"motion_levels_controller_invalid_sensor_packets_total 1",
		"motion_levels_controller_desired_frame_age_seconds -1",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, `x="`) || strings.Contains(body, `y="`) || strings.Contains(body, "session_id") {
		t.Fatalf("metrics contain detailed or unbounded labels:\n%s", body)
	}
}

func TestEventsStartWithFullState(t *testing.T) {
	cfg := DefaultConfig()
	handler, _, _, _ := testHTTPHandler(cfg)
	server := httptest.NewServer(handler)
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/events?window=15")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var data string
	for i := 0; i < 5; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			break
		}
	}
	var state fullStateResponse
	if data == "" || json.Unmarshal([]byte(data), &state) != nil || state.WindowMinutes != 15 || len(state.TileStats) != floor.TileCount {
		t.Fatalf("unexpected initial SSE state: data=%q state=%+v", data, state)
	}
}

func TestEventsLimitConcurrentDiagnosticStreams(t *testing.T) {
	handler, _, _, _ := testHTTPHandler(DefaultConfig())
	server := httptest.NewServer(handler)
	defer server.Close()

	responses := make([]*http.Response, 0, maxDiagnosticStreams)
	for index := 0; index < maxDiagnosticStreams; index++ {
		response, err := server.Client().Get(server.URL + "/events")
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("stream %d status=%d, want 200", index, response.StatusCode)
		}
		responses = append(responses, response)
	}
	defer func() {
		for _, response := range responses {
			_ = response.Body.Close()
		}
	}()

	overflow, err := server.Client().Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer overflow.Body.Close()
	if overflow.StatusCode != http.StatusServiceUnavailable || overflow.Header.Get("Retry-After") != "1" {
		t.Fatalf("overflow status=%d retry=%q, want 503 with Retry-After", overflow.StatusCode, overflow.Header.Get("Retry-After"))
	}
}
