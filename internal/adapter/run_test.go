package adapter

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func freeUDPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	address := listener.LocalAddr().String()
	_ = listener.Close()
	return address
}

func TestRunStartsWithoutPhysicalFloorAndStopsCleanly(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	cfg := DefaultConfig()
	cfg.HTTPAddr = freeTCPAddress(t)
	cfg.EngineAddr = freeTCPAddress(t)
	cfg.ReceiveAddr = freeUDPAddress(t)
	cfg.FloorSourceIP = "192.0.2.55"
	cfg.BroadcastIP = "127.0.0.1"
	cfg.RefreshFPS = 100
	cfg.SourceRetry = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case err := <-done:
		t.Fatalf("adapter exited while floor source was absent: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter did not stop cleanly")
	}
}

func TestRunSignalsReadyOnlyAfterListenersStart(t *testing.T) {
	socketPath := shortTempSocketPath(t)
	address, err := net.ResolveUnixAddr("unixgram", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	notifySocket, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		t.Fatal(err)
	}
	defer notifySocket.Close()
	t.Setenv("NOTIFY_SOCKET", socketPath)
	t.Setenv("WATCHDOG_USEC", "")

	cfg := DefaultConfig()
	cfg.HTTPAddr = freeTCPAddress(t)
	cfg.EngineAddr = freeTCPAddress(t)
	cfg.ReceiveAddr = freeUDPAddress(t)
	cfg.BroadcastIP = "127.0.0.1"
	cfg.RefreshFPS = 100

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	_ = notifySocket.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 128)
	count, err := notifySocket.Read(buffer)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !strings.Contains(string(buffer[:count]), "READY=1") {
		cancel()
		t.Fatalf("first notification=%q, want READY=1", buffer[:count])
	}
	for _, address := range []string{cfg.HTTPAddr, cfg.EngineAddr} {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			cancel()
			t.Fatalf("listener %s was unavailable after READY: %v", address, err)
		}
		_ = conn.Close()
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error=%v", err)
	}
}

func TestRunFailsFastForBrokenNotifySocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", filepath.Join(shortTempSocketPath(t), "missing.sock"))
	cfg := DefaultConfig()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Run(ctx, cfg); err == nil || !strings.Contains(err.Error(), "systemd notify socket") {
		t.Fatalf("Run error=%v, want notify socket error", err)
	}
}
