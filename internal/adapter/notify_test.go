package adapter

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func shortTempSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "notify-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return filepath.Join(dir, "n.sock")
}

func TestSystemdNotifier(t *testing.T) {
	socketPath := shortTempSocketPath(t)
	serverAddr, err := net.ResolveUnixAddr("unixgram", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server, err := net.ListenUnixgram("unixgram", serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	t.Setenv("NOTIFY_SOCKET", socketPath)
	t.Setenv("WATCHDOG_USEC", "10000000")
	notifier, err := newSystemdNotifier()
	if err != nil {
		t.Fatal(err)
	}
	if notifier == nil {
		t.Fatal("expected notifier")
	}
	defer notifier.close()
	if notifier.watchdogInterval != 5*time.Second {
		t.Fatalf("watchdog interval=%s, want 5s", notifier.watchdogInterval)
	}
	notifier.ready()
	notifier.watchdog()
	notifier.stopping()

	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	var received bytes.Buffer
	buffer := make([]byte, 128)
	for received.Len() < len("READY=1\nWATCHDOG=1\nSTOPPING=1\n") {
		count, err := server.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		received.Write(buffer[:count])
	}
	if got, want := received.String(), "READY=1\nWATCHDOG=1\nSTOPPING=1\n"; got != want {
		t.Fatalf("received %q, want %q", got, want)
	}
}

func TestSystemdNotifierNilSafe(t *testing.T) {
	var notifier *systemdNotifier
	notifier.ready()
	notifier.watchdog()
	notifier.stopping()
	notifier.close()
	t.Setenv("NOTIFY_SOCKET", "")
	got, err := newSystemdNotifier()
	if err != nil || got != nil {
		t.Fatalf("notifier=%v err=%v, want nil nil", got, err)
	}
}

func TestSystemdNotifierSupportsLinuxAbstractSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux abstract Unix sockets are platform-specific")
	}
	name := fmt.Sprintf("@motion-levels-notify-%d-%d", os.Getpid(), time.Now().UnixNano())
	address, err := net.ResolveUnixAddr("unixgram", name)
	if err != nil {
		t.Fatal(err)
	}
	server, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	t.Setenv("NOTIFY_SOCKET", name)
	notifier, err := newSystemdNotifier()
	if err != nil {
		t.Fatal(err)
	}
	defer notifier.close()
	notifier.ready()

	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 32)
	count, err := server.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:count]); got != "READY=1\n" {
		t.Fatalf("notification=%q, want READY=1", got)
	}
}

func TestSystemdNotifierHonorsShortWatchdogDeadline(t *testing.T) {
	socketPath := shortTempSocketPath(t)
	address, err := net.ResolveUnixAddr("unixgram", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	t.Setenv("NOTIFY_SOCKET", socketPath)
	t.Setenv("WATCHDOG_USEC", "100000") // 100 ms deadline

	notifier, err := newSystemdNotifier()
	if err != nil {
		t.Fatal(err)
	}
	defer notifier.close()
	if got, want := notifier.watchdogInterval, 50*time.Millisecond; got != want {
		t.Fatalf("watchdog interval=%s, want %s", got, want)
	}
}
