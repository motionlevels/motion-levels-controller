package adapter

import (
	"bytes"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestSystemdNotifier(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "notify.sock")

	serverAddr, err := net.ResolveUnixAddr("unixgram", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	serverConn, err := net.ListenUnixgram("unixgram", serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer serverConn.Close()

	t.Setenv("NOTIFY_SOCKET", sockPath)

	notifier := newSystemdNotifier()
	if notifier == nil {
		t.Fatal("expected notifier to be created with valid NOTIFY_SOCKET")
	}
	defer notifier.close()

	notifier.ready()
	notifier.watchdog()
	notifier.stopping()

	_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1024)
	var received bytes.Buffer
	for received.Len() < len("READY=1\nWATCHDOG=1\nSTOPPING=1\n") {
		n, err := serverConn.Read(buf)
		if err != nil {
			t.Fatalf("read from notify socket: %v", err)
		}
		received.Write(buf[:n])
	}

	expected := "READY=1\nWATCHDOG=1\nSTOPPING=1\n"
	if received.String() != expected {
		t.Fatalf("received %q, want %q", received.String(), expected)
	}
}

func TestSystemdNotifierNilSafe(t *testing.T) {
	var notifier *systemdNotifier
	notifier.ready()
	notifier.watchdog()
	notifier.stopping()
	notifier.close()

	t.Setenv("NOTIFY_SOCKET", "")
	unconfigured := newSystemdNotifier()
	if unconfigured != nil {
		t.Fatal("expected nil notifier when NOTIFY_SOCKET is empty")
	}
}
