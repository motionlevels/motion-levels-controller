package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/motionlevels/motion-levels-controller/contracts/inputpb"
	"github.com/motionlevels/motion-levels-controller/contracts/pbstream"
	"github.com/motionlevels/motion-levels-controller/contracts/recordingpb"
	"github.com/motionlevels/motion-levels-controller/internal/controllerid"
	"github.com/motionlevels/motion-levels-controller/internal/floor"
	"golang.org/x/net/ipv4"
	"google.golang.org/protobuf/proto"
)

//go:embed web
var webFS embed.FS

type config struct {
	HTTPAddr            string
	FrameAddr           string
	InputAddr           string
	DuplexAddr          string
	RecvPort            int
	FloorSourceIP       string
	BroadcastIP         string
	BroadcastPort       int
	ControllerID        string
	ControllerIDFile    string
	LivePushPlatformURL string
	LivePushToken       string
	LivePushFPS         int
	LivePushTimeout     time.Duration
	RefreshFPS          int
	EngineFadeDelay     time.Duration
	EngineFadeDuration  time.Duration
}

type websocketHub struct {
	mu              sync.Mutex
	clients         map[*websocket.Conn]*websocketClient
	config          configMessage
	pressureStreams *pressureStreamHub
	duplex          *duplexHub
}

type websocketClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
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

type liveFloorPushJob struct {
	ControllerID       string `json:"controllerId"`
	SessionID          string `json:"sessionId,omitempty"`
	Sequence           uint64 `json:"sequence"`
	Width              uint32 `json:"width"`
	Height             uint32 `json:"height"`
	PresentedUnixNanos int64  `json:"presentedUnixNanos"`
	FrameBase64        string `json:"frameBase64"`
}

type liveFramePusher struct {
	endpoint     string
	token        string
	interval     time.Duration
	client       *http.Client
	jobs         chan liveFloorPushJob
	lastEnqueued time.Time
	lastErrorLog time.Time
}

type inputMessage struct {
	Type    string `json:"type"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Pressed bool   `json:"pressed"`
}

type configMessage struct {
	Type          string `json:"type"`
	RefreshFPS    int    `json:"refreshFps"`
	GridWidth     int    `json:"gridWidth"`
	GridHeight    int    `json:"gridHeight"`
	FrameAddr     string `json:"frameAddr"`
	InputAddr     string `json:"inputAddr"`
	RecvPort      int    `json:"recvPort"`
	BroadcastAddr string `json:"broadcastAddr"`
	FloorSourceIP string `json:"floorSourceIp,omitempty"`
	ControllerID  string `json:"controllerId"`
}

type statusMessage struct {
	Type              string     `json:"type"`
	UptimeSeconds     int64      `json:"uptimeSeconds"`
	PresentedFrames   uint64     `json:"presentedFrames"`
	ActualFPS         float64    `json:"actualFps"`
	RefreshFPS        int        `json:"refreshFps"`
	WebsocketClients  int        `json:"websocketClients"`
	UDPErrorCount     uint64     `json:"udpErrorCount"`
	GameFrameAgeMS    int64      `json:"gameFrameAgeMs"`
	GameFrameSequence uint64     `json:"gameFrameSequence"`
	GameEngineOnline  bool       `json:"gameEngineOnline"`
	EngineFadeAmount  float64    `json:"engineFadeAmount"`
	ControllerID      string     `json:"controllerId"`
	Sync              syncStatus `json:"sync"`
}

type syncStatus struct {
	Status                string  `json:"status"`
	SessionID             string  `json:"sessionId,omitempty"`
	Samples               uint64  `json:"samples"`
	EngineClockOffsetMS   float64 `json:"engineClockOffsetMs"`
	PresentLatencyMS      float64 `json:"presentLatencyMs"`
	JitterMS              float64 `json:"jitterMs"`
	LastGameFrameSequence uint64  `json:"lastGameFrameSequence"`
}

type pressureMessage struct {
	Type    string `json:"type"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Pressed bool   `json:"pressed"`
	Source  string `json:"source"`
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

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseConfig() config {
	cfg := config{}
	livePushPlatformURL := os.Getenv("MOTION_LEVELS_LIVE_PUSH_PLATFORM_URL")
	if strings.TrimSpace(livePushPlatformURL) == "" {
		livePushPlatformURL = os.Getenv("MOTION_LEVELS_PLATFORM_URL")
	}
	livePushToken := os.Getenv("MOTION_LEVELS_LIVE_PUSH_TOKEN")
	if strings.TrimSpace(livePushToken) == "" {
		livePushToken = os.Getenv("MOTION_LEVELS_PLATFORM_TOKEN")
	}
	flag.StringVar(&cfg.HTTPAddr, "http", "127.0.0.1:4101", "HTTP address for websocket preview")
	flag.StringVar(&cfg.FrameAddr, "frames", "127.0.0.1:4201", "TCP address for length-prefixed protobuf frame stream")
	flag.StringVar(&cfg.InputAddr, "input-events", "127.0.0.1:4202", "TCP address for pressure event subscribers; empty disables")
	flag.StringVar(&cfg.DuplexAddr, "duplex", "127.0.0.1:4203", "TCP address for the protocol-v2 duplex floor stream; empty disables")
	flag.IntVar(&cfg.RecvPort, "recv-port", 7800, "UDP port for tile handshake/sensor packets")
	flag.StringVar(&cfg.FloorSourceIP, "floor-source-ip", os.Getenv("MOTION_LEVELS_FLOOR_SOURCE_IP"), "local IPv4 source address for floor UDP output; empty uses the default route")
	flag.StringVar(&cfg.BroadcastIP, "broadcast-ip", "255.255.255.255", "UDP broadcast IP for LED packets")
	flag.IntVar(&cfg.BroadcastPort, "broadcast-port", 4626, "UDP broadcast port for LED packets")
	flag.StringVar(&cfg.ControllerID, "controller-id", "", "stable controller UUID override; normally leave empty and use controller-id-file")
	flag.StringVar(&cfg.ControllerIDFile, "controller-id-file", controllerid.DefaultPath, "file used to persist this controller's generated UUID")
	flag.StringVar(&cfg.LivePushPlatformURL, "live-push-platform-url", livePushPlatformURL, "platform base URL for outbound live floor preview frames; empty disables")
	flag.StringVar(&cfg.LivePushToken, "live-push-token", livePushToken, "platform bearer token for live floor preview ingest; can also use MOTION_LEVELS_LIVE_PUSH_TOKEN or MOTION_LEVELS_PLATFORM_TOKEN")
	flag.IntVar(&cfg.LivePushFPS, "live-push-fps", envInt("MOTION_LEVELS_LIVE_PUSH_FPS", 5), "maximum outbound live floor preview push rate; 0 disables")
	flag.DurationVar(&cfg.LivePushTimeout, "live-push-timeout", envDuration("MOTION_LEVELS_LIVE_PUSH_TIMEOUT", 2*time.Second), "HTTP timeout for live floor preview pushes")
	flag.IntVar(&cfg.RefreshFPS, "refresh-fps", 50, "floor-controller output refresh rate for UDP and websocket preview")
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
	if c.LivePushFPS < 0 {
		errs = append(errs, fmt.Errorf("live-push-fps must be zero or greater"))
	}
	if c.LivePushTimeout <= 0 {
		errs = append(errs, fmt.Errorf("live-push-timeout must be positive"))
	}
	if strings.TrimSpace(c.LivePushPlatformURL) != "" {
		parsed, err := url.Parse(c.LivePushPlatformURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("live-push-platform-url must be an absolute URL"))
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
	resolvedControllerID, err := controllerid.Resolve(cfg.ControllerIDFile, cfg.ControllerID)
	if err != nil {
		return err
	}
	cfg.ControllerID = resolvedControllerID

	conn, err := openUDP(cfg.RecvPort)
	if err != nil {
		return err
	}
	defer conn.Close()
	sender, err := newUDPSender(conn, cfg.FloorSourceIP)
	if err != nil {
		return err
	}

	broadcastAddr := &net.UDPAddr{IP: net.ParseIP(cfg.BroadcastIP), Port: cfg.BroadcastPort}
	metrics := newControllerMetrics()
	pressureStreams := &pressureStreamHub{clients: make(map[*pressureStreamClient]bool)}
	duplex := newDuplexHub(cfg)
	hub := &websocketHub{
		clients:         make(map[*websocket.Conn]*websocketClient),
		config:          cfg.configMessage(),
		pressureStreams: pressureStreams,
		duplex:          duplex,
	}
	state := &controllerState{sensorState: make(map[sensorKey]bool)}
	frames := &latestFrameBuffer{}
	livePusher := newLiveFramePusher(cfg)
	if livePusher != nil {
		go livePusher.run(ctx)
		log.Printf("live floor push: %s at up to %d fps", livePusher.endpoint, cfg.LivePushFPS)
	}
	log.Printf("controller id: %s", cfg.ControllerID)
	log.Printf("config: %s", cfg)

	go metricsLoop(ctx, metrics)
	go statusLoop(ctx, cfg, hub, metrics, duplex)
	go readUDP(ctx, conn, state, hub, pressureStreams, duplex)
	go serveHTTP(ctx, cfg, hub, state, metrics)
	go listenPressureSubscribers(ctx, cfg.InputAddr, pressureStreams)
	go syncLoop(ctx, sender, broadcastAddr, metrics)
	go presentationLoop(ctx, cfg, sender, broadcastAddr, hub, state, frames, metrics, livePusher, duplex)
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
	conn    *net.UDPConn
	packet  *ipv4.PacketConn
	control *ipv4.ControlMessage
}

func newUDPSender(conn *net.UDPConn, sourceIPValue string) (*udpSender, error) {
	sourceIPValue = strings.TrimSpace(sourceIPValue)
	if sourceIPValue == "" {
		return &udpSender{conn: conn}, nil
	}

	sourceIP := net.ParseIP(sourceIPValue)
	if sourceIP == nil || sourceIP.To4() == nil {
		return nil, fmt.Errorf("floor UDP source %q is not a valid IPv4 address", sourceIPValue)
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces for floor UDP source: %w", err)
	}
	for _, networkInterface := range interfaces {
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			addressIP, _, err := net.ParseCIDR(address.String())
			if err == nil && addressIP.Equal(sourceIP) {
				return &udpSender{
					conn:   conn,
					packet: ipv4.NewPacketConn(conn),
					control: &ipv4.ControlMessage{
						IfIndex: networkInterface.Index,
						Src:     sourceIP.To4(),
					},
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("floor UDP source %s is not assigned to a local interface", sourceIPValue)
}

func (s *udpSender) WriteToUDP(packet []byte, addr *net.UDPAddr) (int, error) {
	if s.control == nil {
		return s.conn.WriteToUDP(packet, addr)
	}
	return s.packet.WriteTo(packet, s.control, addr)
}

func readUDP(ctx context.Context, conn *net.UDPConn, state *controllerState, hub *websocketHub, pressureStreams *pressureStreamHub, duplex *duplexHub) {
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
				hub.broadcastPressure(event)
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

func newHTTPHandler(cfg config, hub *websocketHub, state *controllerState, metrics *controllerMetrics) http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hub.add(conn, state)
	})
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
		if _, err := w.Write([]byte(`{"status":"ok"}` + "\n")); err != nil {
			log.Printf("health response: %v", err)
		}
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(snapshotStatus(metrics, cfg, hub)); err != nil {
			log.Printf("status response: %v", err)
		}
	})
	mux.HandleFunc("/metrics", controllerMetricsHandler(cfg, hub, metrics))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(index)
	})
	return mux
}

func serveHTTP(ctx context.Context, cfg config, hub *websocketHub, state *controllerState, metrics *controllerMetrics) {
	for _, url := range previewURLs(cfg.HTTPAddr, "/") {
		log.Printf("preview: %s", url)
	}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: newHTTPHandler(cfg, hub, state, metrics)}
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

func presentationLoop(ctx context.Context, cfg config, sender *udpSender, addr *net.UDPAddr, hub *websocketHub, state *controllerState, frames *latestFrameBuffer, metrics *controllerMetrics, livePusher *liveFramePusher, duplex *duplexHub) {
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
			sequence++
			tiles := tilesFromFrame(frame, state.snapshotPressed())
			fade := metrics.engineFadeAmount(now, cfg.EngineFadeDelay, cfg.EngineFadeDuration)
			if fade > 0 {
				tiles = fadeTiles(tiles, 1-fade)
			}
			sendFrame(sender, addr, tiles, metrics)
			metrics.markPresented(sequence, now, frame)
			viewerFrame := buildViewerFrame(sequence, frame.Width, frame.Height, tiles)
			hub.broadcastBinary(viewerFrame)
			duplex.broadcastPresented(sequence, frame.Sequence, now, frame.Width, frame.Height, tiles, fade)
			// During the compatibility window the controller keeps publishing
			// for v1 engines. A v2 engine receives the observed frame and owns
			// the only platform publication, avoiding duplicate writers.
			if livePusher != nil && !duplex.active() && livePusher.shouldEnqueue(now) {
				livePusher.enqueue(liveFloorPushJob{
					ControllerID:       cfg.ControllerID,
					SessionID:          frame.SessionId,
					Sequence:           sequence,
					Width:              frame.Width,
					Height:             frame.Height,
					PresentedUnixNanos: now.UnixNano(),
					FrameBase64:        base64.StdEncoding.EncodeToString(viewerFrame),
				})
			}
		}
	}
}

func (p *liveFramePusher) shouldEnqueue(now time.Time) bool {
	if p == nil || now.Sub(p.lastEnqueued) < p.interval {
		return false
	}
	p.lastEnqueued = now
	return true
}

func newLiveFramePusher(cfg config) *liveFramePusher {
	platformURL := strings.TrimSpace(cfg.LivePushPlatformURL)
	if platformURL == "" || cfg.LivePushFPS == 0 {
		return nil
	}
	return &liveFramePusher{
		endpoint: strings.TrimRight(platformURL, "/") + "/api/live-floor/ingest",
		token:    strings.TrimSpace(cfg.LivePushToken),
		interval: time.Second / time.Duration(cfg.LivePushFPS),
		client:   &http.Client{Timeout: cfg.LivePushTimeout},
		jobs:     make(chan liveFloorPushJob, 1),
	}
}

func (p *liveFramePusher) enqueue(job liveFloorPushJob) {
	if p == nil {
		return
	}

	select {
	case p.jobs <- job:
		return
	default:
	}

	select {
	case <-p.jobs:
	default:
	}

	select {
	case p.jobs <- job:
	default:
	}
}

func (p *liveFramePusher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-p.jobs:
			if err := p.post(ctx, job); err != nil {
				now := time.Now()
				if now.Sub(p.lastErrorLog) >= 10*time.Second {
					log.Printf("live floor push: %v", err)
					p.lastErrorLog = now
				}
			}
		}
	}
}

func (p *liveFramePusher) post(ctx context.Context, job liveFloorPushJob) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}

	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("platform returned %s", response.Status)
	}
	return nil
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

func statusLoop(ctx context.Context, cfg config, hub *websocketHub, metrics *controllerMetrics, duplex *duplexHub) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status := snapshotStatus(metrics, cfg, hub)
			hub.broadcastJSON(status)
			duplex.broadcastStatus(status)
		}
	}
}

func snapshotStatus(metrics *controllerMetrics, cfg config, hub *websocketHub) statusMessage {
	now := time.Now()
	gameFrameAgeMS := int64(-1)
	if received := metrics.lastGameFrameReceived.Load(); received > 0 {
		gameFrameAgeMS = now.Sub(time.Unix(0, received)).Milliseconds()
	}
	return statusMessage{
		Type:              "status",
		UptimeSeconds:     int64(now.Sub(metrics.startedAt).Seconds()),
		PresentedFrames:   metrics.presentedFrames.Load(),
		ActualFPS:         math.Float64frombits(metrics.actualFPSBits.Load()),
		RefreshFPS:        cfg.RefreshFPS,
		WebsocketClients:  hub.clientCount(),
		UDPErrorCount:     metrics.udpSendErrors.Load(),
		GameFrameAgeMS:    gameFrameAgeMS,
		GameFrameSequence: metrics.lastGameFrameSequence.Load(),
		GameEngineOnline:  metrics.gameEngineOnline(),
		EngineFadeAmount:  metrics.engineFadeAmount(now, cfg.EngineFadeDelay, cfg.EngineFadeDuration),
		ControllerID:      cfg.ControllerID,
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
		log.Printf("send sync: %v", err)
		return
	}
	log.Printf("sync broadcast sent to %s", addr)
}

func sendFrame(sender *udpSender, addr *net.UDPAddr, tiles []floor.Tile, metrics *controllerMetrics) {
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
			log.Printf("send frame: %v", err)
			return
		}
	}
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

func buildViewerFrame(sequence uint64, width, height uint32, tiles []floor.Tile) []byte {
	const headerLen = 16
	tileCount := int(width * height)
	rgbOffset := headerLen
	pressureOffset := rgbOffset + tileCount*3
	data := make([]byte, pressureOffset+(tileCount+7)/8)

	copy(data[0:4], []byte{'M', 'L', 'F', '1'})
	binary.LittleEndian.PutUint32(data[4:8], uint32(sequence))
	binary.LittleEndian.PutUint16(data[8:10], uint16(width))
	binary.LittleEndian.PutUint16(data[10:12], uint16(height))
	data[12] = 1
	data[13] = 0
	binary.LittleEndian.PutUint16(data[14:16], headerLen)

	for _, tile := range tiles {
		if tile.X < 0 || tile.Y < 0 || tile.X >= int(width) || tile.Y >= int(height) {
			continue
		}
		index := tile.Y*int(width) + tile.X
		rgbIndex := rgbOffset + index*3
		data[rgbIndex] = tile.R
		data[rgbIndex+1] = tile.G
		data[rgbIndex+2] = tile.B
		if tile.Pressed {
			data[pressureOffset+index/8] |= 1 << uint(index%8)
		}
	}

	return data
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

func (c config) configMessage() configMessage {
	return configMessage{
		Type:          "config",
		RefreshFPS:    c.RefreshFPS,
		GridWidth:     floor.GridWidth,
		GridHeight:    floor.GridHeight,
		FrameAddr:     c.FrameAddr,
		InputAddr:     c.InputAddr,
		RecvPort:      c.RecvPort,
		BroadcastAddr: fmt.Sprintf("%s:%d", c.BroadcastIP, c.BroadcastPort),
		FloorSourceIP: c.FloorSourceIP,
		ControllerID:  c.ControllerID,
	}
}

func previewURLs(addr, path string) []string {
	port := portFromAddr(addr)
	urls := []string{"http://127.0.0.1" + port + path}
	for _, ip := range localIPv4s() {
		urls = append(urls, "http://"+ip+port+path)
	}
	return urls
}

func portFromAddr(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err == nil && port != "" {
		return ":" + port
	}
	if len(addr) > 0 && addr[0] == ':' {
		return addr
	}
	return ""
}

func localIPv4s() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

func (h *websocketHub) add(conn *websocket.Conn, state *controllerState) {
	log.Printf("websocket connected")
	client := &websocketClient{conn: conn}
	if err := client.writeJSON(h.config); err != nil {
		_ = conn.Close()
		return
	}
	h.mu.Lock()
	h.clients[conn] = client
	h.mu.Unlock()
	go h.readClient(client, state)
}

func (h *websocketHub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *websocketHub) readClient(client *websocketClient, state *controllerState) {
	defer func() {
		h.removeClient(client)
	}()

	for {
		var message inputMessage
		if err := client.conn.ReadJSON(&message); err != nil {
			return
		}
		if message.Type != "press" || !floor.InLogicalBounds(message.X, message.Y) {
			continue
		}
		physical := floor.LogicalToPhysical(message.X, message.Y)
		event := pressEvent{
			Source:     "web",
			Controller: physical.Controller,
			Channel:    physical.Channel,
			Position:   physical.Position,
			X:          message.X,
			Y:          message.Y,
			Pressed:    message.Pressed,
		}
		if state.applyPress(event) {
			h.broadcastPressure(event)
			if h.pressureStreams != nil {
				h.pressureStreams.broadcast(event)
			}
			if h.duplex != nil {
				h.duplex.broadcastPressure(event)
			}
		}
	}
}

func (h *websocketHub) broadcastPressure(event pressEvent) {
	h.broadcastJSON(pressureMessage{
		Type:    "pressure",
		X:       event.X,
		Y:       event.Y,
		Pressed: event.Pressed,
		Source:  event.Source,
	})
}

func (h *websocketHub) broadcastJSON(message any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}

	h.broadcast(websocket.TextMessage, data)
}

func (h *websocketHub) broadcastBinary(data []byte) {
	h.broadcast(websocket.BinaryMessage, data)
}

func (h *websocketHub) broadcast(messageType int, data []byte) {
	for _, client := range h.snapshotClients() {
		if err := client.writeMessage(messageType, data); err != nil {
			h.removeClient(client)
		}
	}
}

func (h *websocketHub) snapshotClients() []*websocketClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	clients := make([]*websocketClient, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	return clients
}

func (h *websocketHub) removeClient(client *websocketClient) {
	if client == nil || client.conn == nil {
		return
	}
	h.mu.Lock()
	if h.clients[client.conn] == client {
		delete(h.clients, client.conn)
	}
	h.mu.Unlock()
	_ = client.conn.Close()
}

func (c *websocketClient) writeJSON(message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return c.writeMessage(websocket.TextMessage, data)
}

func (c *websocketClient) writeMessage(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	err := c.conn.WriteMessage(messageType, data)
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
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
	livePush := "off"
	if strings.TrimSpace(c.LivePushPlatformURL) != "" && c.LivePushFPS > 0 {
		livePush = fmt.Sprintf("%s@%dfps", c.LivePushPlatformURL, c.LivePushFPS)
	}
	return fmt.Sprintf("controller-id=%s http=%s frames=%s input-events=%s duplex=%s refresh=%dfps udp=:%d floor-source-ip=%s broadcast=%s:%d live-push=%s fade=%s+%s", c.ControllerID, c.HTTPAddr, c.FrameAddr, c.InputAddr, c.DuplexAddr, c.RefreshFPS, c.RecvPort, c.FloorSourceIP, c.BroadcastIP, c.BroadcastPort, livePush, c.EngineFadeDelay, c.EngineFadeDuration)
}
