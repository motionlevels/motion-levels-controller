package adapter

import (
	"strings"
	"testing"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsIPv6AndUnsupportedRotation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BroadcastIP = "::1"
	cfg.FloorRotation = floor.Rotation(90)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "IPv4") || !strings.Contains(err.Error(), "rotation") {
		t.Fatalf("validation error=%v", err)
	}
}

func TestConfigRequiresLoopbackManagementListeners(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "http wildcard", mutate: func(cfg *Config) { cfg.HTTPAddr = "0.0.0.0:4200" }},
		{name: "http LAN", mutate: func(cfg *Config) { cfg.HTTPAddr = "192.0.2.10:4200" }},
		{name: "engine wildcard", mutate: func(cfg *Config) { cfg.EngineAddr = ":4201" }},
		{name: "engine LAN", mutate: func(cfg *Config) { cfg.EngineAddr = "192.0.2.10:4201" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := DefaultConfig()
			testCase.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("validation error=%v, want loopback rejection", err)
			}
		})
	}
}

func TestConfigAcceptsLoopbackHostnamesAndIPv6(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTPAddr = "localhost:0"
	cfg.EngineAddr = "[::1]:0"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid loopback addresses rejected: %v", err)
	}
}
