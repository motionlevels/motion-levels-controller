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
