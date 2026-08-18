package adapter

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestUDPOutputPinsConfiguredLoopbackSource(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	cfg := DefaultConfig()
	cfg.FloorSourceIP = "127.0.0.1"
	cfg.BroadcastIP = "127.0.0.1"
	cfg.BroadcastPort = receiver.LocalAddr().(*net.UDPAddr).Port
	status := newRuntimeStatus()
	output, err := newUDPOutput(cfg, status)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	if err := output.Write([]byte("floor-source")); err != nil {
		t.Fatal(err)
	}
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	count, source, err := receiver.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if string(buffer[:count]) != "floor-source" || !source.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("payload=%q source=%s", buffer[:count], source.IP)
	}
	if !status.sourceAssigned.Load() || !status.udpWriteAvailable.Load() {
		t.Fatal("successful exact-source write did not update status")
	}
}

func TestUDPOutputNeverFallsBackFromMissingSource(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FloorSourceIP = "192.0.2.55"
	cfg.BroadcastIP = "127.0.0.1"
	status := newRuntimeStatus()
	output, err := newUDPOutput(cfg, status)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	err = output.Write([]byte("must-not-fallback"))
	if !errors.Is(err, errFloorSourceUnavailable) {
		t.Fatalf("write error=%v, want source unavailable", err)
	}
	if status.sourceAssigned.Load() || status.udpWriteAvailable.Load() {
		t.Fatal("missing exact source was treated as available")
	}
}
