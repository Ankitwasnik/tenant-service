package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Ankitwasnik/tenant-service/internal/worker"
)

// Defaults (DESIGN.md §8).
const (
	defaultMinDelayMS = 500
	defaultMaxDelayMS = 2000
	defaultFailRate   = 0.0
)

// parseConfig reads the worker's settings. Every flag has a WORKER_* env var;
// the env var sets the default and the flag overrides it, so compose can set
// either. AMQP_URL is required.
func parseConfig(args []string, getenv func(string) string, output io.Writer) (worker.Config, string, error) {
	minDelay, err := envInt(getenv, "WORKER_MIN_DELAY_MS", defaultMinDelayMS)
	if err != nil {
		return worker.Config{}, "", err
	}
	maxDelay, err := envInt(getenv, "WORKER_MAX_DELAY_MS", defaultMaxDelayMS)
	if err != nil {
		return worker.Config{}, "", err
	}
	failRate, err := envFloat(getenv, "WORKER_FAIL_RATE", defaultFailRate)
	if err != nil {
		return worker.Config{}, "", err
	}

	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.IntVar(&minDelay, "min-delay-ms", minDelay, "lower bound of the random delay before each step, in ms (env WORKER_MIN_DELAY_MS)")
	fs.IntVar(&maxDelay, "max-delay-ms", maxDelay, "upper bound of the random delay, in ms (env WORKER_MAX_DELAY_MS)")
	fs.Float64Var(&failRate, "fail-rate", failRate, "probability 0-1 that a task fails (env WORKER_FAIL_RATE)")
	if err := fs.Parse(args); err != nil {
		return worker.Config{}, "", err
	}
	if fs.NArg() > 0 {
		return worker.Config{}, "", fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	cfg := worker.Config{
		MinDelay: time.Duration(minDelay) * time.Millisecond,
		MaxDelay: time.Duration(maxDelay) * time.Millisecond,
		FailRate: failRate,
	}
	if err := cfg.Validate(); err != nil {
		return worker.Config{}, "", err
	}

	amqpURL := getenv("AMQP_URL")
	if amqpURL == "" {
		return worker.Config{}, "", errors.New("missing required environment variable: AMQP_URL")
	}
	return cfg, amqpURL, nil
}

func envInt(getenv func(string) string, key string, def int) (int, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: not an integer", key, raw)
	}
	return v, nil
}

func envFloat(getenv func(string) string, key string, def float64) (float64, error) {
	raw := getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: not a number", key, raw)
	}
	return v, nil
}
