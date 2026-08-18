package adapter

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

const (
	DefaultHTTPAddr      = "127.0.0.1:4200"
	DefaultEngineAddr    = "127.0.0.1:4201"
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
	DefaultDebugPressLease = 2 * time.Second
)

// BuildRevision is embedded by release builds.
var BuildRevision = "unknown"

type Config struct {
	HTTPAddr            string
	EngineAddr          string
	ReceiveAddr         string
	FloorSourceIP       string
	FloorRotation       floor.Rotation
	BroadcastIP         string
	BroadcastPort       int
	RefreshFPS          int
	EnableDebugControls bool

	FrameTimeout    time.Duration
	FrameHold       time.Duration
	FrameFade       time.Duration
	FloorSeenWindow time.Duration
	SourceRetry     time.Duration
	SyncInterval    time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	DebugPressLease time.Duration
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
		DebugPressLease: DefaultDebugPressLease,
	}
}

func (c Config) Validate() error {
	var errs []error
	if err := validateLoopbackTCPAddress("HTTP", c.HTTPAddr); err != nil {
		errs = append(errs, err)
	}
	if err := validateLoopbackTCPAddress("engine", c.EngineAddr); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(c.ReceiveAddr) == "" {
		errs = append(errs, fmt.Errorf("receive address must not be empty"))
	} else if _, err := net.ResolveUDPAddr("udp4", c.ReceiveAddr); err != nil {
		errs = append(errs, fmt.Errorf("receive address must be a valid IPv4 UDP address: %w", err))
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
		"debug press lease": c.DebugPressLease,
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

func validateLoopbackTCPAddress(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s address must not be empty", name)
	}
	host, portValue, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s address must be host:port: %w", name, err)
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("%s port must be between 0 and 65535", name)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s address must bind to loopback", name)
	}
	return nil
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

func (s PressureSnapshot) IsPressed(x, y int) bool {
	if !floor.InLogicalBounds(x, y) {
		return false
	}
	index := y*floor.GridWidth + x
	return s.Bits[index/8]&(1<<uint(index%8)) != 0
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
