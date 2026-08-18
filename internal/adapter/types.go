package adapter

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

const (
	DefaultHTTPAddr      = "127.0.0.1:4101"
	DefaultEngineAddr    = "127.0.0.1:4203"
	DefaultReceiveAddr   = ":7800"
	DefaultBroadcastIP   = "255.255.255.255"
	DefaultBroadcastPort = 4626
	DefaultRefreshFPS    = 50

	DefaultFrameTimeout    = 500 * time.Millisecond
	DefaultFrameHold       = 2 * time.Second
	DefaultFrameFade       = 3 * time.Second
	DefaultFloorSeenWindow = 10 * time.Second
	DefaultSourceRetry     = time.Second
	DefaultSyncInterval    = 5 * time.Second
	DefaultReadTimeout     = 10 * time.Second
	DefaultWriteTimeout    = 2 * time.Second
)

// BuildRevision is embedded by release builds.
var BuildRevision = "unknown"

type Config struct {
	HTTPAddr      string
	EngineAddr    string
	ReceiveAddr   string
	FloorSourceIP string
	FloorRotation floor.Rotation
	BroadcastIP   string
	BroadcastPort int
	RefreshFPS    int

	FrameTimeout    time.Duration
	FrameHold       time.Duration
	FrameFade       time.Duration
	FloorSeenWindow time.Duration
	SourceRetry     time.Duration
	SyncInterval    time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
}

func DefaultConfig() Config {
	return Config{
		HTTPAddr:        DefaultHTTPAddr,
		EngineAddr:      DefaultEngineAddr,
		ReceiveAddr:     DefaultReceiveAddr,
		FloorRotation:   floor.Rotation0,
		BroadcastIP:     DefaultBroadcastIP,
		BroadcastPort:   DefaultBroadcastPort,
		RefreshFPS:      DefaultRefreshFPS,
		FrameTimeout:    DefaultFrameTimeout,
		FrameHold:       DefaultFrameHold,
		FrameFade:       DefaultFrameFade,
		FloorSeenWindow: DefaultFloorSeenWindow,
		SourceRetry:     DefaultSourceRetry,
		SyncInterval:    DefaultSyncInterval,
		ReadTimeout:     DefaultReadTimeout,
		WriteTimeout:    DefaultWriteTimeout,
	}
}

func (c Config) Validate() error {
	var errs []error
	for name, value := range map[string]string{
		"http":    c.HTTPAddr,
		"engine":  c.EngineAddr,
		"receive": c.ReceiveAddr,
	} {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Errorf("%s address must not be empty", name))
		}
	}
	if c.RefreshFPS < 1 || c.RefreshFPS > 500 {
		errs = append(errs, fmt.Errorf("refresh FPS must be between 1 and 500"))
	}
	if c.BroadcastPort < 1 || c.BroadcastPort > 65535 {
		errs = append(errs, fmt.Errorf("broadcast port must be between 1 and 65535"))
	}
	if ip := net.ParseIP(c.BroadcastIP); ip == nil || ip.To4() == nil {
		errs = append(errs, fmt.Errorf("broadcast IP must be a valid IPv4 address"))
	}
	if value := strings.TrimSpace(c.FloorSourceIP); value != "" {
		if ip := net.ParseIP(value); ip == nil || ip.To4() == nil {
			errs = append(errs, fmt.Errorf("floor source IP must be a valid IPv4 address"))
		}
	}
	if !floor.IsSupportedRotation(int(c.FloorRotation)) {
		errs = append(errs, fmt.Errorf("floor rotation must be 0 or 180"))
	}
	for name, value := range map[string]time.Duration{
		"frame timeout":     c.FrameTimeout,
		"frame hold":        c.FrameHold,
		"frame fade":        c.FrameFade,
		"floor seen window": c.FloorSeenWindow,
		"source retry":      c.SourceRetry,
		"sync interval":     c.SyncInterval,
		"read timeout":      c.ReadTimeout,
		"write timeout":     c.WriteTimeout,
	} {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	if floor.DefaultControllers*floor.DefaultChannels*floor.DefaultLEDsPerChannel != floor.TileCount {
		errs = append(errs, fmt.Errorf("physical LED count does not match logical grid"))
	}
	return errors.Join(errs...)
}

type Frame struct {
	Sequence   uint64
	ReceivedAt time.Time
	RGB        [floor.RGBByteCount]byte
}

type PressureSnapshot struct {
	Sequence   uint64
	ObservedAt time.Time
	Bits       [floor.PressureByteCount]byte
}

type OutputSnapshot struct {
	FramesSent           uint64
	DesiredSequence      uint64
	ObservedAt           time.Time
	DesiredFrameAge      time.Duration
	FadeRatio            float32
	PressureSequence     uint64
	PressureBits         [floor.PressureByteCount]byte
	UDPWriteAvailable    bool
	FloorSeenRecently    bool
	UDPWriteErrors       uint64
	PhysicalFrameWasSent bool
}
