package adapter

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestEngineSessionReceivesPackedFramesAndStartsWithPressureSnapshot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReadTimeout = time.Second
	cfg.WriteTimeout = time.Second
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Unix(0, 123)}
	status := newRuntimeStatus()
	hub := newEngineHub(cfg, frames, pressure, status)

	server, client := net.Pipe()
	defer client.Close()
	session := hub.attach(server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		session.run(ctx)
		close(done)
	}()

	reader := bufio.NewReader(client)
	messageType, payload, err := readWireMessage(reader)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != messagePressureState {
		t.Fatalf("first adapter message type=%d, want pressure state", messageType)
	}
	initial, err := decodePressureSnapshot(payload)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Sequence != 0 || initial.ObservedAt.UnixNano() != 123 {
		t.Fatalf("unexpected initial pressure snapshot: %+v", initial)
	}

	rgb := make([]byte, floor.RGBByteCount)
	rgb[0], rgb[1], rgb[2] = 10, 20, 30
	desired, err := encodeDesiredFrame(5, rgb)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriter(client)
	if err := writeWireMessage(writer, messageDesiredFrame, desired); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if frame, ok := frames.snapshot(); ok {
			if frame.Sequence != 5 || frame.RGB[0] != 10 || frame.RGB[1] != 20 || frame.RGB[2] != 30 {
				t.Fatalf("unexpected stored frame: %+v", frame)
			}
			cancel()
			<-done
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("engine frame was not received")
}

func TestEngineSessionRejectsUnexpectedMessage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ReadTimeout = time.Second
	cfg.WriteTimeout = time.Second
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Now()}
	status := newRuntimeStatus()
	hub := newEngineHub(cfg, frames, pressure, status)

	server, client := net.Pipe()
	defer client.Close()
	session := hub.attach(server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		session.run(ctx)
		close(done)
	}()

	reader := bufio.NewReader(client)
	if _, _, err := readWireMessage(reader); err != nil { // initial pressure
		t.Fatal(err)
	}
	writer := bufio.NewWriter(client)
	if err := writeWireMessage(writer, messageOutputState, nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unexpected engine message did not close the session")
	}
}
