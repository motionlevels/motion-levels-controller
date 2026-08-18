package adapter

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"
)

type systemdNotifier struct {
	conn             net.Conn
	watchdogInterval time.Duration
}

func newSystemdNotifier() (*systemdNotifier, error) {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return nil, nil
	}
	addr, err := net.ResolveUnixAddr("unixgram", socketPath)
	if err != nil {
		return nil, fmt.Errorf("resolve systemd notify socket: %w", err)
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("connect systemd notify socket: %w", err)
	}
	notifier := &systemdNotifier{conn: conn}
	if pidValue := os.Getenv("WATCHDOG_PID"); pidValue != "" {
		pid, err := strconv.Atoi(pidValue)
		if err != nil || pid != os.Getpid() {
			return notifier, nil
		}
	}
	if usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64); err == nil && usec > 0 {
		notifier.watchdogInterval = time.Duration(usec) * time.Microsecond / 2
		// Stay comfortably inside short watchdog deadlines. A 100 ms floor
		// would already miss a configured 100 ms watchdog.
		if notifier.watchdogInterval < 10*time.Millisecond {
			notifier.watchdogInterval = 10 * time.Millisecond
		}
	}
	return notifier, nil
}

func (n *systemdNotifier) notify(state string) {
	if n == nil || n.conn == nil {
		return
	}
	if _, err := n.conn.Write([]byte(state + "\n")); err != nil {
		log.Printf("systemd notify %q: %v", state, err)
	}
}

func (n *systemdNotifier) ready()    { n.notify("READY=1") }
func (n *systemdNotifier) watchdog() { n.notify("WATCHDOG=1") }
func (n *systemdNotifier) stopping() { n.notify("STOPPING=1") }

func (n *systemdNotifier) close() {
	if n != nil && n.conn != nil {
		_ = n.conn.Close()
	}
}
