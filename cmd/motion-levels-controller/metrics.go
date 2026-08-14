package main

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var buildRevision = "unknown"

type prometheusWriter struct {
	b strings.Builder
}

func (p *prometheusWriter) metric(name, help, kind string, value any, labels ...string) {
	_, _ = fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n%s", name, help, name, kind, name)
	if len(labels) > 0 {
		p.b.WriteByte('{')
		for index := 0; index+1 < len(labels); index += 2 {
			if index > 0 {
				p.b.WriteByte(',')
			}
			_, _ = fmt.Fprintf(&p.b, "%s=%s", labels[index], strconv.Quote(labels[index+1]))
		}
		p.b.WriteByte('}')
	}
	_, _ = fmt.Fprintf(&p.b, " %v\n", value)
}

func controllerMetricsHandler(cfg config, metrics *controllerMetrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		status := snapshotStatus(metrics, cfg)
		now := time.Now()
		frameAge := float64(status.GameFrameAgeMS) / 1000
		if status.GameFrameAgeMS < 0 {
			frameAge = 0
		}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)

		var p prometheusWriter
		p.metric("motion_levels_controller_up", "Whether the floor controller process can serve metrics.", "gauge", 1)
		p.metric("motion_levels_controller_build_info", "Floor controller build information.", "gauge", 1, "revision", buildRevision)
		p.metric("motion_levels_controller_process_start_time_seconds", "Floor controller process start time since Unix epoch.", "gauge", metrics.startedAt.Unix())
		p.metric("motion_levels_controller_go_memory_bytes", "Memory reserved by the Go runtime.", "gauge", memory.Sys)
		p.metric("motion_levels_controller_process_goroutines", "Current number of goroutines.", "gauge", runtime.NumGoroutine())
		p.metric("motion_levels_controller_uptime_seconds", "Floor controller process uptime.", "gauge", now.Sub(metrics.startedAt).Seconds())
		p.metric("motion_levels_controller_presented_frames_total", "Frames presented to the physical floor.", "counter", status.PresentedFrames)
		p.metric("motion_levels_controller_actual_fps", "Measured physical floor presentation rate.", "gauge", status.ActualFPS)
		p.metric("motion_levels_controller_configured_fps", "Configured physical floor presentation rate.", "gauge", status.RefreshFPS)
		p.metric("motion_levels_controller_game_engine_connected", "Whether the game engine frame stream is connected.", "gauge", boolNumber(status.GameEngineOnline))
		p.metric("motion_levels_controller_game_frame_age_seconds", "Age of the latest frame received from the game engine.", "gauge", frameAge)
		p.metric("motion_levels_controller_engine_fade_ratio", "Current safety fade from live frame (0) to black (1).", "gauge", status.EngineFadeAmount)
		p.metric("motion_levels_controller_udp_send_errors_total", "UDP floor output send errors.", "counter", status.UDPErrorCount)
		p.metric("motion_levels_controller_floor_source_assigned", "Whether the exact configured floor source is currently assigned to an active interface; always 1 when no source is configured.", "gauge", boolNumber(status.UDPSourceAssigned))
		p.metric("motion_levels_controller_floor_transport_status_known", "Whether at least one floor UDP output attempt has established transport status.", "gauge", boolNumber(status.UDPTransportKnown))
		p.metric("motion_levels_controller_floor_transport_available", "Whether the most recent floor UDP output attempt succeeded.", "gauge", boolNumber(status.UDPTransportReady))
		p.metric("motion_levels_controller_floor_source_resolution_attempts_total", "Attempts to acquire the exact configured floor UDP source.", "counter", status.UDPResolveRuns)
		lastUDPSuccessTimestamp := 0.0
		if sent := metrics.lastUDPSuccessUnixNano.Load(); sent > 0 {
			lastUDPSuccessTimestamp = float64(sent) / float64(time.Second)
		}
		p.metric("motion_levels_controller_floor_last_udp_success_timestamp_seconds", "Unix timestamp of the most recent successful floor UDP output, or zero before the first success.", "gauge", lastUDPSuccessTimestamp)
		p.metric("motion_levels_controller_sync_samples_total", "Frame clock synchronization samples.", "counter", status.Sync.Samples)
		p.metric("motion_levels_controller_engine_clock_offset_seconds", "Observed engine to controller clock offset.", "gauge", status.Sync.EngineClockOffsetMS/1000)
		p.metric("motion_levels_controller_present_latency_seconds", "Observed controller presentation latency.", "gauge", status.Sync.PresentLatencyMS/1000)
		p.metric("motion_levels_controller_sync_jitter_seconds", "Observed frame synchronization jitter.", "gauge", status.Sync.JitterMS/1000)
		_, _ = w.Write([]byte(p.b.String()))
	}
}

func boolNumber(value bool) int {
	if value {
		return 1
	}
	return 0
}
