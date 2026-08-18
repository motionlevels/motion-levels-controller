package adapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type engineHub struct {
	cfg      Config
	frames   *frameStore
	pressure *pressureStore
	status   *runtimeStatus

	mu         sync.RWMutex
	generation atomic.Uint64
	current    *engineSession
	lastOutput OutputSnapshot
	hasOutput  bool
}

func newEngineHub(cfg Config, frames *frameStore, pressure *pressureStore, status *runtimeStatus) *engineHub {
	return &engineHub{cfg: cfg, frames: frames, pressure: pressure, status: status}
}

func (h *engineHub) attach(conn net.Conn) *engineSession {
	generation := h.generation.Add(1)
	// Wait for any in-flight physical frame transaction, then invalidate the
	// retired generation before exposing the replacement session.
	h.frames.beginGeneration(generation)
	h.status.clearFrame()

	h.mu.Lock()
	old := h.current
	session := &engineSession{
		conn:       conn,
		generation: generation,
		hub:        h,
		pressure:   make(chan PressureSnapshot, 1),
		output:     make(chan OutputSnapshot, 1),
		done:       make(chan struct{}),
	}
	h.current = session
	h.status.setEngineConnected(true)
	initialPressure := h.pressure.snapshot()
	lastOutput := h.lastOutput
	hasOutput := h.hasOutput
	h.mu.Unlock()

	if old != nil {
		old.close()
	}
	replaceLatest(session.pressure, initialPressure)
	if hasOutput {
		replaceLatest(session.output, lastOutput)
	}
	return session
}

func (h *engineHub) detach(session *engineSession) {
	h.mu.Lock()
	if h.current == session {
		h.current = nil
		h.status.setEngineConnected(false)
	}
	h.mu.Unlock()
}

func (h *engineHub) close() {
	h.mu.RLock()
	current := h.current
	h.mu.RUnlock()
	if current != nil {
		current.close()
	}
}

func (h *engineHub) publishPressure(snapshot PressureSnapshot) {
	h.mu.RLock()
	current := h.current
	h.mu.RUnlock()
	if current != nil {
		replaceLatest(current.pressure, snapshot)
	}
}

func (h *engineHub) publishOutput(snapshot OutputSnapshot) {
	h.mu.Lock()
	h.lastOutput = snapshot
	h.hasOutput = true
	current := h.current
	h.mu.Unlock()
	if current != nil {
		replaceLatest(current.output, snapshot)
	}
}

type engineServer struct {
	cfg Config
	hub *engineHub
}

func (s *engineServer) run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.EngineAddr)
	if err != nil {
		return fmt.Errorf("listen for engine: %w", err)
	}
	defer listener.Close()
	log.Printf("engine stream: %s", listener.Addr())

	go func() {
		<-ctx.Done()
		_ = listener.Close()
		s.hub.close()
	}()

	var sessions sync.WaitGroup
	defer sessions.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept engine connection: %w", err)
		}
		session := s.hub.attach(conn)
		log.Printf("engine connected: %s generation=%d", conn.RemoteAddr(), session.generation)
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			session.run(ctx)
			s.hub.detach(session)
			log.Printf("engine disconnected: %s generation=%d", conn.RemoteAddr(), session.generation)
		}()
	}
}

type engineSession struct {
	conn       net.Conn
	generation uint64
	hub        *engineHub
	pressure   chan PressureSnapshot
	output     chan OutputSnapshot
	done       chan struct{}
	closeOnce  sync.Once
}

func (s *engineSession) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer s.close()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer s.close()
		s.writeLoop(ctx)
	}()

	s.readLoop(ctx)
	cancel()
	s.close()
	<-writerDone
}

func (s *engineSession) readLoop(ctx context.Context) {
	reader := bufio.NewReaderSize(s.conn, maxWirePayload+wireHeaderSize)
	var lastSequence uint64
	var staleCount uint64
	var lastStaleLog time.Time
	for {
		if err := s.conn.SetReadDeadline(time.Now().Add(s.hub.cfg.ReadTimeout)); err != nil {
			return
		}
		messageType, payload, err := readWireMessage(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				log.Printf("engine read: %v", err)
			}
			return
		}
		if messageType != messageDesiredFrame {
			log.Printf("engine sent unexpected message type %d", messageType)
			return
		}
		sequence, rgb, err := decodeDesiredFrame(payload)
		if err != nil {
			log.Printf("invalid desired frame: %v", err)
			return
		}
		if sequence <= lastSequence {
			staleCount++
			now := time.Now()
			if now.Sub(lastStaleLog) >= time.Second {
				log.Printf("ignored %d stale desired frame(s) sequence=%d last=%d", staleCount, sequence, lastSequence)
				staleCount = 0
				lastStaleLog = now
			}
			continue
		}
		receivedAt := time.Now()
		if !s.hub.frames.store(s.generation, sequence, rgb, receivedAt) {
			return
		}
		lastSequence = sequence
		s.hub.status.markFrame(sequence, receivedAt)
	}
}

func (s *engineSession) writeLoop(ctx context.Context) {
	writer := bufio.NewWriterSize(s.conn, maxWirePayload+wireHeaderSize)
	for {
		messageType, payload, ok := s.nextOutgoing(ctx)
		if !ok {
			return
		}
		if err := s.conn.SetWriteDeadline(time.Now().Add(s.hub.cfg.WriteTimeout)); err != nil {
			return
		}
		if err := writeWireMessage(writer, messageType, payload); err != nil {
			if ctx.Err() == nil {
				log.Printf("engine write: %v", err)
			}
			return
		}
		if err := writer.Flush(); err != nil {
			if ctx.Err() == nil {
				log.Printf("engine flush: %v", err)
			}
			return
		}
	}
}

func (s *engineSession) nextOutgoing(ctx context.Context) (byte, []byte, bool) {
	select {
	case pressure := <-s.pressure:
		return messagePressureState, encodePressureSnapshot(pressure), true
	default:
	}
	select {
	case <-ctx.Done():
		return 0, nil, false
	case <-s.done:
		return 0, nil, false
	case pressure := <-s.pressure:
		return messagePressureState, encodePressureSnapshot(pressure), true
	case output := <-s.output:
		return messageOutputState, encodeOutputSnapshot(output), true
	}
}

func (s *engineSession) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}
