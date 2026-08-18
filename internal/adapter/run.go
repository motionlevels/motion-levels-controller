package adapter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

func Run(parent context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	status := newRuntimeStatus()
	frames := &frameStore{}
	pressure := &pressureStore{observedAt: time.Now()}
	hub := newEngineHub(cfg, frames, pressure, status)
	sender, err := newUDPOutput(cfg, status)
	if err != nil {
		return err
	}
	defer sender.Close()

	notifier, err := newSystemdNotifier()
	if err != nil {
		return err
	}
	defer notifier.close()

	readyCh := make(chan string, 4)
	readySignal := func(name string) func() {
		var once sync.Once
		return func() {
			once.Do(func() { readyCh <- name })
		}
	}

	engine := &engineServer{cfg: cfg, hub: hub, ready: readySignal("engine")}
	sensors := &sensorReader{cfg: cfg, pressure: pressure, hub: hub, status: status, ready: readySignal("sensors")}
	output := &outputLoop{
		cfg: cfg, sender: sender, frames: frames, pressure: pressure,
		hub: hub, status: status, notifier: notifier, ready: readySignal("output"),
	}

	var wait sync.WaitGroup
	errCh := make(chan error, 1)
	start := func(name string, run func(context.Context) error) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := run(ctx); err != nil && ctx.Err() == nil {
				select {
				case errCh <- fmt.Errorf("%s: %w", name, err):
				default:
				}
			}
		}()
	}

	start("engine server", engine.run)
	start("floor input", sensors.run)
	start("floor output", output.run)
	if cfg.EnableDebugControls {
		debugLeases := &debugPressureLeaseLoop{cfg: cfg, pressure: pressure, hub: hub, status: status}
		start("debug pressure leases", debugLeases.run)
	}
	start("HTTP server", func(ctx context.Context) error {
		return runHTTPServer(ctx, cfg, status, pressure, hub, frames, readySignal("http"))
	})
	wait.Add(1)
	go func() {
		defer wait.Done()
		status.metricsLoop(ctx.Done())
	}()

	readyCount := 0
	var runErr error
	for readyCount < 4 && runErr == nil {
		select {
		case <-readyCh:
			readyCount++
		case <-parent.Done():
			runErr = parent.Err()
		case runErr = <-errCh:
		}
	}
	readySent := readyCount == 4 && runErr == nil
	if readySent {
		notifier.ready()
		select {
		case <-parent.Done():
			runErr = parent.Err()
		case runErr = <-errCh:
		}
	}

	if readySent {
		notifier.stopping()
	}
	cancel()
	hub.close()
	wait.Wait()
	return runErr
}

func runHTTPServer(ctx context.Context, cfg Config, status *runtimeStatus, pressure *pressureStore, hub *engineHub, frames *frameStore, ready func()) error {
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           newHTTPHandler(cfg, status, pressure, hub, frames),
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	log.Printf("health, metrics and diagnostics: %s", listener.Addr())
	if ready != nil {
		ready()
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
