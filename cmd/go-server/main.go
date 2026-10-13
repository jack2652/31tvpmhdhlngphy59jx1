package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"option-scope-go/internal/config"
	"option-scope-go/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	log.Printf("runtime configuration: dotenv=%q status=%s host=%s port=%s port_source=%q database=%q", cfg.DotEnvPath, cfg.DotEnvStatus, cfg.Host, cfg.Port, cfg.PortSource, cfg.DatabasePath)
	// Configure GC before initializing providers, caches and SQLite. This
	// applies to direct execution, systemd and every watchdog restart.
	if cfg.GoMemoryLimitBytes >= 0 {
		debug.SetMemoryLimit(cfg.GoMemoryLimitBytes)
	}
	activeMemoryLimit := debug.SetMemoryLimit(-1)
	if activeMemoryLimit == math.MaxInt64 {
		log.Printf("Go GC memory budget: source=%s soft_limit=off effective_ram_limit=%d MiB startup_available=%d MiB", cfg.GoMemoryLimitSource, cfg.MemoryLimitBytes/(1<<20), cfg.MemoryAvailableBytes/(1<<20))
	} else {
		log.Printf("Go GC memory budget: source=%s soft_limit=%d MiB (%d bytes) effective_ram_limit=%d MiB startup_available=%d MiB", cfg.GoMemoryLimitSource, activeMemoryLimit/(1<<20), activeMemoryLimit, cfg.MemoryLimitBytes/(1<<20), cfg.MemoryAvailableBytes/(1<<20))
	}
	if cfg.GoMemoryLimitBytes < 0 {
		log.Print("automatic Go memory limit unavailable: RAM detection failed; preserving the runtime setting")
	} else if activeMemoryLimit != math.MaxInt64 && cfg.MemoryLimitBytes > 0 && activeMemoryLimit >= cfg.MemoryLimitBytes {
		log.Print("configured Go memory limit leaves no RAM headroom for binary mappings, SQLite and the operating system")
	}
	if cfg.MemoryLimitBytes > 0 {
		log.Printf("resource budget: memory_limit=%d MiB upstream_memory_budget=%d MiB low_memory_mode=%s low_memory_enabled=%t upstream_concurrency=%d", cfg.MemoryLimitBytes/(1024*1024), cfg.UpstreamMemoryBytes/(1024*1024), cfg.LowMemoryMode, cfg.LowMemory, cfg.UpstreamConcurrency)
	} else {
		log.Printf("resource budget: memory_limit=unknown upstream_memory_budget=%d MiB low_memory_mode=%s low_memory_enabled=%t upstream_concurrency=%d", cfg.UpstreamMemoryBytes/(1024*1024), cfg.LowMemoryMode, cfg.LowMemory, cfg.UpstreamConcurrency)
	}
	service, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("initialize service: %w", err)
	}
	defer service.Close()
	validationTimeout := cfg.UpstreamTimeout
	if validationTimeout <= 0 {
		validationTimeout = 12 * time.Second
	}
	validationCtx, cancelValidation := context.WithTimeout(context.Background(), validationTimeout)
	alpacaStatus := service.ValidateAlpacaCredentials(validationCtx)
	cancelValidation()
	switch alpacaStatus {
	case "disabled":
		fmt.Println("Alpaca overnight quotes disabled; using the regular market provider")
	case "invalid_config":
		fmt.Fprintln(os.Stderr, "Alpaca credentials are incomplete; using the regular market provider")
	case "invalid_credentials":
		fmt.Fprintln(os.Stderr, "Alpaca credentials are invalid; using the regular market provider and retrying after cooldown")
	case "unavailable":
		fmt.Fprintln(os.Stderr, "Alpaca credential check is unavailable; the service will retry on demand")
	default:
		fmt.Println("Alpaca overnight quote credentials validated")
	}
	address := net.JoinHostPort(cfg.Host, cfg.Port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	requests := &requestTracker{}
	httpServer := &http.Server{
		Addr:              address,
		Handler:           requests.wrap(service.Router()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		// SSE subscriptions are intentionally long-lived; upstream handlers still
		// enforce their own request timeouts.
		WriteTimeout:   0,
		IdleTimeout:    5 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			log.Printf("shutdown signal received; closing SSE streams and draining HTTP requests (press Ctrl+C again to force exit)")
			// Restore the default behavior so a second Ctrl+C/SIGTERM can force an
			// immediate exit if a provider or background worker is stuck.
			stop()
		case <-serveDone:
		}
	}()
	fmt.Printf("Option Scope Go server listening on http://%s\n", address)
	serveErr := serveHTTP(ctx, httpServer, listener, requests, 10*time.Second)
	close(serveDone)
	return serveErr
}

type requestTracker struct {
	mu           sync.Mutex
	draining     bool
	active       sync.WaitGroup
	nextStreamID uint64
	streams      map[uint64]context.CancelFunc
}

func (t *requestTracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		if t.draining {
			t.mu.Unlock()
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		t.active.Add(1)
		var streamID uint64
		var cancelStream context.CancelFunc
		isSSE := strings.HasPrefix(r.URL.Path, "/api/events/")
		if isSSE || strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
			streamCtx, cancel := context.WithCancel(r.Context())
			r = r.WithContext(streamCtx)
			cancelStream = cancel
			t.nextStreamID++
			streamID = t.nextStreamID
			if t.streams == nil {
				t.streams = make(map[uint64]context.CancelFunc)
			}
			t.streams[streamID] = cancel
		}
		t.mu.Unlock()
		defer func() {
			if cancelStream != nil {
				cancelStream()
				t.mu.Lock()
				delete(t.streams, streamID)
				t.mu.Unlock()
			}
			t.active.Done()
		}()
		next.ServeHTTP(w, r)
	})
}

func (t *requestTracker) stopAccepting() {
	t.mu.Lock()
	t.draining = true
	cancellations := make([]context.CancelFunc, 0, len(t.streams))
	for _, cancel := range t.streams {
		cancellations = append(cancellations, cancel)
	}
	t.mu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
}

func (t *requestTracker) wait() { t.active.Wait() }

func serveHTTP(ctx context.Context, httpServer *http.Server, listener net.Listener, requests *requestTracker, shutdownTimeout time.Duration) error {
	shutdownDone := make(chan struct{})
	serveDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
		case <-serveDone:
			return
		}
		requests.stopAccepting()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful HTTP shutdown timed out; closing active connections: %v", err)
			if closeErr := httpServer.Close(); closeErr != nil {
				log.Printf("force HTTP shutdown failed: %v", closeErr)
			}
		}
		requests.wait()
	}()

	serveErr := httpServer.Serve(listener)
	close(serveDone)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		requests.stopAccepting()
		closeErr := httpServer.Close()
		<-shutdownDone
		requests.wait()
		return errors.Join(serveErr, closeErr)
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
	return nil
}
