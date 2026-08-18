package adapter

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

var errFloorSourceUnavailable = errors.New("configured floor UDP source is unavailable")

type packetSender interface {
	Write([]byte) error
	Close() error
}

type udpOutput struct {
	cfg    Config
	status *runtimeStatus
	dest   *net.UDPAddr
	now    func() time.Time

	mu          sync.Mutex
	conn        *net.UDPConn
	nextResolve time.Time
	lastError   error
	known       bool
	available   bool
}

func newUDPOutput(cfg Config, status *runtimeStatus) (*udpOutput, error) {
	dest := &net.UDPAddr{IP: net.ParseIP(cfg.BroadcastIP).To4(), Port: cfg.BroadcastPort}
	if dest.IP == nil {
		return nil, fmt.Errorf("invalid floor broadcast IPv4 address %q", cfg.BroadcastIP)
	}
	output := &udpOutput{cfg: cfg, status: status, dest: dest, now: time.Now}
	if strings.TrimSpace(cfg.FloorSourceIP) == "" {
		status.sourceAssigned.Store(true)
	}
	return output, nil
}

func (o *udpOutput) Write(packet []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	conn, err := o.ensureConnLocked()
	if err != nil {
		o.markUnavailableLocked(err)
		return err
	}
	if err := conn.SetWriteDeadline(o.now().Add(o.cfg.WriteTimeout)); err != nil {
		_ = conn.Close()
		o.conn = nil
		o.nextResolve = time.Time{}
		o.lastError = fmt.Errorf("set floor UDP write deadline: %w", err)
		o.markUnavailableLocked(o.lastError)
		return o.lastError
	}
	if _, err := conn.WriteToUDP(packet, o.dest); err != nil {
		_ = conn.Close()
		o.conn = nil
		o.nextResolve = time.Time{}
		o.lastError = err
		o.markUnavailableLocked(err)
		return err
	}
	o.status.markUDPAvailable(o.now())
	o.markAvailableLocked()
	return nil
}

func (o *udpOutput) ensureConnLocked() (*net.UDPConn, error) {
	if o.conn != nil {
		return o.conn, nil
	}
	now := o.now()
	if now.Before(o.nextResolve) {
		return nil, o.lastError
	}

	var local *net.UDPAddr
	sourceValue := strings.TrimSpace(o.cfg.FloorSourceIP)
	if sourceValue != "" {
		source := net.ParseIP(sourceValue).To4()
		assigned, interfaceName, err := findActiveSource(source)
		if err != nil {
			o.status.sourceAssigned.Store(false)
			o.nextResolve = now.Add(o.cfg.SourceRetry)
			o.lastError = err
			return nil, err
		}
		if !assigned {
			err := fmt.Errorf("%w: %s is not assigned to an active interface", errFloorSourceUnavailable, sourceValue)
			o.status.sourceAssigned.Store(false)
			o.nextResolve = now.Add(o.cfg.SourceRetry)
			o.lastError = err
			return nil, err
		}
		local = &net.UDPAddr{IP: source, Port: 0}
		o.status.sourceAssigned.Store(true)
		log.Printf("floor UDP source acquired on %s", interfaceName)
	} else {
		local = &net.UDPAddr{IP: net.IPv4zero, Port: 0}
		o.status.sourceAssigned.Store(true)
	}

	conn, err := net.ListenUDP("udp4", local)
	if err != nil {
		o.nextResolve = now.Add(o.cfg.SourceRetry)
		o.lastError = fmt.Errorf("open floor UDP output: %w", err)
		return nil, o.lastError
	}
	if err := setBroadcast(conn); err != nil {
		_ = conn.Close()
		o.nextResolve = now.Add(o.cfg.SourceRetry)
		o.lastError = fmt.Errorf("enable floor UDP broadcast: %w", err)
		return nil, o.lastError
	}
	o.conn = conn
	o.nextResolve = time.Time{}
	o.lastError = nil
	return conn, nil
}

func (o *udpOutput) markAvailableLocked() {
	changed := !o.known || !o.available
	o.known = true
	o.available = true
	if changed {
		log.Printf("floor UDP writes available to %s", o.dest)
	}
}

func (o *udpOutput) markUnavailableLocked(reason error) {
	o.status.markUDPUnavailable()
	changed := !o.known || o.available
	o.known = true
	o.available = false
	if changed {
		log.Printf("floor UDP writes unavailable; retaining exact configuration and retrying: %v", reason)
	}
}

func (o *udpOutput) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conn == nil {
		return nil
	}
	err := o.conn.Close()
	o.conn = nil
	return err
}

func findActiveSource(source net.IP) (bool, string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false, "", fmt.Errorf("list interfaces: %w", err)
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
			if err == nil && addressIP.Equal(source) {
				return true, networkInterface.Name, nil
			}
		}
	}
	return false, "", nil
}

func setBroadcast(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}
