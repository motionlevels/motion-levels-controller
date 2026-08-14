package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/motionlevels/motion-levels-controller/contracts/inputpb"
	"github.com/motionlevels/motion-levels-controller/contracts/pbstream"
	"github.com/motionlevels/motion-levels-controller/contracts/recordingpb"
	"github.com/motionlevels/motion-levels-controller/internal/floor"
	"golang.org/x/net/ipv4"
	"google.golang.org/protobuf/proto"
)

type config struct {
	HTTPAddr           string
	FrameAddr          string
	InputAddr          string
	DuplexAddr         string
	RecvPort           int
	FloorSourceIP      string
	BroadcastIP        string
	BroadcastPort      int
	RefreshFPS         int
	EngineFadeDelay    time.Duration
	EngineFadeDuration time.Duration
}

type pressureStreamHub struct {
	mu      sync.Mutex
	clients map[*pressureStreamClient]bool
	seq     uint64
}

type pressureStreamClient struct {
	conn net.Conn
	jobs chan *inputpb.PressureEvent
}

type controllerMetrics struct {
	startedAt              time.Time
	presentedFrames        atomic.Uint64
	presentedFramesWindow  atomic.Uint64
	actualFPSBits          atomic.Uint64
	udpSendErrors          atomic.Uint64
	udpSourceResolveRuns   atomic.Uint64
	udpSourceAssigned      atomic.Bool
	udpTransportKnown      atomic.Bool
	udpTransportAvailable  atomic.Bool
	lastUDPSuccessUnixNano atomic.Int64
	lastPresentedUnixNanos atomic.Int64
	lastGameFrameSequence  atomic.Uint64
	lastGameFrameUnixNanos atomic.Int64
	lastGameFrameReceived  atomic.Int64
	lastGameSessionID      atomic.Value
	syncSamples            atomic.Uint64
	syncOffsetNanos        atomic.Int64
	syncJitterNanos        atomic.Int64
	presentLatencyNanos    atomic.Int64
	gameEngineConnections  atomic.Int64
	lastGameDisconnected   atomic.Int64
}

type controllerState struct {
	mu          sync.RWMutex
	pressed     [floor.GridHeight][floor.GridWidth]bool
	sensorState map[sensorKey]bool
}

type latestFrameBuffer struct {
	mu    sync.RWMutex
	frame *recordingpb.FrameRecord
}

type gameEngineStreamGate struct {
	mu     sync.Mutex
	active bool
}

type frameGrid [floor.GridHeight][floor.GridWidth]floor.RGB

type sensorKey struct {
	controller int
	channel    int
	position   int
}

type pressEvent struct {
	Source     string
	Controller int
	Channel    int
	Position   int
	X          int
	Y          int
	Pressed    bool
}

type statusMessage struct {
	UptimeSeconds     int64
	PresentedFrames   uint64
	ActualFPS         float64
	RefreshFPS        int
	UDPErrorCount     uint64
	UDPSourceAssigned bool
	UDPTransportKnown bool
	UDPTransportReady bool
	UDPResolveRuns    uint64
	LastUDPSuccessMS  int64
	GameFrameAgeMS    int64
	GameFrameSequence uint64
	GameEngineOnline  bool
	EngineFadeAmount  float64
	Sync              syncStatus
}

type syncStatus struct {
	Status                string
	SessionID             string
	Samples               uint64
	EngineClockOffsetMS   float64
	PresentLatencyMS      float64
	JitterMS              float64
	LastGameFrameSequence uint64
}

func main() {
	cfg := parseConfig()
	if err := cfg.validate(); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func parseConfig() config {
	cfg := config{}
	flag.StringVar(&cfg.HTTPAddr, "http", "127.0.0.1:4101", "HTTP address for minimal health and Prometheus endpoints")
	flag.StringVar(&cfg.FrameAddr, "frames", "127.0.0.1:4201", "TCP address for length-prefixed protobuf frame stream")
	flag.StringVar(&cfg.InputAddr, "input-events", "127.0.0.1:4202", "TCP address for pressure event subscribers; empty disables")
	flag.StringVar(&cfg.DuplexAddr, "duplex", "127.0.0.1:4203", "TCP address for the protocol-v2 duplex floor stream; empty disables")
	flag.IntVar(&cfg.RecvPort, "recv-port", 7800, "UDP port for tile handshake/sensor packets")
	flag.StringVar(&cfg.FloorSourceIP, "floor-source-ip", os.Getenv("MOTION_LEVELS_FLOOR_SOURCE_IP"), "local IPv4 source address for floor UDP output; empty uses the default route")
	flag.StringVar(&cfg.BroadcastIP, "broadcast-ip", "255.255.255.255", "UDP broadcast IP for LED packets")
	flag.IntVar(&cfg.BroadcastPort, "broadcast-port", 4626, "UDP broadcast port for LED packets")
	flag.IntVar(&cfg.RefreshFPS, "refresh-fps", 50, "floor-adapter physical UDP refresh rate")
	flag.DurationVar(&cfg.EngineFadeDelay, "engine-fade-delay", 2*time.Second, "time to hold the last game frame after the game-engine disconnects before fading")
	flag.DurationVar(&cfg.EngineFadeDuration, "engine-fade-duration", 3*time.Second, "duration of the fade to black after engine-fade-delay")
	flag.Parse()
	return cfg
}

func (c config) validate() error {
	var errs []error
	if c.RefreshFPS < 1 {
		errs = append(errs, fmt.Errorf("refresh-fps must be at least 1"))
	}
	if c.RecvPort < 1 || c.RecvPort > 65535 {
		errs = append(errs, fmt.Errorf("recv-port must be between 1 and 65535"))
	}
	if c.BroadcastPort < 1 || c.BroadcastPort > 65535 {
		errs = append(errs, fmt.Errorf("broadcast-port must be between 1 and 65535"))
	}
	if value := strings.TrimSpace(c.FloorSourceIP); value != "" {
		if parsed := net.ParseIP(value); parsed == nil || parsed.To4() == nil {
			errs = append(errs, fmt.Errorf("floor-source-ip must be a valid IPv4 address"))
		}
	}
	if c.EngineFadeDelay < 0 {
		errs = append(errs, fmt.Errorf("engine-fade-delay must be non-negative"))
	}
	if c.EngineFadeDuration < 0 {
		errs = append(errs, fmt.Errorf("engine-fade-duration must be non-negative"))
	}
	if net.ParseIP(c.BroadcastIP) == nil {
		errs = append(errs, fmt.Errorf("broadcast-ip must be a valid IPv4 or IPv6 address"))
	}
	if floor.DefaultControllers*floor.DefaultChannels*floor.DefaultLEDsPerChannel != floor.GridWidth*floor.GridHeight {
		errs = append(errs, fmt.Errorf("hardware LED count does not match logical grid"))
	}
	return errors.Join(errs...)
}

func run(ctx context.Context, cfg config) error {
	conn, err := openUDP(cfg.RecvPort)
	if err != nil {
		return err
	}
	defer conn.Close()
	metrics := newControllerMetrics()
	sender, err := newUDPSender(conn, cfg.FloorSourceIP, metrics)
	if err != nil {
		return err
	}

	broadcastAddr := &net.UDPAddr{IP: net.ParseIP(cfg.BroadcastIP), Port: cfg.BroadcastPort}
	pressureStreams := &pressureStreamHub{clients: make(map[*pressureStreamClient]bool)}
	duplex := newDuplexHub(cfg)
	state := &controllerState{sensorState: make(map[sensorKey]bool)}
	frames := &latestFrameBuffer{}
	log.Printf("config: %s", cfg)

	go metricsLoop(ctx, metrics)
	go statusLoop(ctx, cfg, metrics, duplex)
	go readUDP(ctx, conn, state, pressureStreams, duplex)
	go serveHTTP(ctx, cfg, metrics)
	go listenPressureSubscribers(ctx, cfg.InputAddr, pressureStreams)
	go syncLoop(ctx, sender, broadcastAddr, metrics)
	go presentationLoop(ctx, cfg, sender, broadcastAddr, state, frames, metrics, duplex)
	onFrame := func(record *recordingpb.FrameRecord, receivedAt time.Time) {
		metrics.markGameFrame(record, receivedAt)
		frames.update(record)
	}
	gate := &gameEngineStreamGate{}
	errors := make(chan error, 2)
	go func() {
		errors <- listenFrameStream(ctx, cfg.FrameAddr, gate, onFrame, metrics.markGameEngineConnected, metrics.markGameEngineDisconnected)
	}()
	go func() {
		errors <- listenDuplexStream(ctx, cfg.DuplexAddr, gate, duplex, onFrame, metrics.markGameEngineConnected, metrics.markGameEngineDisconnected)
	}()
	return <-errors
}

func openUDP(port int) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, err
	}
	if err := setBroadcast(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func setBroadcast(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	err = raw.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	})
	if err != nil {
		return err
	}
	return setErr
}

type udpSender struct {
	conn          *net.UDPConn
	packet        *ipv4.PacketConn
	sourceIP      net.IP
	metrics       *controllerMetrics
	retryInterval time.Duration
	now           func() time.Time
	resolveSource func(net.IP) (udpSourceBinding, error)

	mu                sync.Mutex
	binding           *udpSourceBinding
	nextResolve       time.Time
	lastResolveError  error
	availabilityKnown bool
	available         bool
}

type udpSourceBinding struct {
	control       *ipv4.ControlMessage
	interfaceName string
}

var errUDPSourceUnavailable = errors.New("configured floor UDP source is unavailable")

func newUDPSender(conn *net.UDPConn, sourceIPValue string, metrics *controllerMetrics) (*udpSender, error) {
	sourceIPValue = strings.TrimSpace(sourceIPValue)
	if sourceIPValue == "" {
		if metrics != nil {
			metrics.udpSourceAssigned.Store(true)
		}
		return &udpSender{conn: conn, metrics: metrics, now: time.Now}, nil
	}

	sourceIP := net.ParseIP(sourceIPValue)
	if sourceIP == nil || sourceIP.To4() == nil {
		return nil, fmt.Errorf("floor UDP source %q is not a valid IPv4 address", sourceIPValue)
	}
	return &udpSender{
		conn:          conn,
		packet:        ipv4.NewPacketConn(conn),
		sourceIP:      sourceIP.To4(),
		metrics:       metrics,
		retryInterval: time.Second,
		now:           time.Now,
		resolveSource: resolveUDPSource,
	}, nil
}

func resolveUDPSource(sourceIP net.IP) (udpSourceBinding, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return udpSourceBinding{}, fmt.Errorf("list interfaces for floor UDP source: %w", err)
	}
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			addressIP, _, err := net.ParseCIDR(address.String())
			if err == nil && addressIP.Equal(sourceIP) {
				return udpSourceBinding{
					interfaceName: networkInterface.Name,
					control: &ipv4.ControlMessage{
						IfIndex: networkInterface.Index,
						Src:     sourceIP.To4(),
					},
				}, nil
			}
		}
	}
	return udpSourceBinding{}, fmt.Errorf("%w: %s is not assigned to an active local interface", errUDPSourceUnavailable, sourceIP)
}

func (s *udpSender) WriteToUDP(packet []byte, addr *net.UDPAddr) (int, error) {
	if s.sourceIP == nil {
		count, err := s.conn.WriteToUDP(packet, addr)
		s.markWriteResult(err)
		return count, err
	}
	binding, err := s.currentBinding()
	if err != nil {
		s.markUnavailable(err)
		return 0, err
	}
	count, err := s.packet.WriteTo(packet, binding.control, addr)
	if err != nil {
		s.invalidateBinding(binding, err)
		return count, err
	}
	s.markAvailable(binding)
	return count, nil
}

func (s *udpSender) currentBinding() (*udpSourceBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding != nil {
		return s.binding, nil
	}
	now := s.now()
	if now.Before(s.nextResolve) {
		return nil, s.lastResolveError
	}
	if s.metrics != nil {
		s.metrics.udpSourceResolveRuns.Add(1)
	}
	binding, err := s.resolveSource(s.sourceIP)
	if err != nil {
		s.nextResolve = now.Add(s.retryInterval)
		s.lastResolveError = err
		if s.metrics != nil {
			s.metrics.udpSourceAssigned.Store(false)
		}
		return nil, err
	}
	s.binding = &binding
	s.nextResolve = time.Time{}
	s.lastResolveError = nil
	if s.metrics != nil {
		s.metrics.udpSourceAssigned.Store(true)
	}
	return s.binding, nil
}

func (s *udpSender) invalidateBinding(binding *udpSourceBinding, reason error) {
	s.mu.Lock()
	if s.binding == binding {
		s.binding = nil
		s.nextResolve = time.Time{}
		s.lastResolveError = reason
	}
	s.mu.Unlock()
	if s.metrics != nil {
		s.metrics.udpSourceAssigned.Store(false)
	}
	s.markUnavailable(reason)
}

func (s *udpSender) markWriteResult(err error) {
	if err != nil {
		s.markUnavailable(err)
		return
	}
	s.markAvailable(nil)
}

func (s *udpSender) markAvailable(binding *udpSourceBinding) {
	if s.metrics != nil {
		s.metrics.udpTransportKnown.Store(true)
		s.metrics.udpTransportAvailable.Store(true)
		s.metrics.lastUDPSuccessUnixNano.Store(s.now().UnixNano())
	}
	s.mu.Lock()
	changed := !s.availabilityKnown || !s.available
	s.availabilityKnown = true
	s.available = true
	s.mu.Unlock()
	if !changed {
		return
	}
	if binding == nil {
		log.Printf("floor UDP output available using default route")
		return
	}
	log.Printf("floor UDP output available on configured source interface %s", binding.interfaceName)
}

func (s *udpSender) markUnavailable(reason error) {
	if s.metrics != nil {
		s.metrics.udpTransportKnown.Store(true)
		s.metrics.udpTransportAvailable.Store(false)
	}
	s.mu.Lock()
	changed := !s.availabilityKnown || s.available
	s.availabilityKnown = true
	s.available = false
	s.mu.Unlock()
	if changed {
		log.Printf("floor UDP output unavailable; retaining exact configuration and retrying: %v", reason)
	}
}

func readUDP(ctx context.Context, conn *net.UDPConn, state *controllerState, pressureStreams *pressureStreamHub, duplex *duplexHub) {
	buffer := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, addr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		packet := buffer[:n]
		if len(packet) == 0 {
			continue
		}
		switch packet[0] {
		case 0x68:
			log.Printf("tile handshake from %s (%d bytes)", addr, n)
		case 0x88:
			for _, event := range state.applySensorPacket(packet) {
				pressureStreams.broadcast(event)
				duplex.broadcastPressure(event)
			}
		default:
			log.Printf("unknown UDP packet 0x%02x from %s (%d bytes)", packet[0], addr, n)
		}
	}
}

func listenPressureSubscribers(ctx context.Context, addr string, hub *pressureStreamHub) {
	if addr == "" || hub == nil {
		return
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("pressure event stream disabled: %v", err)
		return
	}
	defer listener.Close()
	log.Printf("pressure event stream: %s", addr)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("pressure event accept: %v", err)
			continue
		}
		client := &pressureStreamClient{conn: conn, jobs: make(chan *inputpb.PressureEvent, 256)}
		hub.add(client)
		go hub.writeClient(client)
	}
}

func newHTTPHandler(cfg config, metrics *controllerMetrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		response := struct {
			Status      string `json:"status"`
			FloorOutput string `json:"floor_output"`
		}{
			Status:      "ok",
			FloorOutput: metrics.floorOutputStatus(),
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("health response: %v", err)
		}
	})
	mux.HandleFunc("/metrics", controllerMetricsHandler(cfg, metrics))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

func serveHTTP(ctx context.Context, cfg config, metrics *controllerMetrics) {
	log.Printf("health and metrics: %s", cfg.HTTPAddr)
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: newHTTPHandler(cfg, metrics)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func listenFrameStream(ctx context.Context, addr string, gate *gameEngineStreamGate, onFrame func(*recordingpb.FrameRecord, time.Time), onConnect, onDisconnect func()) error {
	if strings.TrimSpace(addr) == "" {
		<-ctx.Done()
		return context.Canceled
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	log.Printf("frame stream: %s", addr)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return context.Canceled
			default:
				log.Printf("frame stream accept: %v", err)
				continue
			}
		}
		if !gate.tryAcquire() {
			log.Printf("game-engine stream rejected: already connected remote=%s", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		go handleFrameConnection(conn, onFrame, onConnect, func() {
			onDisconnect()
			gate.release()
		})
	}
}

func handleFrameConnection(conn net.Conn, onFrame func(*recordingpb.FrameRecord, time.Time), onConnect, onDisconnect func()) {
	log.Printf("game-engine connected: %s", conn.RemoteAddr())
	onConnect()
	defer func() {
		onDisconnect()
		log.Printf("game-engine disconnected: %s", conn.RemoteAddr())
		conn.Close()
	}()
	reader := bufio.NewReader(conn)
	for {
		var frame recordingpb.FrameRecord
		if err := pbstream.Read(reader, &frame); err != nil {
			if err != io.EOF {
				log.Printf("frame stream read: %v", err)
			}
			return
		}
		receivedAt := time.Now()
		if frame.ControllerReceivedUnixNanos == 0 {
			frame.ControllerReceivedUnixNanos = receivedAt.UnixNano()
		}
		if frame.GameFrameSequence == 0 {
			frame.GameFrameSequence = frame.Sequence
		}
		if frame.GameUnixNanos == 0 {
			frame.GameUnixNanos = frame.UnixNanos
		}
		onFrame(&frame, receivedAt)
	}
}

func (g *gameEngineStreamGate) tryAcquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active {
		return false
	}
	g.active = true
	return true
}

func (g *gameEngineStreamGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active = false
}

func presentationLoop(ctx context.Context, cfg config, sender *udpSender, addr *net.UDPAddr, state *controllerState, frames *latestFrameBuffer, metrics *controllerMetrics, duplex *duplexHub) {
	log.Printf("presentation refresh: %d fps", cfg.RefreshFPS)
	ticker := time.NewTicker(time.Second / time.Duration(cfg.RefreshFPS))
	defer ticker.Stop()

	var sequence uint64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			frame, ok := frames.snapshot()
			if !ok {
				continue
			}
			tiles := tilesFromFrame(frame, state.snapshotPressed())
			fade := metrics.engineFadeAmount(now, cfg.EngineFadeDelay, cfg.EngineFadeDuration)
			if fade > 0 {
				tiles = fadeTiles(tiles, 1-fade)
			}
			if !sendFrame(sender, addr, tiles, metrics) {
				continue
			}
			sequence++
			metrics.markPresented(sequence, now, frame)
			duplex.broadcastPresented(sequence, frame.Sequence, now, frame.Width, frame.Height, tiles, fade)
		}
	}
}

func newControllerMetrics() *controllerMetrics {
	return &controllerMetrics{startedAt: time.Now()}
}

func metricsLoop(ctx context.Context, metrics *controllerMetrics) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fps := float64(metrics.presentedFramesWindow.Swap(0))
			metrics.actualFPSBits.Store(math.Float64bits(fps))
		}
	}
}

func statusLoop(ctx context.Context, cfg config, metrics *controllerMetrics, duplex *duplexHub) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			duplex.broadcastStatus(snapshotStatus(metrics, cfg))
		}
	}
}

func snapshotStatus(metrics *controllerMetrics, cfg config) statusMessage {
	now := time.Now()
	gameFrameAgeMS := int64(-1)
	if received := metrics.lastGameFrameReceived.Load(); received > 0 {
		gameFrameAgeMS = now.Sub(time.Unix(0, received)).Milliseconds()
	}
	lastUDPSuccessMS := int64(-1)
	if sent := metrics.lastUDPSuccessUnixNano.Load(); sent > 0 {
		lastUDPSuccessMS = now.Sub(time.Unix(0, sent)).Milliseconds()
	}
	return statusMessage{
		UptimeSeconds:     int64(now.Sub(metrics.startedAt).Seconds()),
		PresentedFrames:   metrics.presentedFrames.Load(),
		ActualFPS:         math.Float64frombits(metrics.actualFPSBits.Load()),
		RefreshFPS:        cfg.RefreshFPS,
		UDPErrorCount:     metrics.udpSendErrors.Load(),
		UDPSourceAssigned: metrics.udpSourceAssigned.Load(),
		UDPTransportKnown: metrics.udpTransportKnown.Load(),
		UDPTransportReady: metrics.udpTransportAvailable.Load(),
		UDPResolveRuns:    metrics.udpSourceResolveRuns.Load(),
		LastUDPSuccessMS:  lastUDPSuccessMS,
		GameFrameAgeMS:    gameFrameAgeMS,
		GameFrameSequence: metrics.lastGameFrameSequence.Load(),
		GameEngineOnline:  metrics.gameEngineOnline(),
		EngineFadeAmount:  metrics.engineFadeAmount(now, cfg.EngineFadeDelay, cfg.EngineFadeDuration),
		Sync:              metrics.syncStatus(),
	}
}

func (m *controllerMetrics) markPresented(sequence uint64, now time.Time, frame *recordingpb.FrameRecord) {
	m.presentedFrames.Store(sequence)
	m.presentedFramesWindow.Add(1)
	m.lastPresentedUnixNanos.Store(now.UnixNano())
	if frame == nil {
		return
	}
	gameUnixNanos := frame.GameUnixNanos
	if gameUnixNanos == 0 {
		gameUnixNanos = frame.UnixNanos
	}
	if gameUnixNanos > 0 {
		m.presentLatencyNanos.Store(now.UnixNano() - gameUnixNanos)
	}
}

func (m *controllerMetrics) markGameFrame(frame *recordingpb.FrameRecord, receivedAt time.Time) {
	if frame == nil {
		return
	}
	m.lastGameFrameSequence.Store(frame.Sequence)
	m.lastGameFrameUnixNanos.Store(frame.UnixNanos)
	receivedUnixNanos := receivedAt.UnixNano()
	if frame.ControllerReceivedUnixNanos > 0 {
		receivedUnixNanos = frame.ControllerReceivedUnixNanos
	}
	m.lastGameFrameReceived.Store(receivedUnixNanos)
	if frame.SessionId != "" {
		m.lastGameSessionID.Store(frame.SessionId)
	}
	gameUnixNanos := frame.GameUnixNanos
	if gameUnixNanos == 0 {
		gameUnixNanos = frame.UnixNanos
	}
	if gameUnixNanos <= 0 {
		return
	}
	offset := receivedUnixNanos - gameUnixNanos
	sample := m.syncSamples.Add(1)
	previous := m.syncOffsetNanos.Swap(offset)
	if sample > 1 {
		m.syncJitterNanos.Store(absInt64(offset - previous))
	}
}

func (m *controllerMetrics) syncStatus() syncStatus {
	samples := m.syncSamples.Load()
	offset := m.syncOffsetNanos.Load()
	jitter := m.syncJitterNanos.Load()
	latency := m.presentLatencyNanos.Load()
	status := "unknown"
	if samples > 0 {
		status = "ok"
		if absInt64(offset) > int64(100*time.Millisecond) || jitter > int64(100*time.Millisecond) {
			status = "bad"
		} else if absInt64(offset) > int64(50*time.Millisecond) || jitter > int64(25*time.Millisecond) {
			status = "warn"
		}
		if !m.gameEngineOnline() {
			status = "offline"
		}
	}
	sessionID, _ := m.lastGameSessionID.Load().(string)
	return syncStatus{
		Status:                status,
		SessionID:             sessionID,
		Samples:               samples,
		EngineClockOffsetMS:   nanosToMillis(offset),
		PresentLatencyMS:      nanosToMillis(latency),
		JitterMS:              nanosToMillis(jitter),
		LastGameFrameSequence: m.lastGameFrameSequence.Load(),
	}
}

func nanosToMillis(value int64) float64 {
	return math.Round(float64(value)/float64(time.Millisecond)*10) / 10
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func (m *controllerMetrics) markGameEngineConnected() {
	m.gameEngineConnections.Add(1)
}

func (m *controllerMetrics) markGameEngineDisconnected() {
	if m.gameEngineConnections.Add(-1) <= 0 {
		m.gameEngineConnections.Store(0)
		m.lastGameDisconnected.Store(time.Now().UnixNano())
	}
}

func (m *controllerMetrics) gameEngineOnline() bool {
	return m.gameEngineConnections.Load() > 0
}

func (m *controllerMetrics) engineFadeAmount(now time.Time, delay, duration time.Duration) float64 {
	if m.gameEngineOnline() {
		return 0
	}
	disconnectedAt := m.lastGameDisconnected.Load()
	if disconnectedAt == 0 {
		return 0
	}
	elapsed := now.Sub(time.Unix(0, disconnectedAt))
	if elapsed <= delay {
		return 0
	}
	if duration <= 0 {
		return 1
	}
	return min(1, float64(elapsed-delay)/float64(duration))
}

func (m *controllerMetrics) markUDPError() {
	m.udpSendErrors.Add(1)
}

func (m *controllerMetrics) floorOutputStatus() string {
	if !m.udpTransportKnown.Load() {
		return "unknown"
	}
	if m.udpTransportAvailable.Load() {
		return "available"
	}
	return "unavailable"
}

func syncLoop(ctx context.Context, sender *udpSender, addr *net.UDPAddr, metrics *controllerMetrics) {
	sendSync(sender, addr, metrics)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendSync(sender, addr, metrics)
		}
	}
}

func sendSync(sender *udpSender, addr *net.UDPAddr, metrics *controllerMetrics) {
	packet := floor.BuildSyncPacket(0, 0, floor.DefaultChannels, []byte{255, 255, 255, 255})
	if _, err := sender.WriteToUDP(packet, addr); err != nil {
		metrics.markUDPError()
		return
	}
	log.Printf("sync broadcast sent to %s", addr)
}

func sendFrame(sender *udpSender, addr *net.UDPAddr, tiles []floor.Tile, metrics *controllerMetrics) bool {
	grid := colorGridFromTiles(tiles)
	packets := floor.BuildFrame(
		floor.DefaultControllers,
		floor.DefaultChannels,
		floor.DefaultLEDsPerChannel,
		func(controller, channel, position int) floor.RGB {
			x, y := floor.PhysicalToLogical(controller, channel, position)
			if !floor.InLogicalBounds(x, y) {
				return floor.Black
			}
			return grid[y][x]
		},
	)
	for _, packet := range packets {
		if _, err := sender.WriteToUDP(packet, addr); err != nil {
			metrics.markUDPError()
			return false
		}
	}
	return true
}

func colorGridFromTiles(tiles []floor.Tile) frameGrid {
	var grid frameGrid
	for _, tile := range tiles {
		if !floor.InLogicalBounds(tile.X, tile.Y) {
			continue
		}
		grid[tile.Y][tile.X] = floor.RGB{R: tile.R, G: tile.G, B: tile.B}
	}
	return grid
}

func tilesFromFrame(frame *recordingpb.FrameRecord, pressed [floor.GridHeight][floor.GridWidth]bool) []floor.Tile {
	tiles := make([]floor.Tile, 0, len(frame.Tiles))
	for _, tile := range frame.Tiles {
		x := int(tile.X)
		y := int(tile.Y)
		if !floor.InLogicalBounds(x, y) {
			continue
		}
		tiles = append(tiles, floor.Tile{
			X:       x,
			Y:       y,
			R:       byte(tile.R),
			G:       byte(tile.G),
			B:       byte(tile.B),
			Pressed: pressed[y][x],
		})
	}
	return tiles
}

func fadeTiles(tiles []floor.Tile, scale float64) []floor.Tile {
	if scale >= 1 {
		return tiles
	}
	if scale <= 0 {
		scale = 0
	}
	faded := make([]floor.Tile, len(tiles))
	for i, tile := range tiles {
		tile.R = byte(math.Round(float64(tile.R) * scale))
		tile.G = byte(math.Round(float64(tile.G) * scale))
		tile.B = byte(math.Round(float64(tile.B) * scale))
		faded[i] = tile
	}
	return faded
}

func (b *latestFrameBuffer) update(frame *recordingpb.FrameRecord) {
	if frame == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.frame = proto.Clone(frame).(*recordingpb.FrameRecord)
}

func (b *latestFrameBuffer) snapshot() (*recordingpb.FrameRecord, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.frame == nil {
		return nil, false
	}
	return proto.Clone(b.frame).(*recordingpb.FrameRecord), true
}

func (h *pressureStreamHub) add(client *pressureStreamClient) {
	if h == nil || client == nil {
		return
	}
	h.mu.Lock()
	h.clients[client] = true
	h.mu.Unlock()
	log.Printf("pressure subscriber connected")
}

func (h *pressureStreamHub) remove(client *pressureStreamClient) {
	if h == nil || client == nil {
		return
	}
	h.mu.Lock()
	if h.clients[client] {
		delete(h.clients, client)
		close(client.jobs)
	}
	h.mu.Unlock()
	_ = client.conn.Close()
}

func (h *pressureStreamHub) writeClient(client *pressureStreamClient) {
	defer h.remove(client)
	writer := bufio.NewWriterSize(client.conn, 64*1024)
	for event := range client.jobs {
		if err := pbstream.Write(writer, event); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

func (h *pressureStreamHub) broadcast(event pressEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	record := pressureProtoFromEvent(h.seq, time.Now(), event)
	for client := range h.clients {
		select {
		case client.jobs <- proto.Clone(record).(*inputpb.PressureEvent):
		default:
			log.Printf("pressure subscriber too slow; dropping event")
		}
	}
}

func pressureProtoFromEvent(sequence uint64, now time.Time, event pressEvent) *inputpb.PressureEvent {
	return &inputpb.PressureEvent{
		Sequence:   sequence,
		UnixNanos:  now.UnixNano(),
		X:          uint32(event.X),
		Y:          uint32(event.Y),
		Pressed:    event.Pressed,
		Source:     event.Source,
		Controller: uint32(event.Controller),
		Channel:    uint32(event.Channel),
		Position:   uint32(event.Position),
	}
}

func (s *controllerState) applySensorPacket(packet []byte) []pressEvent {
	if len(packet) < 4 {
		return nil
	}
	controller := int(packet[1])
	channels := (len(packet) - 3) / 171
	var events []pressEvent
	for channel := 0; channel < channels; channel++ {
		base := 3 + channel*171
		for position := 0; position < 170; position++ {
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

			key := sensorKey{controller: controller, channel: channel, position: position}
			s.mu.Lock()
			previous, seen := s.sensorState[key]
			if seen && previous == pressed {
				s.mu.Unlock()
				continue
			}
			s.sensorState[key] = pressed
			s.mu.Unlock()

			x, y := floor.PhysicalToLogical(controller, channel, position)
			if !floor.InLogicalBounds(x, y) {
				continue
			}
			event := pressEvent{
				Source:     "udp",
				Controller: controller,
				Channel:    channel,
				Position:   position,
				X:          x,
				Y:          y,
				Pressed:    pressed,
			}
			if s.applyPress(event) {
				events = append(events, event)
			}
		}
	}
	return events
}

func (s *controllerState) applyPress(event pressEvent) bool {
	if !floor.InLogicalBounds(event.X, event.Y) {
		return false
	}

	s.mu.Lock()
	previous := s.pressed[event.Y][event.X]
	if previous == event.Pressed {
		s.mu.Unlock()
		return false
	}
	s.pressed[event.Y][event.X] = event.Pressed
	s.mu.Unlock()

	state := "release"
	if event.Pressed {
		state = "press"
	}
	log.Printf("%s %s x=%d y=%d controller=%d channel=%d position=%d", event.Source, state, event.X, event.Y, event.Controller, event.Channel, event.Position)
	return true
}

func (s *controllerState) snapshotPressed() [floor.GridHeight][floor.GridWidth]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pressed
}

func (c config) String() string {
	return fmt.Sprintf("http=%s frames=%s input-events=%s duplex=%s refresh=%dfps udp=:%d floor-source-ip=%s broadcast=%s:%d fade=%s+%s", c.HTTPAddr, c.FrameAddr, c.InputAddr, c.DuplexAddr, c.RefreshFPS, c.RecvPort, c.FloorSourceIP, c.BroadcastIP, c.BroadcastPort, c.EngineFadeDelay, c.EngineFadeDuration)
}
