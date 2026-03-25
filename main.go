package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/samber/lo"
)

func main() {
	// Initialize structured logging
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Configuration via environment variables
	shutdownDelay := getEnvDuration("SHUTDOWN_DELAY", 0)
	drainTimeout := getEnvDuration("DRAIN_TIMEOUT", 30*time.Second)
	errorRate := getEnvFloat("ERROR_RATE", 0.0)
	failureMode := getEnvString("FAILURE_MODE", "500") // 500, 503, 504, hang, reset, close, partial, slow-body, random, 5xx
	maxErrorDelay := getEnvInt("MAX_ERROR_DELAY_SECONDS", 5)
	enableBackgroundWorker := getEnvBool("ENABLE_BACKGROUND_WORKER", false)

	slog.Info("Service configuration",
		"shutdown_delay", shutdownDelay,
		"drain_timeout", drainTimeout,
		"error_rate", errorRate,
		"failure_mode", failureMode,
		"max_error_delay", maxErrorDelay,
		"enable_background_worker", enableBackgroundWorker,
	)

	// Create a context that is cancelled on SIGINT or SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var activeRequests int64

	// 1. Start the Background Worker if enabled
	if enableBackgroundWorker {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWorker(ctx)
		}()
	}

	// 2. Start the HTTP Server
	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", getEnvString("SERVER_PORT", "8080")),
		Handler: setupRoutes(&activeRequests, errorRate, failureMode, maxErrorDelay),
	}

	go func() {
		slog.Info("HTTP server starting", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for the termination signal
	<-ctx.Done()
	slog.Info("Termination signal received", "signal", "SIGTERM/SIGINT")

	// Optional delay before starting shutdown
	if shutdownDelay > 0 {
		slog.Info("Delaying shutdown start", "delay", shutdownDelay)
		time.Sleep(shutdownDelay)
	}

	// 3. Graceful Shutdown phase
	shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	slog.Info("Shutting down HTTP server...", "active_requests", atomic.LoadInt64(&activeRequests))
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown failed", "error", err)
	}

	slog.Info("Waiting for background worker to finish...")
	wg.Wait()

	slog.Info("Service gracefully stopped", "final_active_requests", atomic.LoadInt64(&activeRequests))
}

func setupRoutes(activeRequests *int64, errorRate float64, failureMode string, maxErrorDelay int) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	})

	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		statusStr := strings.TrimPrefix(r.URL.Path, "/status/")
		if statusStr == "" {
			http.Error(w, "Status code required", http.StatusBadRequest)
			return
		}

		status, err := strconv.Atoi(statusStr)
		if err != nil {
			http.Error(w, "Invalid status code", http.StatusBadRequest)
			return
		}

		w.WriteHeader(status)
		fmt.Fprintf(w, "Status: %d\n", status)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only handle exact root path
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		atomic.AddInt64(activeRequests, 1)
		defer atomic.AddInt64(activeRequests, -1)

		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.New().String()
		}

		delayStr := r.URL.Query().Get("delay")
		delay := 0
		if delayStr != "" {
			if d, err := strconv.Atoi(delayStr); err == nil && d > 0 {
				delay = d
			}
		} else if delayStr == "random" {
			delay = rand.Intn(10) + 1 // Random delay between 1 and 10 seconds
		}

		slog.Info("Starting request", "id", id, "method", r.Method, "path", r.URL.Path, "delay_sec", delay, "total_active", atomic.LoadInt64(activeRequests))

		// Simulate random failure
		if errorRate > 0 && rand.Float64() < errorRate {
			mode := failureMode
			if mode == "random" {
				modes := []string{"500", "503", "504", "hang", "reset", "close", "partial", "slow-body"}
				mode = lo.Sample(modes)
			} else if mode == "5xx" {
				// Randomly choose between 500, 503, and 504
				modes := []string{"500", "503", "504"}
				mode = lo.Sample(modes)
			} else if strings.Contains(mode, ",") {
				// Fallback: If it's a comma-separated list like "500,hang,close"
				modes := strings.Split(mode, ",")
				modes = lo.Map(modes, func(item string, _ int) string {
					return strings.TrimSpace(item)
				})
				mode = lo.Sample(modes)
			}

			slog.Warn("Simulating failure", "id", id, "mode", mode)

			switch mode {
			case "hang":
				slog.Info("Failure mode: hanging request", "id", id)
				select {
				case <-r.Context().Done():
					return
				}
			case "reset", "close":
				slog.Info("Failure mode: connection termination", "id", id, "type", mode)
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, err := hj.Hijack()
					if err == nil {
						if mode == "reset" {
							if tcpConn, ok := conn.(*net.TCPConn); ok {
								tcpConn.SetLinger(0)
							}
						}
						conn.Close()
						return
					}
				}
				http.Error(w, "Failure Simulation Failed", 500)
				return
			case "partial":
				slog.Info("Failure mode: sending partial response", "id", id)
				// Send headers and part of the body using chunked encoding, then close abruptly
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.WriteHeader(http.StatusOK)

				// Write a partial JSON object
				fmt.Fprintf(w, `{"id": "%s", "status": "incomplete", "data": "`, id)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}

				// Hijack to close abruptly after sending the first part
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, _ := hj.Hijack()
					conn.Close()
					slog.Info("Connection closed abruptly after partial response", "id", id)
				}
				return
			case "slow-body":
				slog.Info("Failure mode: slow-body transmission", "id", id)
				// Send body extremely slowly using Transfer-Encoding: chunked
				// Go's net/http automatically uses chunked encoding when Content-Length is not set and Flush is called.
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.WriteHeader(http.StatusOK)

				flusher, ok := w.(http.Flusher)
				if !ok {
					slog.Error("ResponseWriter does not support flushing", "id", id)
					return
				}

				for i := 1; i <= 10; i++ {
					fmt.Fprintf(w, "Chunk %d: Simulated slow data at %v\n", i, time.Now().Format(time.RFC3339))
					flusher.Flush()
					slog.Info("Sent chunk", "id", id, "chunk", i)
					time.Sleep(1 * time.Second)
				}
				return
			case "503":
				slog.Info("Failure mode: returning 503 Service Unavailable", "id", id)
				if maxErrorDelay > 0 {
					delay := time.Duration(rand.Intn(maxErrorDelay))
					slog.Info("Delaying 503 error", "id", id, "delay_in_seconds", delay)
					time.Sleep(delay)
				}
				http.Error(w, "Service Unavailable (Simulated)", http.StatusServiceUnavailable)
				return
			case "504":
				slog.Info("Failure mode: returning 504 Gateway Timeout", "id", id)
				if maxErrorDelay > 0 {
					delay := time.Duration(rand.Intn(maxErrorDelay))
					slog.Info("Delaying 504 error", "id", id, "delay_in_seconds", delay)
					time.Sleep(delay)
				}
				http.Error(w, "Gateway Timeout (Simulated)", http.StatusGatewayTimeout)
				return
			case "500":
				fallthrough
			default:
				slog.Info("Failure mode: returning 500 Internal Server Error", "id", id)
				http.Error(w, "Internal Server Error (Simulated)", http.StatusInternalServerError)
				return
			}
		}

		// Normal operation with delay
		select {
		case <-time.After(time.Duration(delay) * time.Second):
			slog.Info("Request completed", "id", id, "total_active", atomic.LoadInt64(activeRequests))

			response := struct {
				ID      string      `json:"id"`
				Method  string      `json:"method"`
				Status  string      `json:"status"`
				Headers http.Header `json:"headers"`
				Message string      `json:"message"`
			}{
				ID:      id,
				Method:  r.Method,
				Status:  "success",
				Headers: r.Header,
				Message: fmt.Sprintf("Echo response after %ds", delay),
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)

		case <-r.Context().Done():
			slog.Warn("Work request interrupted by client", "id", id, "total_active", atomic.LoadInt64(activeRequests))
		}
	})

	return mux
}

func runWorker(ctx context.Context) {
	slog.Info("Background worker started")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			slog.Info("Worker is processing periodic task...")
		case <-ctx.Done():
			slog.Info("Worker received shutdown signal, performing cleanup...")
			time.Sleep(250 * time.Millisecond)
			slog.Info("Worker cleanup finished")
			return
		}
	}
}

func getEnvInt(key string, defaultValue int) int {
	if val, ok := os.LookupEnv(key); ok {
		i, err := strconv.Atoi(val)
		if err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		d, err := time.ParseDuration(val)
		if err == nil {
			return d
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if val, ok := os.LookupEnv(key); ok {
		f, err := strconv.ParseFloat(val, 64)
		if err == nil {
			return f
		}
	}
	return defaultValue
}

func getEnvString(key string, defaultValue string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		b, err := strconv.ParseBool(val)
		if err == nil {
			return b
		}
	}
	return defaultValue
}
