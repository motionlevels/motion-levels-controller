package main

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/contracts/floorpb"
	"github.com/motionlevels/motion-levels-controller/contracts/pbstream"
	"github.com/motionlevels/motion-levels-controller/contracts/recordingpb"
	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestDesiredFrameRecordRejectsInvalidShape(t *testing.T) {
	tests := []struct {
		name  string
		frame *floorpb.DesiredFrame
	}{
		{name: "zero sequence", frame: &floorpb.DesiredFrame{Width: floor.GridWidth, Height: floor.GridHeight, Rgb: make([]byte, floor.GridWidth*floor.GridHeight*3)}},
		{name: "wrong grid", frame: &floorpb.DesiredFrame{Sequence: 1, Width: 8, Height: 8, Rgb: make([]byte, 8*8*3)}},
		{name: "short RGB", frame: &floorpb.DesiredFrame{Sequence: 1, Width: floor.GridWidth, Height: floor.GridHeight, Rgb: []byte{1, 2, 3}}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := desiredFrameRecord(testCase.frame); err == nil {
				t.Fatal("expected invalid desired frame error")
			}
		})
	}
}

func TestDesiredFrameRecordConvertsPackedRGB(t *testing.T) {
	rgb := make([]byte, floor.GridWidth*floor.GridHeight*3)
	index := 2*floor.GridWidth + 3
	rgb[index*3] = 11
	rgb[index*3+1] = 22
	rgb[index*3+2] = 33

	record, err := desiredFrameRecord(&floorpb.DesiredFrame{
		Sequence:  7,
		UnixNanos: 123,
		Width:     floor.GridWidth,
		Height:    floor.GridHeight,
		Rgb:       rgb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Sequence != 7 || record.GameFrameSequence != 7 || record.GameUnixNanos != 123 {
		t.Fatalf("unexpected technical lineage: %+v", record)
	}
	tile := record.Tiles[index]
	if tile.X != 3 || tile.Y != 2 || tile.R != 11 || tile.G != 22 || tile.B != 33 {
		t.Fatalf("unexpected converted tile: %+v", tile)
	}
	if record.SessionId != "" || record.VenueSessionId != "" {
		t.Fatalf("business identity leaked into adapter frame: %+v", record)
	}
}

func TestDuplexHandshakeCarriesFramesAndObservedEvents(t *testing.T) {
	server, engine := net.Pipe()
	defer engine.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := newDuplexHub(config{RefreshFPS: 50})
	frames := make(chan *recordingpb.FrameRecord, 1)
	connected := make(chan bool, 2)
	go handleDuplexConnection(ctx, server, hub, func(frame *recordingpb.FrameRecord, _ time.Time) {
		frames <- frame
	}, func() {
		connected <- true
	}, func() {
		connected <- false
	})

	writer := bufio.NewWriter(engine)
	if err := pbstream.Write(writer, &floorpb.Envelope{Payload: &floorpb.Envelope_EngineHello{
		EngineHello: &floorpb.EngineHello{ProtocolVersion: floorProtocolVersion, EngineRevision: "test-engine"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(engine)
	var helloEnvelope floorpb.Envelope
	if err := pbstream.Read(reader, &helloEnvelope); err != nil {
		t.Fatal(err)
	}
	hello := helloEnvelope.GetAdapterHello()
	if hello == nil || hello.ProtocolVersion != floorProtocolVersion || hello.Width != floor.GridWidth || hello.Height != floor.GridHeight || hello.TargetFps != 50 {
		t.Fatalf("unexpected adapter hello: %+v", hello)
	}
	select {
	case value := <-connected:
		if !value {
			t.Fatal("duplex disconnected before becoming active")
		}
	case <-time.After(time.Second):
		t.Fatal("duplex did not become active")
	}

	rgb := make([]byte, floor.GridWidth*floor.GridHeight*3)
	rgb[0], rgb[1], rgb[2] = 10, 20, 30
	if err := pbstream.Write(writer, &floorpb.Envelope{Payload: &floorpb.Envelope_DesiredFrame{
		DesiredFrame: &floorpb.DesiredFrame{Sequence: 9, UnixNanos: 456, Width: floor.GridWidth, Height: floor.GridHeight, Rgb: rgb},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-frames:
		if frame.Sequence != 9 || frame.Tiles[0].R != 10 || frame.Tiles[0].G != 20 || frame.Tiles[0].B != 30 {
			t.Fatalf("unexpected desired frame: %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("controller did not receive desired frame")
	}

	hub.broadcastPressure(pressEvent{Controller: 1, Channel: 2, Position: 3, X: 4, Y: 5, Pressed: true})
	var pressureEnvelope floorpb.Envelope
	if err := pbstream.Read(reader, &pressureEnvelope); err != nil {
		t.Fatal(err)
	}
	pressure := pressureEnvelope.GetPressureEvent()
	if pressure == nil || pressure.X != 4 || pressure.Y != 5 || !pressure.Pressed || pressure.HardwareController != 1 || pressure.HardwareChannel != 2 || pressure.HardwarePosition != 3 {
		t.Fatalf("unexpected pressure event: %+v", pressure)
	}

	hub.broadcastPresented(12, 9, time.Unix(0, 789), floor.GridWidth, floor.GridHeight, []floor.Tile{{X: 1, Y: 2, R: 7, G: 8, B: 9, Pressed: true}}, 0.25)
	var presentedEnvelope floorpb.Envelope
	if err := pbstream.Read(reader, &presentedEnvelope); err != nil {
		t.Fatal(err)
	}
	presented := presentedEnvelope.GetPresentedFrame()
	index := 2*floor.GridWidth + 1
	if presented == nil || presented.PresentationSequence != 12 || presented.DesiredSequence != 9 || presented.PresentedUnixNanos != 789 || presented.FadeRatio != 0.25 {
		t.Fatalf("unexpected presented metadata: %+v", presented)
	}
	if presented.Rgb[index*3] != 7 || presented.Rgb[index*3+1] != 8 || presented.Rgb[index*3+2] != 9 || presented.PressureBits[index/8]&(1<<uint(index%8)) == 0 {
		t.Fatal("presented snapshot did not preserve RGB and pressure")
	}

	cancel()
	select {
	case value := <-connected:
		if value {
			t.Fatal("expected disconnect notification")
		}
	case <-time.After(time.Second):
		t.Fatal("duplex did not disconnect on cancellation")
	}
}

func TestDuplexRejectsWrongProtocolWithoutConnecting(t *testing.T) {
	server, engine := net.Pipe()
	defer engine.Close()
	hub := newDuplexHub(config{RefreshFPS: 50})
	connected := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		handleDuplexConnection(context.Background(), server, hub, func(*recordingpb.FrameRecord, time.Time) {}, func() {
			connected <- true
		}, func() {
			connected <- false
		})
		close(done)
	}()

	writer := bufio.NewWriter(engine)
	if err := pbstream.Write(writer, &floorpb.Envelope{Payload: &floorpb.Envelope_EngineHello{
		EngineHello: &floorpb.EngineHello{ProtocolVersion: 99},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wrong protocol handshake was not rejected")
	}
	select {
	case value := <-connected:
		t.Fatalf("unexpected connection callback: %v", value)
	default:
	}
}

func TestReplaceLatestDropsOnlySupersededSnapshot(t *testing.T) {
	channel := make(chan *floorpb.Envelope, 1)
	first := &floorpb.Envelope{Payload: &floorpb.Envelope_AdapterStatus{AdapterStatus: &floorpb.AdapterStatus{PresentedFrames: 1}}}
	second := &floorpb.Envelope{Payload: &floorpb.Envelope_AdapterStatus{AdapterStatus: &floorpb.AdapterStatus{PresentedFrames: 2}}}
	replaceLatest(channel, first)
	replaceLatest(channel, second)
	if got := (<-channel).GetAdapterStatus().PresentedFrames; got != 2 {
		t.Fatalf("latest status frames = %d, want 2", got)
	}
}
