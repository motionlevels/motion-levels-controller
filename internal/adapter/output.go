package adapter

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

type outputLoop struct {
	cfg      Config
	sender   packetSender
	frames   *frameStore
	pressure *pressureStore
	hub      *engineHub
	status   *runtimeStatus
}

func (o *outputLoop) run(ctx context.Context) error {
	log.Printf("physical refresh: %d fps", o.cfg.RefreshFPS)
	if err := o.writeSync(); err != nil {
		o.status.markUDPError()
	}
	if _, err := o.present(time.Now(), true); err != nil {
		o.status.markUDPError()
	}

	refresh := time.NewTicker(time.Second / time.Duration(o.cfg.RefreshFPS))
	defer refresh.Stop()
	syncTicker := time.NewTicker(o.cfg.SyncInterval)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			if _, err := o.present(time.Now(), true); err != nil {
				log.Printf("final black frame: %v", err)
			}
			return nil
		case <-syncTicker.C:
			// This goroutine owns every physical UDP write, so a sync packet can
			// never interleave with the packets of a frame transaction.
			if err := o.writeSync(); err != nil {
				o.status.markUDPError()
			}
		case now := <-refresh.C:
			if _, err := o.present(now, false); err != nil {
				o.status.markUDPError()
			}
		}
	}
}

func (o *outputLoop) present(now time.Time, forceBlack bool) (OutputSnapshot, error) {
	var rgb [floor.RGBByteCount]byte
	frame, hasFrame, releaseFrame := o.frames.presentationSnapshot()
	desiredSequence := uint64(0)
	frameAge := time.Duration(-1)
	fade := float32(1)

	if hasFrame && !forceBlack {
		desiredSequence = frame.Sequence
		frameAge = now.Sub(frame.ReceivedAt)
		fade = float32(frameFadeRatio(frameAge, o.cfg.FrameTimeout, o.cfg.FrameHold, o.cfg.FrameFade))
		copy(rgb[:], frame.RGB[:])
		applyFade(rgb[:], 1-float64(fade))
	}
	o.status.markFade(fade)

	err := o.writePhysicalFrame(rgb)
	// Release the generation barrier only after the complete frame transaction.
	// A replacement engine cannot complete its attach while an old frame is in
	// flight, but status publication is deliberately outside this lock.
	releaseFrame()
	sent := err == nil
	framesSent := o.status.framesSent.Load()
	if sent {
		framesSent = o.status.markFrameSent(now, fade)
	}

	pressure := o.pressure.snapshot()
	status := o.status.snapshot(now, o.cfg)
	output := OutputSnapshot{
		FramesSent:           framesSent,
		DesiredSequence:      desiredSequence,
		ObservedAt:           now,
		DesiredFrameAge:      frameAge,
		FadeRatio:            fade,
		PressureSequence:     pressure.Sequence,
		PressureBits:         pressure.Bits,
		UDPWriteAvailable:    status.UDPWriteAvailable,
		FloorSeenRecently:    status.FloorSeenRecently,
		UDPWriteErrors:       status.UDPWriteErrors,
		PhysicalFrameWasSent: sent,
	}
	o.hub.publishOutput(output)
	if err != nil {
		return output, fmt.Errorf("write physical frame: %w", err)
	}
	return output, nil
}

func (o *outputLoop) writeSync() error {
	packet := floor.BuildSyncPacket(0, 0, floor.DefaultChannels, []byte{255, 255, 255, 255})
	if err := o.sender.Write(packet); err != nil {
		return fmt.Errorf("write sync packet: %w", err)
	}
	return nil
}

func (o *outputLoop) writePhysicalFrame(rgb [floor.RGBByteCount]byte) error {
	packets := floor.BuildFrame(
		floor.DefaultControllers,
		floor.DefaultChannels,
		floor.DefaultLEDsPerChannel,
		func(controller, channel, position int) floor.RGB {
			x, y := floor.PhysicalToLogical(controller, channel, position, o.cfg.FloorRotation)
			if !floor.InLogicalBounds(x, y) {
				return floor.Black
			}
			index := (y*floor.GridWidth + x) * 3
			return floor.RGB{R: rgb[index], G: rgb[index+1], B: rgb[index+2]}
		},
	)
	for _, packet := range packets {
		if err := o.sender.Write(packet); err != nil {
			return err
		}
	}
	return nil
}

func frameFadeRatio(age, timeout, hold, duration time.Duration) float64 {
	if age < 0 {
		return 1
	}
	fadeStart := timeout + hold
	if age <= fadeStart {
		return 0
	}
	if duration <= 0 {
		return 1
	}
	return min(1, float64(age-fadeStart)/float64(duration))
}

func applyFade(rgb []byte, scale float64) {
	if scale >= 1 {
		return
	}
	if scale < 0 {
		scale = 0
	}
	for index, value := range rgb {
		rgb[index] = byte(math.Round(float64(value) * scale))
	}
}
