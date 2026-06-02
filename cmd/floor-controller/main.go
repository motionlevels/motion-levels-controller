package main

import (
	"bufio"
	"context"
	"embed"
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
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lobis/motion-levels/floor-controller/internal/floor"
	"github.com/lobis/motion-levels/floor-controller/internal/recording"
	"github.com/lobis/motion-levels/packages/contracts/pbstream"
	"github.com/lobis/motion-levels/packages/contracts/recordingpb"
	"google.golang.org/protobuf/proto"
)

//go:embed web
var webFS embed.FS

type config struct {
	HTTPAddr           string
	FrameAddr          string
	RecvPort           int
	BroadcastIP        string
	BroadcastPort      int
	RecordFrames       string
	RecordCompression  string
	RecordSegmentBytes int64
	RefreshFPS         int
	EngineFadeDelay    time.Duration
	EngineFadeDuration time.Duration
}

type websocketHub struct {
	mu      sync.Mutex
	clients map[*websocket.Conn]bool
	config  configMessage
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
	RecvPort      int    `json:"recvPort"`
	BroadcastAddr string `json:"broadcastAddr"`
	Recording     bool   `json:"recording"`
	Compression   string `json:"compression"`
}

type statusMessage struct {
	Type              string          `json:"type"`
	UptimeSeconds     int64           `json:"uptimeSeconds"`
	PresentedFrames   uint64          `json:"presentedFrames"`
	ActualFPS         float64         `json:"actualFps"`
	WebsocketClients  int             `json:"websocketClients"`
	UDPErrorCount     uint64          `json:"udpErrorCount"`
	GameFrameAgeMS    int64           `json:"gameFrameAgeMs"`
	GameFrameSequence uint64          `json:"gameFrameSequence"`
	GameEngineOnline  bool            `json:"gameEngineOnline"`
	EngineFadeAmount  float64         `json:"engineFadeAmount"`
	Recording         recording.Stats `json:"recording"`
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

func parseConfig() config {
	cfg := config{}
	flag.StringVar(&cfg.HTTPAddr, "http", ":8080", "HTTP address for websocket preview")
	flag.StringVar(&cfg.FrameAddr, "frames", ":9090", "TCP address for length-prefixed protobuf frame stream")
	flag.IntVar(&cfg.RecvPort, "recv-port", 7800, "UDP port for tile handshake/sensor packets")
	flag.StringVar(&cfg.BroadcastIP, "broadcast-ip", "255.255.255.255", "UDP broadcast IP for LED packets")
	flag.IntVar(&cfg.BroadcastPort, "broadcast-port", 4626, "UDP broadcast port for LED packets")
	flag.StringVar(&cfg.RecordFrames, "record-frames", "recordings", "recording directory or .pbstream file; empty disables recording")
	flag.StringVar(&cfg.RecordCompression, "record-compression", "gzip", "recording compression: gzip or none")
	flag.Int64Var(&cfg.RecordSegmentBytes, "record-segment-bytes", recording.DefaultMaxSegmentBytes, "maximum recording segment size in bytes before rotating")
	flag.IntVar(&cfg.RefreshFPS, "refresh-fps", 30, "floor-controller output refresh rate for UDP, websocket, and recording")
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
	if !recording.SupportedCompression(c.RecordCompression) {
		errs = append(errs, fmt.Errorf("record-compression must be gzip or none"))
	}
	if c.RecordSegmentBytes < 1 {
		errs = append(errs, fmt.Errorf("record-segment-bytes must be at least 1"))
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

	broadcastAddr := &net.UDPAddr{IP: net.ParseIP(cfg.BroadcastIP), Port: cfg.BroadcastPort}
	metrics := newControllerMetrics()
	hub := &websocketHub{clients: make(map[*websocket.Conn]bool), config: cfg.configMessage()}
	state := &controllerState{sensorState: make(map[sensorKey]bool)}
	frames := &latestFrameBuffer{}
	frameRecorder, err := recording.NewFrameRecorderWithOptions(cfg.RecordFrames, recording.Options{
		Compression:     cfg.RecordCompression,
		MaxSegmentBytes: cfg.RecordSegmentBytes,
	})
	if err != nil {
		return err
	}
	defer frameRecorder.Close()
	if cfg.RecordFrames != "" {
		log.Printf("frame recording: %s", frameRecorder.Path())
	}
	log.Printf("config: %s recording=%s", cfg, frameRecorder.Path())

	go metricsLoop(ctx, metrics)
	go statusLoop(ctx, cfg, hub, metrics, frameRecorder)
	go readUDP(ctx, conn, state, hub)
	go serveHTTP(ctx, cfg, hub, state, metrics, frameRecorder)
	go syncLoop(ctx, conn, broadcastAddr, metrics)
	go presentationLoop(ctx, cfg, conn, broadcastAddr, hub, state, frames, frameRecorder, metrics)
	return listenFrameStream(ctx, cfg.FrameAddr, func(record *recordingpb.FrameRecord) {
		metrics.markGameFrame(record)
		frames.update(record)
	}, metrics.markGameEngineConnected, metrics.markGameEngineDisconnected)
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

func readUDP(ctx context.Context, conn *net.UDPConn, state *controllerState, hub *websocketHub) {
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
			}
		default:
			log.Printf("unknown UDP packet 0x%02x from %s (%d bytes)", packet[0], addr, n)
		}
	}
}

func serveHTTP(ctx context.Context, cfg config, hub *websocketHub, state *controllerState, metrics *controllerMetrics, recorder *recording.FrameRecorder) {
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
	mux.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(index)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(snapshotStatus(metrics, cfg, hub, recorder)); err != nil {
			log.Printf("status response: %v", err)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/live", http.StatusTemporaryRedirect)
	})

	for _, url := range previewURLs(cfg.HTTPAddr, "/live") {
		log.Printf("preview: %s", url)
	}
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}
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

func listenFrameStream(ctx context.Context, addr string, onFrame func(*recordingpb.FrameRecord), onConnect, onDisconnect func()) error {
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

	gate := &gameEngineStreamGate{}
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

func handleFrameConnection(conn net.Conn, onFrame func(*recordingpb.FrameRecord), onConnect, onDisconnect func()) {
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
		onFrame(&frame)
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

func presentationLoop(ctx context.Context, cfg config, conn *net.UDPConn, addr *net.UDPAddr, hub *websocketHub, state *controllerState, frames *latestFrameBuffer, recorder *recording.FrameRecorder, metrics *controllerMetrics) {
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
			if fade := metrics.engineFadeAmount(now, cfg.EngineFadeDelay, cfg.EngineFadeDuration); fade > 0 {
				tiles = fadeTiles(tiles, 1-fade)
			}
			if err := recorder.RecordFrame(sequence, now.UnixNano(), frame.Width, frame.Height, tiles); err != nil {
				log.Printf("record frame: %v", err)
			}
			sendFrame(conn, addr, tiles, metrics)
			metrics.markPresented(sequence, now)
			hub.broadcastBinary(buildViewerFrame(sequence, frame.Width, frame.Height, tiles))
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

func statusLoop(ctx context.Context, cfg config, hub *websocketHub, metrics *controllerMetrics, recorder *recording.FrameRecorder) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hub.broadcastJSON(snapshotStatus(metrics, cfg, hub, recorder))
		}
	}
}

func snapshotStatus(metrics *controllerMetrics, cfg config, hub *websocketHub, recorder *recording.FrameRecorder) statusMessage {
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
		WebsocketClients:  hub.clientCount(),
		UDPErrorCount:     metrics.udpSendErrors.Load(),
		GameFrameAgeMS:    gameFrameAgeMS,
		GameFrameSequence: metrics.lastGameFrameSequence.Load(),
		GameEngineOnline:  metrics.gameEngineOnline(),
		EngineFadeAmount:  metrics.engineFadeAmount(now, cfg.EngineFadeDelay, cfg.EngineFadeDuration),
		Recording:         recorder.Stats(),
	}
}

func (m *controllerMetrics) markPresented(sequence uint64, now time.Time) {
	m.presentedFrames.Store(sequence)
	m.presentedFramesWindow.Add(1)
	m.lastPresentedUnixNanos.Store(now.UnixNano())
}

func (m *controllerMetrics) markGameFrame(frame *recordingpb.FrameRecord) {
	if frame == nil {
		return
	}
	m.lastGameFrameSequence.Store(frame.Sequence)
	m.lastGameFrameUnixNanos.Store(frame.UnixNanos)
	m.lastGameFrameReceived.Store(time.Now().UnixNano())
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

func syncLoop(ctx context.Context, conn *net.UDPConn, addr *net.UDPAddr, metrics *controllerMetrics) {
	sendSync(conn, addr, metrics)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendSync(conn, addr, metrics)
		}
	}
}

func sendSync(conn *net.UDPConn, addr *net.UDPAddr, metrics *controllerMetrics) {
	packet := floor.BuildSyncPacket(0, 0, floor.DefaultChannels, []byte{255, 255, 255, 255})
	if _, err := conn.WriteToUDP(packet, addr); err != nil {
		metrics.markUDPError()
		log.Printf("send sync: %v", err)
		return
	}
	log.Printf("sync broadcast sent to %s", addr)
}

func sendFrame(conn *net.UDPConn, addr *net.UDPAddr, tiles []floor.Tile, metrics *controllerMetrics) {
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
		if _, err := conn.WriteToUDP(packet, addr); err != nil {
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
		RecvPort:      c.RecvPort,
		BroadcastAddr: fmt.Sprintf("%s:%d", c.BroadcastIP, c.BroadcastPort),
		Recording:     c.RecordFrames != "",
		Compression:   recording.NormalizeCompression(c.RecordCompression),
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
	if err := conn.WriteJSON(h.config); err != nil {
		_ = conn.Close()
		return
	}
	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()
	go h.readClient(conn, state)
}

func (h *websocketHub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *websocketHub) readClient(conn *websocket.Conn, state *controllerState) {
	defer func() {
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		_ = conn.Close()
	}()

	for {
		var message inputMessage
		if err := conn.ReadJSON(&message); err != nil {
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

	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		if err := client.WriteMessage(websocket.TextMessage, data); err != nil {
			_ = client.Close()
			delete(h.clients, client)
		}
	}
}

func (h *websocketHub) broadcastBinary(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		if err := client.WriteMessage(websocket.BinaryMessage, data); err != nil {
			_ = client.Close()
			delete(h.clients, client)
		}
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
	return fmt.Sprintf("http=%s frames=%s refresh=%dfps udp=:%d broadcast=%s:%d record-compression=%s record-segment-bytes=%d fade=%s+%s", c.HTTPAddr, c.FrameAddr, c.RefreshFPS, c.RecvPort, c.BroadcastIP, c.BroadcastPort, recording.NormalizeCompression(c.RecordCompression), c.RecordSegmentBytes, c.EngineFadeDelay, c.EngineFadeDuration)
}
