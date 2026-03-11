package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	llmURL := flag.String("llm-url", envOrDefault("RELAY_LLM_URL", "http://localhost:3001"), "relayLLM base URL")
	dataDir := flag.String("data-dir", envOrDefault("RELAY_SCHEDULER_DATA", ""), "Data directory for logs")
	port := flag.String("port", envOrDefault("RELAY_SCHEDULER_PORT", "3002"), "HTTP API listen port")
	pollInterval := flag.Duration("poll", 30*time.Second, "How often to poll relayLLM for project list")
	flag.Parse()

	if *dataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			dir, _ = os.UserHomeDir()
		}
		*dataDir = filepath.Join(dir, "relayScheduler")
	}
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		slog.Error("failed to create data directory", "path", *dataDir, "error", err)
		os.Exit(1)
	}

	slog.Info("starting relayScheduler", "llmURL", *llmURL, "dataDir", *dataDir)

	client := NewLLMClient(*llmURL)
	logStore := NewLogStore(filepath.Join(*dataDir, "task-logs"))
	scheduler := NewScheduler(client, logStore)

	// Initial load.
	if err := scheduler.LoadProjects(); err != nil {
		slog.Error("initial project load failed", "error", err)
	}

	// Periodic project poll (picks up new projects, path changes).
	go func() {
		ticker := time.NewTicker(*pollInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := scheduler.LoadProjects(); err != nil {
				slog.Error("project poll failed", "error", err)
			}
		}
	}()

	// HTTP API for task management.
	mux := http.NewServeMux()
	RegisterRoutes(mux, scheduler, logStore)

	serverErr := make(chan error, 1)
	go func() {
		addr := fmt.Sprintf(":%s", *port)
		slog.Info("API listening", "addr", addr)
		serverErr <- http.ListenAndServe(addr, mux)
	}()

	// Wait for shutdown or server failure.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		slog.Error("API server failed", "error", err)
	case <-sig:
	}

	slog.Info("shutting down")
	scheduler.Stop()
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
