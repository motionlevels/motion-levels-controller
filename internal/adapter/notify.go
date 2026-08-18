package adapter

import (
	"net"
	"os"
)

type systemdNotifier struct {
	conn net.Conn
}

func newSystemdNotifier() *systemdNotifier {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return nil
	}
	addr, err := net.ResolveUnixAddr("unixgram", socketPath)
	if err != nil {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return nil
	}
	return &systemdNotifier{conn: conn}
}

func (n *systemdNotifier) notify(state string) {
	if n == nil || n.conn == nil {
		return
	}
	_, _ = n.conn.Write([]byte(state + "\n"))
}

func (n *systemdNotifier) ready() {
	n.notify("READY=1")
}

func (n *systemdNotifier) watchdog() {
	n.notify("WATCHDOG=1")
}

func (n *systemdNotifier) stopping() {
	n.notify("STOPPING=1")
}

func (n *systemdNotifier) close() {
	if n != nil && n.conn != nil {
		_ = n.conn.Close()
	}
}
