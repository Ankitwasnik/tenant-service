package main

import (
	"fmt"
	"strings"
)

// config is the control plane's runtime configuration. It comes from the
// environment only (DESIGN.md §10).
type config struct {
	DatabaseURL string
	AMQPURL     string
	HTTPAddr    string
}

const defaultHTTPAddr = ":8080"

// loadConfig reads the configuration through getenv (os.Getenv in production,
// a map lookup in tests). DATABASE_URL and AMQP_URL are required; HTTP_ADDR
// defaults to :8080.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		DatabaseURL: getenv("DATABASE_URL"),
		AMQPURL:     getenv("AMQP_URL"),
		HTTPAddr:    getenv("HTTP_ADDR"),
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultHTTPAddr
	}

	var missing []string
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.AMQPURL == "" {
		missing = append(missing, "AMQP_URL")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
