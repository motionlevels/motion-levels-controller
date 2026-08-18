package adapter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

const (
	sensorChannelStride = 171
	sensorPacketSize    = 3 + floor.DefaultChannels*sensorChannelStride
)

type sensorReader struct {
	cfg      Config
	pressure *pressureStore
	hub      *engineHub
	status   *runtimeStatus
	ready    func()

	lastWarningLog time.Time
	suppressedLogs int
}

func (r *sensorReader) logWarning(format string, values ...any) {
	now := time.Now()
	if now.Sub(r.lastWarningLog) < time.Second {
		r.suppressedLogs++
		return
	}
	message := fmt.Sprintf(format, values...)
	if r.suppressedLogs > 0 {
		log.Printf("%s (suppressed %d similar warnings)", message, r.suppressedLogs)
		r.suppressedLogs = 0
	} else {
		log.Print(message)
	}
	r.lastWarningLog = now
}

func (r *sensorReader) run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp4", r.cfg.ReceiveAddr)
	if err != nil {
		return fmt.Errorf("resolve floor receive address: %w", err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("listen for floor packets: %w", err)
	}
	defer conn.Close()
	log.Printf("floor input: %s", conn.LocalAddr())
	if r.ready != nil {
		r.ready()
	}

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	buffer := make([]byte, 64*1024)
	for {
		count, remote, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read floor packet: %w", err)
		}
		if count == 0 {
			continue
		}
		packet := buffer[:count]
		observedAt := time.Now()
		switch packet[0] {
		case 0x68:
			r.status.markFloorPacket(observedAt)
			log.Printf("floor handshake from %s (%d bytes)", remote, count)
		case 0x88:
			changes, err := decodeSensorPacket(packet, r.cfg.FloorRotation)
			if err != nil {
				r.status.markInvalidSensorPacket()
				r.logWarning("invalid floor sensor packet from %s: %v", remote, err)
				continue
			}
			r.status.markSensorPacket(observedAt)
			if snapshot, changed := r.pressure.apply(changes, observedAt); changed {
				r.status.markPressure(snapshot.Sequence)
				r.hub.publishPressure(snapshot)
			}
		default:
			r.logWarning("unknown floor packet 0x%02x from %s (%d bytes)", packet[0], remote, count)
		}
	}
}

func decodeSensorPacket(packet []byte, rotation floor.Rotation) ([]pressureChange, error) {
	// This installation's controller emits all eight 171-byte channel blocks in
	// one aggregate packet. Applying a partial packet would retain stale pressure
	// for omitted channels, so incomplete packets are rejected as a unit.
	if len(packet) < sensorPacketSize {
		return nil, fmt.Errorf("sensor packet is %d bytes, want at least %d", len(packet), sensorPacketSize)
	}
	controller := int(packet[1])
	if controller < 0 || controller >= floor.DefaultControllers {
		return nil, fmt.Errorf("controller %d is outside configured range", controller)
	}

	changes := make([]pressureChange, 0, floor.TileCount)
	for channel := 0; channel < floor.DefaultChannels; channel++ {
		base := 3 + channel*sensorChannelStride
		// The vendor block reserves 171 bytes, while this floor has 64 installed
		// sensors per channel. Remaining bytes are vendor metadata/capacity.
		for position := 0; position < floor.DefaultLEDsPerChannel; position++ {
			value := packet[base+position]
			var pressed bool
			switch value {
			case 0xCC:
				pressed = true
			case 0x00:
				pressed = false
			default:
				continue
			}
			x, y := floor.PhysicalToLogical(controller, channel, position, rotation)
			if !floor.InLogicalBounds(x, y) {
				return nil, fmt.Errorf("physical coordinate controller=%d channel=%d position=%d mapped outside the logical floor", controller, channel, position)
			}
			changes = append(changes, pressureChange{X: x, Y: y, Pressed: pressed})
		}
	}
	return changes, nil
}
