package adapter

import (
	"context"
	"errors"
	"fmt"
	"log"
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

	notifier := newSystemdNotifier()
	defer notifier.close()

	engine := &engineServer{cfg: cfg, hub: hub}
	sensors := &sensorReader{cfg: cfg, pressure: pressure, hub: hub, status: status}
	output := &outputLoop{cfg: cfg, sender: sender, frames: frames, pressure: pressure, hub: hub, status: status, notifier: notifier}

	var wait sync.WaitGroup
	errCh := make(chan error, 3)
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
	start("HTTP server", func(ctx context.Context) error {
		return runHTTPServer(ctx, cfg, status, pressure, hub, frames)
	})
	wait.Add(1)
	go func() {
		defer wait.Done()
		status.metricsLoop(ctx.Done())
	}()

	notifier.ready()

	var runErr error
	select {
	case <-parent.Done():
		runErr = parent.Err()
	case runErr = <-errCh:
	}
	notifier.stopping()
	cancel()
	hub.close()
	wait.Wait()
	return runErr
}

func runHTTPServer(ctx context.Context, cfg Config, status *runtimeStatus, pressure *pressureStore, hub *engineHub, frames *frameStore) error {
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           newHTTPHandler(cfg, status, pressure, hub, frames),
		ReadHeaderTimeout: 2 * time.Second,
	}
	log.Printf("health and metrics: %s", cfg.HTTPAddr)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
