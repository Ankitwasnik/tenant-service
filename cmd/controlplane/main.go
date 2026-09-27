// Command controlplane runs the tenant provisioning control plane: the HTTP
// API, the outbox relay and the task-update consumer (DESIGN.md §2).
package main

import (
	"log/slog"
	"os"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// The URLs carry credentials, so only the listen address is logged.
	logger.Info("starting", "http_addr", cfg.HTTPAddr)
}
