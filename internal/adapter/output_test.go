package adapter

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

type recordingPacketSender struct {
	mu       sync.Mutex
	packets  [][]byte
	failAt   int
	writeNum int
}

func (s *recordingPacketSender) Write(packet []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeNum++
	if s.failAt > 0 && s.writeNum == s.failAt {
		return errors.New("test write failure")
	}
	copyPacket := append([]byte(nil), packet...)
	s.packets = append(s.packets, copyPacket)
	return nil
}

func (*recordingPacketSender) Close() error { return nil }

func TestStartupPresentationIsBlack(t *testing.T) {
	loop, sender, status := newTestOutputLoop()
	output, err := loop.present(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if output.DesiredSequence != 0 || output.FadeRatio != 1 || !output.PhysicalFrameWasSent {
		t.Fatalf("unexpected startup output: %+v", output)
	}
	if status.framesSent.Load() != 1 {
		t.Fatalf("frames sent=%d, want 1", status.framesSent.Load())
	}
	assertPhysicalRGBBlack(t, sender.packets)
}

func TestFreshPackedFrameIsSentWithoutTileObjectConversion(t *testing.T) {
	loop, sender, _ := newTestOutputLoop()
	loop.frames.beginGeneration(1)
	rgb := make([]byte, floor.RGBByteCount)
	index := (3*floor.GridWidth + 2) * 3
	rgb[index], rgb[index+1], rgb[index+2] = 100, 50, 25
	now := time.Now()
	if !loop.frames.store(1, 8, rgb, now) {
		t.Fatal("could not store test frame")
	}
	output, err := loop.present(now.Add(10*time.Millisecond), false)
	if err != nil {
		t.Fatal(err)
	}
	if output.DesiredSequence != 8 || output.FadeRatio != 0 {
		t.Fatalf("unexpected fresh output: %+v", output)
	}
	if !physicalRGBHasNonZeroValue(sender.packets) {
		t.Fatal("physical frame contained no non-zero RGB values")
	}
}

func TestFailedTransactionIsNotCountedAsSent(t *testing.T) {
	loop, sender, status := newTestOutputLoop()
	sender.failAt = 1
	output, err := loop.present(time.Now(), true)
	if err == nil {
		t.Fatal("expected physical write failure")
	}
	if output.PhysicalFrameWasSent || status.framesSent.Load() != 0 {
		t.Fatalf("failed frame was counted: output=%+v frames=%d", output, status.framesSent.Load())
	}
}

func newTestOutputLoop() (*outputLoop, *recordingPacketSender, *runtimeStatus) {
	cfg := DefaultConfig()
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Now()}
	status := newRuntimeStatus()
	hub := newEngineHub(cfg, frames, pressure, status)
	sender := &recordingPacketSender{}
	return &outputLoop{cfg: cfg, sender: sender, frames: frames, pressure: pressure, hub: hub, status: status}, sender, status
}

func assertPhysicalRGBBlack(t *testing.T, packets [][]byte) {
	t.Helper()
	for _, packet := range packets {
		if !isRGBDataPacket(packet) {
			continue
		}
		for _, value := range packet[14 : len(packet)-3] {
			if value != 0 {
				t.Fatalf("non-black physical RGB byte: %d", value)
			}
		}
	}
}

func physicalRGBHasNonZeroValue(packets [][]byte) bool {
	for _, packet := range packets {
		if !isRGBDataPacket(packet) {
			continue
		}
		for _, value := range packet[14 : len(packet)-3] {
			if value != 0 {
				return true
			}
		}
	}
	return false
}

func isRGBDataPacket(packet []byte) bool {
	return len(packet) >= 17 && packet[0] == 0x75 && packet[8] == 0x88 && packet[9] == 0x77 && packet[10] != 0xFF
}

type blockingPacketSender struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingPacketSender) Write([]byte) error {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return nil
}

func (*blockingPacketSender) Close() error { return nil }

func TestEngineReplacementWaitsForCompletePhysicalTransaction(t *testing.T) {
	cfg := DefaultConfig()
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Now()}
	status := newRuntimeStatus()
	hub := newEngineHub(cfg, frames, pressure, status)

	server1, client1 := net.Pipe()
	defer client1.Close()
	first := hub.attach(server1)
	defer first.close()
	rgb := make([]byte, floor.RGBByteCount)
	rgb[0] = 100
	if !frames.store(first.generation, 1, rgb, time.Now()) {
		t.Fatal("could not store first generation frame")
	}

	sender := &blockingPacketSender{started: make(chan struct{}), release: make(chan struct{})}
	loop := &outputLoop{cfg: cfg, sender: sender, frames: frames, pressure: pressure, hub: hub, status: status}
	presentDone := make(chan error, 1)
	go func() {
		_, err := loop.present(time.Now(), false)
		presentDone <- err
	}()
	<-sender.started

	server2, client2 := net.Pipe()
	defer client2.Close()
	attachDone := make(chan *engineSession, 1)
	go func() { attachDone <- hub.attach(server2) }()
	select {
	case second := <-attachDone:
		second.close()
		t.Fatal("replacement completed while an old frame transaction was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(sender.release)
	if err := <-presentDone; err != nil {
		t.Fatal(err)
	}
	var second *engineSession
	select {
	case second = <-attachDone:
	case <-time.After(time.Second):
		t.Fatal("replacement did not complete after physical transaction")
	}
	defer second.close()
	if _, ok := frames.snapshot(); ok {
		t.Fatal("replacement did not invalidate the previous frame")
	}
}
