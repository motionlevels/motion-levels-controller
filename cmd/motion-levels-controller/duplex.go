package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/motionlevels/motion-levels-controller/contracts/floorpb"
	"github.com/motionlevels/motion-levels-controller/contracts/pbstream"
	"github.com/motionlevels/motion-levels-controller/contracts/recordingpb"
	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

const (
	floorProtocolVersion  = 2
	maxFloorEnvelopeBytes = 1 << 20
)

type duplexHub struct {
	cfg             config
	mu              sync.RWMutex
	client          *duplexClient
	pressureSeq     atomic.Uint64
	droppedPressure atomic.Uint64
}

type duplexClient struct {
	conn      net.Conn
	pressure  chan *floorpb.Envelope
	presented chan *floorpb.Envelope
	status    chan *floorpb.Envelope
	done      chan struct{}
	closeOnce sync.Once
}

func newDuplexHub(cfg config) *duplexHub {
	return &duplexHub{cfg: cfg}
}

func listenDuplexStream(ctx context.Context, addr string, gate *gameEngineStreamGate, hub *duplexHub, onFrame func(*recordingpb.FrameRecord, time.Time), onConnect, onDisconnect func()) error {
	if strings.TrimSpace(addr) == "" {
		<-ctx.Done()
		return context.Canceled
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	log.Printf("floor protocol v2 duplex stream: %s", addr)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return context.Canceled
			}
			log.Printf("duplex stream accept: %v", err)
			continue
		}
		if !gate.tryAcquire() {
			log.Printf("duplex stream rejected: another engine is active remote=%s", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		go func() {
			defer gate.release()
			handleDuplexConnection(ctx, conn, hub, onFrame, onConnect, onDisconnect)
		}()
	}
}

func handleDuplexConnection(parent context.Context, conn net.Conn, hub *duplexHub, onFrame func(*recordingpb.FrameRecord, time.Time), onConnect, onDisconnect func()) {
	connected := false
	defer func() {
		if connected {
			onDisconnect()
		}
		_ = conn.Close()
	}()

	reader := bufio.NewReaderSize(conn, 1<<20)
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		log.Printf("duplex handshake deadline: %v", err)
		return
	}
	var first floorpb.Envelope
	if err := pbstream.ReadLimit(reader, &first, maxFloorEnvelopeBytes); err != nil {
		log.Printf("duplex handshake read: %v", err)
		return
	}
	hello := first.GetEngineHello()
	if hello == nil || hello.ProtocolVersion != floorProtocolVersion {
		log.Printf("duplex handshake rejected: protocol=%d remote=%s", hello.GetProtocolVersion(), conn.RemoteAddr())
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("duplex handshake clear deadline: %v", err)
		return
	}

	writer := bufio.NewWriterSize(conn, 1<<20)
	if err := pbstream.Write(writer, &floorpb.Envelope{Payload: &floorpb.Envelope_AdapterHello{
		AdapterHello: &floorpb.AdapterHello{
			ProtocolVersion: floorProtocolVersion,
			AdapterRevision: buildRevision,
			Width:           floor.GridWidth,
			Height:          floor.GridHeight,
			TargetFps:       uint32(hub.cfg.RefreshFPS),
		},
	}}); err != nil {
		log.Printf("duplex handshake write: %v", err)
		return
	}
	if err := writer.Flush(); err != nil {
		log.Printf("duplex handshake flush: %v", err)
		return
	}

	client := &duplexClient{
		conn:      conn,
		pressure:  make(chan *floorpb.Envelope, 256),
		presented: make(chan *floorpb.Envelope, 1),
		status:    make(chan *floorpb.Envelope, 1),
		done:      make(chan struct{}),
	}
	if !hub.attach(client) {
		log.Printf("duplex stream rejected after handshake: active client remote=%s", conn.RemoteAddr())
		return
	}
	connected = true
	onConnect()
	log.Printf("game-engine connected via floor protocol v2: %s revision=%s", conn.RemoteAddr(), hello.EngineRevision)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer client.close()
	defer hub.detach(client)
	go client.writeLoop(ctx, writer)

	var lastSequence uint64
	for {
		var envelope floorpb.Envelope
		if err := pbstream.ReadLimit(reader, &envelope, maxFloorEnvelopeBytes); err != nil {
			if err != io.EOF && parent.Err() == nil {
				log.Printf("duplex stream read: %v", err)
			}
			return
		}
		desired := envelope.GetDesiredFrame()
		if desired == nil {
			log.Printf("duplex stream rejected unexpected engine message remote=%s", conn.RemoteAddr())
			return
		}
		if desired.Sequence <= lastSequence {
			log.Printf("duplex stream ignored stale desired frame sequence=%d last=%d", desired.Sequence, lastSequence)
			continue
		}
		frame, err := desiredFrameRecord(desired)
		if err != nil {
			log.Printf("duplex stream invalid desired frame: %v", err)
			return
		}
		lastSequence = desired.Sequence
		onFrame(frame, time.Now())
	}
}

func (h *duplexHub) attach(client *duplexClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		return false
	}
	h.client = client
	return true
}

func (h *duplexHub) detach(client *duplexClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client == client {
		h.client = nil
	}
}

func (h *duplexHub) current() *duplexClient {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

func (h *duplexHub) active() bool {
	return h.current() != nil
}

func (h *duplexHub) broadcastPressure(event pressEvent) {
	client := h.current()
	if client == nil {
		return
	}
	envelope := &floorpb.Envelope{Payload: &floorpb.Envelope_PressureEvent{
		PressureEvent: &floorpb.PressureEvent{
			Sequence:           h.pressureSeq.Add(1),
			UnixNanos:          time.Now().UnixNano(),
			X:                  uint32(event.X),
			Y:                  uint32(event.Y),
			Pressed:            event.Pressed,
			HardwareController: uint32(event.Controller),
			HardwareChannel:    uint32(event.Channel),
			HardwarePosition:   uint32(event.Position),
		},
	}}
	select {
	case client.pressure <- envelope:
	default:
		h.droppedPressure.Add(1)
	}
}

func (h *duplexHub) broadcastPresented(presentationSequence, desiredSequence uint64, presentedAt time.Time, width, height uint32, tiles []floor.Tile, fade float64) {
	client := h.current()
	if client == nil {
		return
	}
	rgb, pressure := framePayload(width, height, tiles)
	replaceLatest(client.presented, &floorpb.Envelope{Payload: &floorpb.Envelope_PresentedFrame{
		PresentedFrame: &floorpb.PresentedFrame{
			PresentationSequence: presentationSequence,
			DesiredSequence:      desiredSequence,
			PresentedUnixNanos:   presentedAt.UnixNano(),
			Width:                width,
			Height:               height,
			Rgb:                  rgb,
			PressureBits:         pressure,
			FadeRatio:            float32(fade),
		},
	}})
}

func (h *duplexHub) broadcastStatus(status statusMessage) {
	client := h.current()
	if client == nil {
		return
	}
	replaceLatest(client.status, &floorpb.Envelope{Payload: &floorpb.Envelope_AdapterStatus{
		AdapterStatus: &floorpb.AdapterStatus{
			UnixNanos:             time.Now().UnixNano(),
			PresentedFrames:       status.PresentedFrames,
			ActualFps:             status.ActualFPS,
			TargetFps:             uint32(status.RefreshFPS),
			DesiredFrameAgeMillis: status.GameFrameAgeMS,
			UdpSendErrors:         status.UDPErrorCount,
		},
	}})
}

func (c *duplexClient) writeLoop(ctx context.Context, writer *bufio.Writer) {
	defer c.close()
	for {
		envelope, ok := c.next(ctx)
		if !ok {
			return
		}
		if err := pbstream.Write(writer, envelope); err != nil {
			log.Printf("duplex stream write: %v", err)
			c.close()
			return
		}
		if err := writer.Flush(); err != nil {
			log.Printf("duplex stream flush: %v", err)
			c.close()
			return
		}
	}
}

func (c *duplexClient) next(ctx context.Context) (*floorpb.Envelope, bool) {
	select {
	case envelope := <-c.pressure:
		return envelope, true
	default:
	}
	select {
	case <-ctx.Done():
		return nil, false
	case <-c.done:
		return nil, false
	case envelope := <-c.pressure:
		return envelope, true
	case envelope := <-c.presented:
		return envelope, true
	case envelope := <-c.status:
		return envelope, true
	}
}

func (c *duplexClient) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func replaceLatest(channel chan *floorpb.Envelope, envelope *floorpb.Envelope) {
	select {
	case channel <- envelope:
		return
	default:
	}
	select {
	case <-channel:
	default:
	}
	select {
	case channel <- envelope:
	default:
	}
}

func desiredFrameRecord(frame *floorpb.DesiredFrame) (*recordingpb.FrameRecord, error) {
	if frame.Width != floor.GridWidth || frame.Height != floor.GridHeight {
		return nil, fmt.Errorf("grid is %dx%d, want %dx%d", frame.Width, frame.Height, floor.GridWidth, floor.GridHeight)
	}
	wantBytes := int(frame.Width * frame.Height * 3)
	if len(frame.Rgb) != wantBytes {
		return nil, fmt.Errorf("RGB payload is %d bytes, want %d", len(frame.Rgb), wantBytes)
	}
	if frame.Sequence == 0 {
		return nil, fmt.Errorf("sequence must be positive")
	}
	tiles := make([]*recordingpb.TileState, 0, frame.Width*frame.Height)
	for index := 0; index < int(frame.Width*frame.Height); index++ {
		offset := index * 3
		tiles = append(tiles, &recordingpb.TileState{
			X: uint32(index) % frame.Width,
			Y: uint32(index) / frame.Width,
			R: uint32(frame.Rgb[offset]),
			G: uint32(frame.Rgb[offset+1]),
			B: uint32(frame.Rgb[offset+2]),
		})
	}
	return &recordingpb.FrameRecord{
		Sequence:          frame.Sequence,
		UnixNanos:         frame.UnixNanos,
		Width:             frame.Width,
		Height:            frame.Height,
		Tiles:             tiles,
		GameFrameSequence: frame.Sequence,
		GameUnixNanos:     frame.UnixNanos,
	}, nil
}

func framePayload(width, height uint32, tiles []floor.Tile) ([]byte, []byte) {
	tileCount := int(width * height)
	rgb := make([]byte, tileCount*3)
	pressure := make([]byte, (tileCount+7)/8)
	for _, tile := range tiles {
		if tile.X < 0 || tile.Y < 0 || tile.X >= int(width) || tile.Y >= int(height) {
			continue
		}
		index := tile.Y*int(width) + tile.X
		rgb[index*3] = tile.R
		rgb[index*3+1] = tile.G
		rgb[index*3+2] = tile.B
		if tile.Pressed {
			pressure[index/8] |= 1 << uint(index%8)
		}
	}
	return rgb, pressure
}
