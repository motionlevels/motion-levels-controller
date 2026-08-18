package adapter

import (
	"context"
	"time"
)

type debugPressureLeaseLoop struct {
	cfg      Config
	pressure *pressureStore
	hub      *engineHub
	status   *runtimeStatus
}

func (l *debugPressureLeaseLoop) run(ctx context.Context) error {
	interval := l.cfg.DebugPressLease / 4
	if interval > 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	if interval < 25*time.Millisecond {
		interval = 25 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if snapshot, changed := l.pressure.expireDebug(now); changed {
				l.status.markPressure(snapshot.Sequence)
				l.hub.publishPressure(snapshot)
			}
		}
	}
}
