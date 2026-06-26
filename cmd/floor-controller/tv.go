package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// kioskServiceName is the systemd unit that drives the HDMI player display.
const kioskServiceName = "motion-levels-kiosk.service"

type tvConnector struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
}

type tvStatus struct {
	HDMIConnected bool          `json:"hdmiConnected"`
	KioskActive   bool          `json:"kioskActive"`
	DisplayURL    string        `json:"displayUrl"`
	Connectors    []tvConnector `json:"connectors"`
}

// readDisplayConnectors reports the connection state of every DRM connector
// (HDMI/DP/...) exposed by the kernel under /sys/class/drm.
func readDisplayConnectors() []tvConnector {
	matches, _ := filepath.Glob("/sys/class/drm/card*-*/status")
	connectors := make([]tvConnector, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name := filepath.Base(filepath.Dir(path)) // e.g. card0-HDMI-A-1
		if i := strings.Index(name, "-"); i >= 0 {
			name = name[i+1:] // strip the leading "cardN-"
		}
		connectors = append(connectors, tvConnector{
			Name:      name,
			Connected: strings.TrimSpace(string(data)) == "connected",
		})
	}
	return connectors
}

func hdmiConnected(connectors []tvConnector) bool {
	for _, c := range connectors {
		if c.Connected && strings.HasPrefix(strings.ToUpper(c.Name), "HDMI") {
			return true
		}
	}
	return false
}

func kioskActive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", kioskServiceName).Output()
	return strings.TrimSpace(string(out)) == "active"
}

func playerDisplayURL() string {
	if v := strings.TrimSpace(os.Getenv("MOTION_LEVELS_PLAYER_URL")); v != "" {
		return v
	}
	return "http://127.0.0.1/display/"
}

func currentTVStatus(ctx context.Context) tvStatus {
	connectors := readDisplayConnectors()
	return tvStatus{
		HDMIConnected: hdmiConnected(connectors),
		KioskActive:   kioskActive(ctx),
		DisplayURL:    playerDisplayURL(),
		Connectors:    connectors,
	}
}

// restartKiosk forces the HDMI player display to (re)start, which also re-runs
// the xrandr output configuration — the way to recover a TV that was plugged in
// after boot or left on a bad mode.
func restartKiosk(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", "restart", kioskServiceName).Run()
}
